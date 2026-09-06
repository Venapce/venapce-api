package httpapi

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/gofiber/fiber/v3"
	"github.com/jackc/pgx/v5"

	"github.com/Venapce/venapce-api/internal/db"
	"github.com/Venapce/venapce-api/internal/superset"
)

const supersetSettingKey = "superset"

// supersetConfig is the persisted shape of the Superset connection. The password
// is stored only as AES-GCM ciphertext (password_enc); it is never returned to
// the client.
type supersetConfig struct {
	URL         string `json:"url"`
	Username    string `json:"username"`
	PasswordEnc string `json:"password_enc"`
	// Managed is true when this connection is owned by the deploy environment
	// (auto-configured from SUPERSET_URL/SUPERSET_ADMIN_* on boot). A managed
	// connection is refreshed from env on every boot and shown read-only in
	// Settings. It flips to false the moment an operator overrides it with an
	// external Superset, after which env no longer touches it.
	Managed bool `json:"managed"`
}

func (s *Server) loadSupersetConfig(ctx context.Context) (*supersetConfig, error) {
	row, err := s.q.GetSetting(ctx, supersetSettingKey)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	var cfg supersetConfig
	if err := json.Unmarshal(row.Value, &cfg); err != nil {
		return nil, err
	}
	if cfg.URL == "" {
		return nil, nil
	}
	return &cfg, nil
}

// GET /api/settings/superset — public-safe view (no secrets).
func (s *Server) getSupersetSettings(c fiber.Ctx) error {
	cfg, err := s.loadSupersetConfig(c.Context())
	if err != nil {
		return err
	}
	if cfg == nil {
		return c.JSON(fiber.Map{"configured": false})
	}
	return c.JSON(fiber.Map{
		"configured": true,
		"url":        cfg.URL,
		"username":   cfg.Username,
		"managed":    cfg.Managed,
	})
}

type putSupersetBody struct {
	URL      string `json:"url"`
	Username string `json:"username"`
	Password string `json:"password"`
}

// PUT /api/settings/superset — save connection, encrypt the password, rebuild the
// live client, and report whether a login succeeds.
func (s *Server) putSupersetSettings(c fiber.Ctx) error {
	var body putSupersetBody
	if err := c.Bind().Body(&body); err != nil {
		return fiber.NewError(fiber.StatusBadRequest, "invalid body")
	}
	if body.URL == "" || body.Username == "" {
		return fiber.NewError(fiber.StatusBadRequest, "url and username are required")
	}

	// Keep the existing password when the field is left blank on an update.
	existing, err := s.loadSupersetConfig(c.Context())
	if err != nil {
		return err
	}
	passEnc := ""
	switch {
	case body.Password != "":
		passEnc, err = s.box.Encrypt(body.Password)
		if err != nil {
			return err
		}
	case existing != nil:
		passEnc = existing.PasswordEnc
	default:
		return fiber.NewError(fiber.StatusBadRequest, "password is required")
	}

	// Saving through the form is an explicit operator override — this connection
	// is now theirs to manage, so env-managed bootstrapping no longer touches it.
	cfg := supersetConfig{URL: body.URL, Username: body.Username, PasswordEnc: passEnc, Managed: false}
	value, err := json.Marshal(cfg)
	if err != nil {
		return err
	}
	if _, err := s.q.UpsertSetting(c.Context(), db.UpsertSettingParams{
		Key:   supersetSettingKey,
		Value: value,
	}); err != nil {
		return err
	}

	// Rebuild the live client and probe the connection.
	pass, err := s.box.Decrypt(passEnc)
	if err != nil {
		return err
	}
	client := superset.NewClient(cfg.URL, cfg.Username, pass)
	s.sup.Set(client)

	resp := fiber.Map{"configured": true, "url": cfg.URL, "username": cfg.Username, "managed": false}
	if who, err := client.TestLogin(c.Context()); err != nil {
		resp["connected"] = false
		resp["connectionError"] = err.Error()
	} else {
		resp["connected"] = true
		resp["connectedAs"] = who
		// Now that we have a working connection, (re)wire the database + datasets
		// in the background. Idempotent, so re-saving settings is safe.
		s.StartSupersetProvisioning()
	}
	return c.JSON(resp)
}

