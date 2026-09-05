package httpapi

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/gofiber/fiber/v3"
	"github.com/jackc/pgx/v5"

	"github.com/Venapce/venapce-api/internal/db"
	"github.com/Venapce/venapce-api/internal/osctrl"
)

const osctrlSettingKey = "osctrl"

// osctrlConfig is the persisted shape of the osctrl connection. The password is
// stored only as AES-GCM ciphertext (password_enc); it is never returned.
type osctrlConfig struct {
	URL         string `json:"url"`
	Username    string `json:"username"`
	Environment string `json:"environment"`
	PasswordEnc string `json:"password_enc"`
}

func (s *Server) loadOsctrlConfig(ctx context.Context) (*osctrlConfig, error) {
	row, err := s.q.GetSetting(ctx, osctrlSettingKey)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	var cfg osctrlConfig
	if err := json.Unmarshal(row.Value, &cfg); err != nil {
		return nil, err
	}
	if cfg.URL == "" {
		return nil, nil
	}
	return &cfg, nil
}

// GET /api/settings/osctrl — public-safe view (no secrets).
func (s *Server) getOsctrlSettings(c fiber.Ctx) error {
	cfg, err := s.loadOsctrlConfig(c.Context())
	if err != nil {
		return err
	}
	if cfg == nil {
		return c.JSON(fiber.Map{"configured": false})
	}
	return c.JSON(fiber.Map{
		"configured":  true,
		"url":         cfg.URL,
		"username":    cfg.Username,
		"environment": cfg.Environment,
	})
}

type putOsctrlBody struct {
	URL         string `json:"url"`
	Username    string `json:"username"`
	Password    string `json:"password"`
	Environment string `json:"environment"`
}

// PUT /api/settings/osctrl — save connection, encrypt the password, rebuild the
// live client, and report whether a login succeeds.
func (s *Server) putOsctrlSettings(c fiber.Ctx) error {
	var body putOsctrlBody
	if err := c.Bind().Body(&body); err != nil {
		return fiber.NewError(fiber.StatusBadRequest, "invalid body")
	}
	if body.URL == "" || body.Username == "" {
		return fiber.NewError(fiber.StatusBadRequest, "url and username are required")
	}

	// Keep the existing password when the field is left blank on an update.
	existing, err := s.loadOsctrlConfig(c.Context())
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

	cfg := osctrlConfig{URL: body.URL, Username: body.Username, Environment: body.Environment, PasswordEnc: passEnc}
	value, err := json.Marshal(cfg)
	if err != nil {
		return err
	}
	if _, err := s.q.UpsertSetting(c.Context(), db.UpsertSettingParams{
		Key:   osctrlSettingKey,
		Value: value,
	}); err != nil {
		return err
	}

	// Rebuild the live client and probe the connection.
	pass, err := s.box.Decrypt(passEnc)
	if err != nil {
		return err
	}
	client := osctrl.NewClient(cfg.URL, cfg.Username, pass, cfg.Environment)
	s.osc.Set(client)

	resp := fiber.Map{"configured": true, "url": cfg.URL, "username": cfg.Username, "environment": cfg.Environment}
	if who, err := client.TestLogin(c.Context()); err != nil {
		resp["connected"] = false
		resp["connectionError"] = err.Error()
	} else {
		resp["connected"] = true
		resp["connectedAs"] = who
	}
	return c.JSON(resp)
}

// POST /api/settings/osctrl/test — probe the currently stored connection.
func (s *Server) testOsctrlSettings(c fiber.Ctx) error {
	client := s.osc.Get()
	if client == nil {
		return fiber.NewError(fiber.StatusBadRequest, "osctrl is not configured")
	}
	who, err := client.TestLogin(c.Context())
	if err != nil {
		return c.JSON(fiber.Map{"connected": false, "connectionError": err.Error()})
	}
	return c.JSON(fiber.Map{"connected": true, "connectedAs": who})
}

