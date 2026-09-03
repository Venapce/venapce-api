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

	cfg := supersetConfig{URL: body.URL, Username: body.Username, PasswordEnc: passEnc}
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

	resp := fiber.Map{"configured": true, "url": cfg.URL, "username": cfg.Username}
	if who, err := client.TestLogin(c.Context()); err != nil {
		resp["connected"] = false
		resp["connectionError"] = err.Error()
	} else {
		resp["connected"] = true
		resp["connectedAs"] = who
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
