package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"strings"

	"github.com/gofiber/fiber/v3"
	"github.com/jackc/pgx/v5"

	"github.com/Venapce/venapce-api/internal/db"
	"github.com/Venapce/venapce-api/internal/flomorphic"
)

// How venapce reaches FloMorphic — the API base, the shared signing secret and the
// infra host — is seeded from the deploy environment, but an operator who installed
// with FloMorphic skipped (or whose FloMorphic moved) must be able to fix it from
// the panel: the alternative is editing .env and recreating the container, and a
// plain `docker compose restart` silently keeps the old environment.
//
// So the values live in the settings table as an override. Env is the default;
// anything stored here wins, per field, and survives a container recreate (the DB
// is on the state volume). Reset clears the override and falls back to env.
const flomorphicAPISettingKey = "flomorphic_api"

// flomorphicAPIConfig is the persisted override. The signing secret is stored only
// as AES-GCM ciphertext and is never returned to the client.
type flomorphicAPIConfig struct {
	URL          string `json:"url,omitempty"`
	JWTSecretEnc string `json:"jwt_secret_enc,omitempty"`
	InfraHost    string `json:"infra_host,omitempty"`
}

// set reports whether the row carries anything at all — a cleared override is
// stored as an empty object rather than deleted, so this is the "is overridden"
// test everywhere.
func (c flomorphicAPIConfig) set() bool {
	return c.URL != "" || c.JWTSecretEnc != "" || c.InfraHost != ""
}

func (s *Server) loadFlomorphicAPIConfig(ctx context.Context) (flomorphicAPIConfig, error) {
	row, err := s.q.GetSetting(ctx, flomorphicAPISettingKey)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return flomorphicAPIConfig{}, nil
		}
		return flomorphicAPIConfig{}, err
	}
	var cfg flomorphicAPIConfig
	if err := json.Unmarshal(row.Value, &cfg); err != nil {
		return flomorphicAPIConfig{}, err
	}
	return cfg, nil
}

// effectiveFlomorphicAccess resolves the stored override against the environment,
// field by field: a stored value wins, an empty one falls back to env. Returns the
// access and whether any field came from the override.
func (s *Server) effectiveFlomorphicAccess(ctx context.Context) (flomorphic.Access, bool, error) {
	stored, err := s.loadFlomorphicAPIConfig(ctx)
	if err != nil {
		return flomorphic.Access{}, false, err
	}
	access := flomorphic.Access{
		URL:       s.cfg.FlomorphicURL,
		JWTSecret: s.cfg.FlomorphicJWTSecret,
		InfraHost: s.cfg.InfraHost,
	}
	if stored.URL != "" {
		access.URL = stored.URL
	}
	if stored.InfraHost != "" {
		access.InfraHost = stored.InfraHost
	}
	if stored.JWTSecretEnc != "" {
		secret, err := s.box.Decrypt(stored.JWTSecretEnc)
		if err != nil {
			// A secret encrypted under a rotated APP_SECRET_KEY is unreadable; fall
			// back to env rather than leaving FloMorphic unreachable with no way to
			// say why.
			return access, stored.set(), err
		}
		access.JWTSecret = secret
	}
	return access, stored.set(), nil
}

// LoadFlomorphicFromDB applies the stored override to the live client at boot.
// Called from main after the pool is up; a missing override leaves the environment's
// access in place.
func (s *Server) LoadFlomorphicFromDB(ctx context.Context) error {
	access, _, err := s.effectiveFlomorphicAccess(ctx)
	if err != nil {
		return err
	}
	s.flo.Set(access)
	return nil
}

// flomorphicAccessView is the public-safe description of the live access: the URL
// and infra host are shown, the secret only as a boolean, plus where each came from
// so the card can say "from the environment" vs "set here".
func (s *Server) flomorphicAccessView(ctx context.Context) (fiber.Map, error) {
	stored, err := s.loadFlomorphicAPIConfig(ctx)
	if err != nil {
		return nil, err
	}
	access := s.flo.Access()
	return fiber.Map{
		"apiConfigured": s.flo.Get() != nil,
		"apiUrl":        access.URL,
		"jwtSecretSet":  access.JWTSecret != "",
		"infraHost":     access.InfraHost,
		// True while nothing is stored: the values are the deploy environment's and
		// change only by editing .env + recreating the container.
		"apiFromEnv": !stored.set(),
	}, nil
}