// osctrlClient resolves the live client or fails with a clear 400 that steers the
// operator to Settings.
func (s *Server) osctrlClient() (*osctrl.Client, error) {
	cl := s.osc.Get()
	if cl == nil {
		return nil, fiber.NewError(fiber.StatusBadRequest, "osctrl is not configured — set it in Settings")
	}
	return cl, nil
}

// envParam returns the requested environment, falling back to the configured
// default when the query omits it.
func (s *Server) envParam(c fiber.Ctx, cl *osctrl.Client) (string, error) {
	env := c.Query("env")
	if env == "" {
		env = cl.Environment()
	}
	if env == "" {
		return "", fiber.NewError(fiber.StatusBadRequest, "env is required")
	}
	return env, nil
}

// GET /api/osctrl/environments
func (s *Server) osctrlEnvironments(c fiber.Ctx) error {
	cl, err := s.osctrlClient()
	if err != nil {
		return err
	}
	raw, err := cl.Environments(c.Context())
	if err != nil {
		return fiber.NewError(fiber.StatusBadGateway, err.Error())
	}
	return writeRaw(c, raw)
}

// GET /api/osctrl/nodes?env=
func (s *Server) osctrlNodes(c fiber.Ctx) error {
	cl, err := s.osctrlClient()
	if err != nil {
		return err
	}
	env, err := s.envParam(c, cl)
	if err != nil {
		return err
	}
	raw, err := cl.Nodes(c.Context(), env)
	if err != nil {
		return fiber.NewError(fiber.StatusBadGateway, err.Error())
	}
	return writeRaw(c, raw)
}

// GET /api/osctrl/enroll?env=&target=
func (s *Server) osctrlEnroll(c fiber.Ctx) error {
	cl, err := s.osctrlClient()
	if err != nil {
		return err
	}
	env, err := s.envParam(c, cl)
	if err != nil {
		return err
	}
	values, err := cl.Enroll(c.Context(), env)
	if err != nil {
		return fiber.NewError(fiber.StatusBadGateway, err.Error())
	}
	return c.JSON(values)
}

// osctrlActions is the set of enroll/remove lifecycle actions osctrl accepts.
var osctrlActions = map[string]bool{"extend": true, "expire": true, "rotate": true, "notexpire": true}

type osctrlActionBody struct {
	Env    string `json:"env"`
	Target string `json:"target"` // "enroll" or "remove"
	Action string `json:"action"` // extend | expire | rotate | notexpire (osctrl uses a fixed extend period)
}

// POST /api/osctrl/enroll/actions — mutate an environment's enroll or remove link
// (rotate/extend/expire/notexpire) and return the refreshed enroll values so the
// page reflects the new state. The JWT is injected in the backend; osctrl is
// always the source of truth (no caching).
func (s *Server) osctrlEnrollAction(c fiber.Ctx) error {
	cl, err := s.osctrlClient()
	if err != nil {
		return err
	}
	var body osctrlActionBody
	if err := c.Bind().Body(&body); err != nil {
		return fiber.NewError(fiber.StatusBadRequest, "invalid body")
	}
	env := body.Env
	if env == "" {
		env = cl.Environment()
	}
	if env == "" {
		return fiber.NewError(fiber.StatusBadRequest, "env is required")
	}
	if !osctrlActions[body.Action] {
		return fiber.NewError(fiber.StatusBadRequest, "invalid action — one of: extend, expire, rotate, notexpire")
	}

	switch body.Target {
	case "enroll", "":
		_, err = cl.EnrollAction(c.Context(), env, body.Action)
	case "remove":
		_, err = cl.RemoveAction(c.Context(), env, body.Action)
	default:
		return fiber.NewError(fiber.StatusBadRequest, "invalid target — enroll or remove")
	}
	if err != nil {
		return fiber.NewError(fiber.StatusBadGateway, err.Error())
	}

	// Re-fetch so the response carries the post-action enroll values.
	values, err := cl.Enroll(c.Context(), env)
	if err != nil {
		return fiber.NewError(fiber.StatusBadGateway, err.Error())
	}
	return c.JSON(values)
}
