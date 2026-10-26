package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"strings"

	"github.com/gofiber/fiber/v3"
	"github.com/jackc/pgx/v5"

	"github.com/Venapce/venapce-api/internal/db"
)

// Where the FloMorphic *web app* answers in a browser — the other half of "where
// FloMorphic is", and the half venapce never calls.
//
// It is kept apart from the API access (flomorphic_access) because the two are
// different addresses for different clients, and every service here runs in a
// container:
//
//   - the API base is reached from inside this container, so it is FloMorphic's
//     container name on the compose network (http://flomorphic:8025);
//   - the web app is reached from the operator's browser, so it is the port the
//     FloMorphic compose publishes on the Docker host (http://localhost:8088) —
//     or, past a reverse proxy, a host name that exists nowhere in this network.
//
// Deriving one from the other is therefore wrong in both directions, which is why
// this is its own stored setting with its own card in Settings. Env
// (FLOMORPHIC_PANEL_URL, defaulting to the standard compose's published port) is
// the default; anything saved here wins and survives a container recreate,
// because the settings table lives on the state volume while .env does not take
// effect until the container is recreated.
const flomorphicPanelSettingKey = "flomorphic_panel"

// flomorphicPanelConfig is the persisted override. Nothing in it is a secret: it
// is an address the operator's browser already knows.
type flomorphicPanelConfig struct {
	URL string `json:"url,omitempty"`
}

func (s *Server) loadFlomorphicPanelConfig(ctx context.Context) (flomorphicPanelConfig, error) {
	row, err := s.q.GetSetting(ctx, flomorphicPanelSettingKey)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return flomorphicPanelConfig{}, nil
		}
		return flomorphicPanelConfig{}, err
	}
	var cfg flomorphicPanelConfig
	if err := json.Unmarshal(row.Value, &cfg); err != nil {
		return flomorphicPanelConfig{}, err
	}
	return cfg, nil
}

// effectiveFlomorphicPanel resolves the web app address: this setting first, then
// the panel URL older installs saved inside the API override (it used to be a
// field on that card), then the environment. Returns the address and whether it
// came from the environment, so the card can label it and offer "reset".
//
// Takes the API override the caller has already loaded rather than re-reading it,
// since the only caller resolving both is effectiveFlomorphicAccess.
func (s *Server) effectiveFlomorphicPanel(ctx context.Context, apiCfg flomorphicAPIConfig) (string, bool, error) {
	stored, err := s.loadFlomorphicPanelConfig(ctx)
	if err != nil {
		return s.cfg.FlomorphicPanelURL, true, err
	}
	if stored.URL != "" {
		return stored.URL, false, nil
	}
	if apiCfg.PanelURL != "" {
		return apiCfg.PanelURL, false, nil
	}
	return s.cfg.FlomorphicPanelURL, true, nil
}

type putFlomorphicPanelBody struct {
	URL string `json:"url"`
}

// errPanelURLRequired / errPanelURLScheme are the two ways a submitted address is
// rejected, named so the handler and its test agree on them.
var (
	errPanelURLRequired = errors.New("url is required — use the reset route to go back to the environment's address")
	errPanelURLScheme   = errors.New("url must start with http:// or https://")
)

// normalizePanelURL trims a submitted web app address to the form the links are
// built from: scheme + host + port, no trailing slash.
//
// The scheme is required rather than guessed. This address is handed to a browser
// verbatim, and a bare "localhost:8088" or "flomorphic:8025" — both of which an
// operator types by reflex — is not a URL a browser resolves the way they mean.
// Failing here is the only moment anyone finds out; the alternative is a link
// that opens a dead tab weeks later, while reading a failed run.
func normalizePanelURL(raw string) (string, error) {
	url := strings.TrimRight(strings.TrimSpace(raw), "/")
	if url == "" {
		return "", errPanelURLRequired
	}
	lower := strings.ToLower(url)
	if !strings.HasPrefix(lower, "http://") && !strings.HasPrefix(lower, "https://") {
		return "", errPanelURLScheme
	}
	return url, nil
}

// PUT /api/settings/flomorphic/panel — save where the FloMorphic web app answers
// in a browser. Applies immediately: it only moves the target of links venapce's
// own pages render, so there is no client to rebuild and nothing to probe (this
// backend cannot reach that address and must not try — failing from inside the
// container says nothing about whether the browser can reach it).
func (s *Server) putFlomorphicPanelSettings(c fiber.Ctx) error {
	var body putFlomorphicPanelBody
	if err := c.Bind().Body(&body); err != nil {
		return fiber.NewError(fiber.StatusBadRequest, "invalid body")
	}
	url, err := normalizePanelURL(body.URL)
	if err != nil {
		return fiber.NewError(fiber.StatusBadRequest, err.Error())
	}

	if err := s.storeFlomorphicPanel(c.Context(), flomorphicPanelConfig{URL: url}); err != nil {
		return err
	}
	return s.flomorphicPanelResponse(c)
}

// POST /api/settings/flomorphic/panel/reset — drop the override, so the deploy
// environment's address applies again. The row is emptied rather than deleted, to
// keep one shape for every settings key.
func (s *Server) resetFlomorphicPanelSettings(c fiber.Ctx) error {
	if err := s.storeFlomorphicPanel(c.Context(), flomorphicPanelConfig{}); err != nil {
		return err
	}
	return s.flomorphicPanelResponse(c)
}

// storeFlomorphicPanel persists the override and re-applies the live access, so
// the links the backend itself hands out (the operation settings plans) move in
// the same request.
func (s *Server) storeFlomorphicPanel(ctx context.Context, cfg flomorphicPanelConfig) error {
	value, err := json.Marshal(cfg)
	if err != nil {
		return err
	}
	if _, err := s.q.UpsertSetting(ctx, db.UpsertSettingParams{
		Key:   flomorphicPanelSettingKey,
		Value: value,
	}); err != nil {
		return err
	}
	access, _, err := s.effectiveFlomorphicAccess(ctx)
	if err != nil {
		return err
	}
	s.flo.Set(access)
	return nil
}

// flomorphicPanelResponse answers the panel routes with the same access view the
// card already reads, so one response refreshes the whole card.
func (s *Server) flomorphicPanelResponse(c fiber.Ctx) error {
	view, err := s.flomorphicAccessView(c.Context())
	if err != nil {
		return err
	}
	return c.JSON(view)
}