type putFlomorphicAPIBody struct {
	URL string `json:"url"`
	// Blank on an update keeps the stored secret; blank with nothing stored falls
	// back to the environment's.
	JWTSecret string `json:"jwtSecret"`
	InfraHost string `json:"infraHost"`
}

// PUT /api/settings/flomorphic/api — save where FloMorphic is, rebuild the live
// client and probe it. This is the runtime equivalent of the installer's prompts:
// it does not register the plugin (that is the PUT on the parent route), it only
// makes registration possible.
func (s *Server) putFlomorphicAPISettings(c fiber.Ctx) error {
	var body putFlomorphicAPIBody
	if err := c.Bind().Body(&body); err != nil {
		return fiber.NewError(fiber.StatusBadRequest, "invalid body")
	}
	url := strings.TrimRight(strings.TrimSpace(body.URL), "/")
	if url == "" {
		return fiber.NewError(fiber.StatusBadRequest, "url is required")
	}

	stored, err := s.loadFlomorphicAPIConfig(c.Context())
	if err != nil {
		return err
	}
	secretEnc := stored.JWTSecretEnc
	if secret := strings.TrimSpace(body.JWTSecret); secret != "" {
		if secretEnc, err = s.box.Encrypt(secret); err != nil {
			return err
		}
	}

	cfg := flomorphicAPIConfig{
		URL:          url,
		JWTSecretEnc: secretEnc,
		InfraHost:    strings.TrimSpace(body.InfraHost),
	}
	value, err := json.Marshal(cfg)
	if err != nil {
		return err
	}
	if _, err := s.q.UpsertSetting(c.Context(), db.UpsertSettingParams{
		Key:   flomorphicAPISettingKey,
		Value: value,
	}); err != nil {
		return err
	}

	access, _, err := s.effectiveFlomorphicAccess(c.Context())
	if err != nil {
		return err
	}
	s.flo.Set(access)

	view, err := s.flomorphicAccessView(c.Context())
	if err != nil {
		return err
	}
	addReachability(c.Context(), view, s.flo.Get())
	return c.JSON(view)
}

// POST /api/settings/flomorphic/api/test — probe the given access (or the live one
// when the body is empty) without saving it, so the operator can find a reachable
// URL before committing to it.
func (s *Server) testFlomorphicAPISettings(c fiber.Ctx) error {
	var body putFlomorphicAPIBody
	// An empty body is valid here and means "test what is configured".
	_ = c.Bind().Body(&body)

	access := s.flo.Access()
	if url := strings.TrimSpace(body.URL); url != "" {
		access.URL = strings.TrimRight(url, "/")
	}
	if secret := strings.TrimSpace(body.JWTSecret); secret != "" {
		access.JWTSecret = secret
	}
	client := flomorphic.NewClient(access.URL, access.JWTSecret)
	if client == nil {
		return fiber.NewError(fiber.StatusBadRequest, "a FloMorphic URL and JWT secret are both required to test the connection")
	}
	resp := fiber.Map{"apiUrl": access.URL}
	addReachability(c.Context(), resp, client)
	return c.JSON(resp)
}

// POST /api/settings/flomorphic/api/reset — drop the override and go back to the
// deploy environment's values. The row is emptied rather than deleted so the
// settings table keeps a single shape for every key.
func (s *Server) resetFlomorphicAPISettings(c fiber.Ctx) error {
	value, err := json.Marshal(flomorphicAPIConfig{})
	if err != nil {
		return err
	}
	if _, err := s.q.UpsertSetting(c.Context(), db.UpsertSettingParams{
		Key:   flomorphicAPISettingKey,
		Value: value,
	}); err != nil {
		return err
	}
	access, _, err := s.effectiveFlomorphicAccess(c.Context())
	if err != nil {
		return err
	}
	s.flo.Set(access)

	view, err := s.flomorphicAccessView(c.Context())
	if err != nil {
		return err
	}
	addReachability(c.Context(), view, s.flo.Get())
	return c.JSON(view)
}

// addReachability annotates a response with whether FloMorphic actually answers on
// the given client. A nil client (incomplete access) is reported as unreachable
// rather than an error — the card shows it as "not configured".
func addReachability(ctx context.Context, m fiber.Map, client *flomorphic.Client) {
	if client == nil {
		m["reachable"] = false
		return
	}
	if err := client.Ping(ctx); err != nil {
		m["reachable"] = false
		m["reachError"] = err.Error()
		return
	}
	m["reachable"] = true
}