// bootstrapSupersetFromEnv configures the Superset connection from the deploy
// environment (SUPERSET_URL/SUPERSET_ADMIN_USER/SUPERSET_ADMIN_PASS), so the
// packaged product needs no manual Settings step. It (re)writes the stored
// connection as *managed* and rebuilds the live client. It is a no-op when the
// env is incomplete, or when the operator has overridden Superset with an
// external instance (stored Managed==false). Safe to call on every boot.
func (s *Server) bootstrapSupersetFromEnv(ctx context.Context) (bool, error) {
	if !s.cfg.SupersetManaged() {
		return false, nil
	}
	existing, err := s.loadSupersetConfig(ctx)
	if err != nil {
		return false, err
	}
	if existing != nil && !existing.Managed {
		// Operator points Venapce at their own Superset — leave it be.
		return false, nil
	}

	passEnc, err := s.box.Encrypt(s.cfg.SupersetAdminPass)
	if err != nil {
		return false, err
	}
	cfg := supersetConfig{
		URL:         s.cfg.SupersetURL,
		Username:    s.cfg.SupersetAdminUser,
		PasswordEnc: passEnc,
		Managed:     true,
	}
	value, err := json.Marshal(cfg)
	if err != nil {
		return false, err
	}
	if _, err := s.q.UpsertSetting(ctx, db.UpsertSettingParams{Key: supersetSettingKey, Value: value}); err != nil {
		return false, err
	}
	s.sup.Set(superset.NewClient(cfg.URL, cfg.Username, s.cfg.SupersetAdminPass))
	return true, nil
}

// BootstrapSupersetFromEnv is the boot-time entry point (called from main after
// LoadSupersetFromDB). It never blocks boot on a failure — the manual Settings
// path always remains available.
func (s *Server) BootstrapSupersetFromEnv(ctx context.Context) error {
	_, err := s.bootstrapSupersetFromEnv(ctx)
	return err
}

// POST /api/settings/superset/reset — revert to the built-in (env-managed)
// Superset, discarding an external override. Only meaningful when the deploy
// environment provides the built-in connection.
func (s *Server) resetSupersetSettings(c fiber.Ctx) error {
	if !s.cfg.SupersetManaged() {
		return fiber.NewError(fiber.StatusBadRequest, "no built-in Superset is configured in this environment")
	}
	// Force a re-bootstrap even over an external override by clearing the
	// managed flag guard: write the managed connection directly.
	existing, err := s.loadSupersetConfig(c.Context())
	if err != nil {
		return err
	}
	if existing != nil {
		existing.Managed = true // allow bootstrap to take over
		value, _ := json.Marshal(existing)
		_, _ = s.q.UpsertSetting(c.Context(), db.UpsertSettingParams{Key: supersetSettingKey, Value: value})
	}
	if _, err := s.bootstrapSupersetFromEnv(c.Context()); err != nil {
		return err
	}
	cfg, err := s.loadSupersetConfig(c.Context())
	if err != nil {
		return err
	}
	resp := fiber.Map{"configured": true, "url": cfg.URL, "username": cfg.Username, "managed": true}
	if client := s.sup.Get(); client != nil {
		if who, err := client.TestLogin(c.Context()); err != nil {
			resp["connected"] = false
			resp["connectionError"] = err.Error()
		} else {
			resp["connected"] = true
			resp["connectedAs"] = who
			s.StartSupersetProvisioning()
		}
	}
	return c.JSON(resp)
}

// POST /api/settings/superset/test — probe the currently stored connection.
func (s *Server) testSupersetSettings(c fiber.Ctx) error {
	client := s.sup.Get()
	if client == nil {
		return fiber.NewError(fiber.StatusBadRequest, "Superset is not configured")
	}
	who, err := client.TestLogin(c.Context())
	if err != nil {
		return c.JSON(fiber.Map{"connected": false, "connectionError": err.Error()})
	}
	return c.JSON(fiber.Map{"connected": true, "connectedAs": who})
}

// GET /api/settings/superset/examples/status — on-demand example-data load state
// (idle | running | loaded | failed), proxied from the Superset control blueprint.
func (s *Server) getExamplesStatus(c fiber.Ctx) error {
	client := s.sup.Get()
	if client == nil {
		return fiber.NewError(fiber.StatusBadRequest, "Superset is not configured")
	}
	status, body, err := client.ExamplesStatus(c.Context())
	if err != nil {
		return err
	}
	return passThroughJSON(c, status, body)
}

// POST /api/settings/superset/examples — trigger a background load of Superset's
// example datasets (handy for a demo). Idempotent; see the control blueprint.
func (s *Server) loadExamples(c fiber.Ctx) error {
	client := s.sup.Get()
	if client == nil {
		return fiber.NewError(fiber.StatusBadRequest, "Superset is not configured")
	}
	status, body, err := client.LoadExamples(c.Context())
	if err != nil {
		return err
	}
	return passThroughJSON(c, status, body)
}

// passThroughJSON relays the control blueprint's JSON body to the front. The
// blueprint encodes the outcome in the JSON `state` field, so its 202 (started)
// and 409 (already running) are normal and collapse to 200 — the front keys off
// `state`. Real errors (401/403/5xx) pass through so apiErr surfaces them. A
// missing/opaque body (e.g. an nginx error page) becomes a 502.
func passThroughJSON(c fiber.Ctx, status int, body []byte) error {
	if len(body) == 0 || body[0] != '{' {
		return fiber.NewError(fiber.StatusBadGateway, "unexpected response from Superset control endpoint")
	}
	if status == fiber.StatusAccepted || status == fiber.StatusConflict {
		status = fiber.StatusOK
	}
	c.Set("Content-Type", "application/json")
	return c.Status(status).Send(body)
}
