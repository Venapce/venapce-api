package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/gofiber/fiber/v3"
	"github.com/jackc/pgx/v5"

	"github.com/Venapce/venapce-api/internal/db"
	"github.com/Venapce/venapce-api/internal/plugin"
)

// Venapce runs as a FloMorphic plugin: all business logic lives in FloMorphic
// workflows, and turnkey osctrl access is brokered by FloMorphic's `infra`. The
// operator defines a "venapce" plugin in the FloMorphic panel and pastes its
// plugin env here; we keep what we need and use INFRA_URL's host to drive
// infra's existing osspace flow (Google OAuth -> a provisioned osctrl space).
const flomorphicSettingKey = "flomorphic"

// infraOsspacePort is the HTTP port infra serves its license/command API on. The
// plugin env's INFRA_URL points at the NATS port (4222); the osspace endpoint
// lives on the same host at this port.
const infraOsspacePort = "8022"

// flomorphicConfig is the persisted plugin registration. INFRA_CRED is stored
// only as ciphertext; the rest is public-safe.
type flomorphicConfig struct {
	PluginID    string `json:"plugin_id"`
	InfraURLRaw string `json:"infra_url_raw"` // the plugin's INFRA_URL (nats://host:4222)
	InfraBase   string `json:"infra_base"`    // derived http://host:8022
	InfraCredEnc string `json:"infra_cred_enc,omitempty"`
}

func (s *Server) loadFlomorphicConfig(ctx context.Context) (*flomorphicConfig, error) {
	row, err := s.q.GetSetting(ctx, flomorphicSettingKey)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	var cfg flomorphicConfig
	if err := json.Unmarshal(row.Value, &cfg); err != nil {
		return nil, err
	}
	if cfg.InfraBase == "" {
		return nil, nil
	}
	return &cfg, nil
}

// GET /api/settings/flomorphic — public-safe view (no secret).
func (s *Server) getFlomorphicSettings(c fiber.Ctx) error {
	cfg, err := s.loadFlomorphicConfig(c.Context())
	if err != nil {
		return err
	}
	if cfg == nil {
		return c.JSON(fiber.Map{"configured": false})
	}
	// Surface whether a managed osctrl space is already wired up, so the card can
	// show "connected" without a second round-trip.
	osctrlManaged := false
	if oc, _ := s.loadOsctrlConfig(c.Context()); oc != nil {
		osctrlManaged = oc.Managed
	}
	return c.JSON(fiber.Map{
		"configured":    true,
		"pluginId":      cfg.PluginID,
		"infraBase":     cfg.InfraBase,
		"osctrlManaged": osctrlManaged,
		"plugin":        s.plg.Status(),
	})
}

type putFlomorphicBody struct {
	Env string `json:"env"` // the pasted plugin env block (KEY=VALUE lines)
}

// PUT /api/settings/flomorphic — register the FloMorphic plugin from its pasted
// env. We parse PLUGIN_ID / INFRA_CRED / INFRA_URL, derive the infra HTTP base,
// and store it (INFRA_CRED encrypted).
func (s *Server) putFlomorphicSettings(c fiber.Ctx) error {
	var body putFlomorphicBody
	if err := c.Bind().Body(&body); err != nil {
		return fiber.NewError(fiber.StatusBadRequest, "invalid body")
	}
	env := parsePluginEnv(body.Env)
	infraURL := env["INFRA_URL"]
	if infraURL == "" {
		return fiber.NewError(fiber.StatusBadRequest, "INFRA_URL missing from the plugin env — copy the full plugin env from the FloMorphic panel")
	}
	infraBase, err := deriveInfraBase(infraURL)
	if err != nil {
		return fiber.NewError(fiber.StatusBadRequest, "could not read a host from INFRA_URL: "+err.Error())
	}

	credEnc := ""
	if cred := env["INFRA_CRED"]; cred != "" {
		if credEnc, err = s.box.Encrypt(cred); err != nil {
			return err
		}
	} else if existing, _ := s.loadFlomorphicConfig(c.Context()); existing != nil {
		credEnc = existing.InfraCredEnc // keep the previous cred if the paste omits it
	}

	cfg := flomorphicConfig{
		PluginID:     env["PLUGIN_ID"],
		InfraURLRaw:  infraURL,
		InfraBase:    infraBase,
		InfraCredEnc: credEnc,
	}
	value, err := json.Marshal(cfg)
	if err != nil {
		return err
	}
	if _, err := s.q.UpsertSetting(c.Context(), db.UpsertSettingParams{Key: flomorphicSettingKey, Value: value}); err != nil {
		return err
	}

	// Pasting the plugin env is exactly the "run/restart the plugin" trigger: use
	// the just-saved env to (re)connect the in-process plugin to infra. A failure
	// here (infra unreachable, etc.) is reported on the response, not fatal — the
	// registration is saved and the plugin can be retried from the restart route.
	resp := fiber.Map{"configured": true, "pluginId": cfg.PluginID, "infraBase": cfg.InfraBase}
	if err := s.startVenapcePlugin(c.Context()); err != nil {
		resp["pluginError"] = err.Error()
	}
	resp["plugin"] = s.plg.Status()
	return c.JSON(resp)
}

// pluginEnv assembles the plugin connection env from the stored FloMorphic
// registration, decrypting INFRA_CRED. It reports ok=false (no error) when no
// registration is stored yet, so boot and the save path can both skip quietly.
func (s *Server) pluginEnv(ctx context.Context) (plugin.Env, bool, error) {
	cfg, err := s.loadFlomorphicConfig(ctx)
	if err != nil || cfg == nil {
		return plugin.Env{}, false, err
	}
	if cfg.InfraCredEnc == "" {
		return plugin.Env{}, false, nil
	}
	cred, err := s.box.Decrypt(cfg.InfraCredEnc)
	if err != nil {
		return plugin.Env{}, false, err
	}
	return plugin.Env{
		PluginID:  cfg.PluginID,
		InfraURL:  cfg.InfraURLRaw,
		InfraCred: cred,
	}, true, nil
}

// startVenapcePlugin (re)connects the in-process plugin from the stored env. It
// is a no-op when nothing is registered yet, and idempotent when the env is
// unchanged (the manager fingerprints it).
func (s *Server) startVenapcePlugin(ctx context.Context) error {
	env, ok, err := s.pluginEnv(ctx)
	if err != nil {
		return err
	}
	if !ok {
		return nil
	}
	return s.plg.Start(env)
}

// StartVenapcePlugin is the boot-time entry point (called from main), mirroring
// LoadOsctrlFromDB: it never blocks boot, and a failure only means the plugin is
// offline until the operator saves/restarts it.
func (s *Server) StartVenapcePlugin(ctx context.Context) error {
	return s.startVenapcePlugin(ctx)
}

// POST /api/settings/flomorphic/plugin/restart — force a (re)connect of the
// in-process plugin from the stored env, and report the resulting status.
func (s *Server) restartVenapcePlugin(c fiber.Ctx) error {
	env, ok, err := s.pluginEnv(c.Context())
	if err != nil {
		return err
	}
	if !ok {
		return fiber.NewError(fiber.StatusBadRequest, "no plugin env stored — paste the FloMorphic plugin env in Settings first")
	}
	if err := s.plg.Start(env); err != nil {
		return c.JSON(fiber.Map{"restarted": false, "plugin": s.plg.Status(), "error": err.Error()})
	}
	return c.JSON(fiber.Map{"restarted": true, "plugin": s.plg.Status()})
}

// POST /api/settings/flomorphic/osspace — drive infra's osspace flow. Idempotent
// and safe to poll:
//   - if infra has no space yet, it returns the Google-OAuth url the user must
//     open ({status:"pending", redirect});
//   - once the user has authenticated and the space is provisioned, infra returns
//     it, and we wire it up as a managed osctrl connection
//     ({status:"connected", environment, hostname}).
func (s *Server) connectOsspace(c fiber.Ctx) error {
	cfg, err := s.loadFlomorphicConfig(c.Context())
	if err != nil {
		return err
	}
	if cfg == nil {
		return fiber.NewError(fiber.StatusBadRequest, "FloMorphic is not connected — register the plugin env first")
	}

	res, err := fetchOsspace(c.Context(), cfg.InfraBase)
	if err != nil {
		return fiber.NewError(fiber.StatusBadGateway, err.Error())
	}
	if res.Redirect != "" {
		return c.JSON(fiber.Map{"status": "pending", "redirect": res.Redirect})
	}
	if res.Hostname == "" || res.Username == "" || res.Password == "" || res.Environment == "" {
		return fiber.NewError(fiber.StatusBadGateway, "infra returned an incomplete osctrl space")
	}

	// osctrl exposes its API on the space host; the client appends /api/v1.
	osctrlURL := "https://" + res.Hostname
	who, err := s.saveManagedOsctrl(c.Context(), osctrlURL, res.Username, res.Password, res.Environment)
	if err != nil {
		// The space exists but the login probe failed — still report it so the
		// front can surface the state rather than looping forever.
		return c.JSON(fiber.Map{
			"status":          "connected",
			"environment":     res.Environment,
			"hostname":        res.Hostname,
			"connected":       false,
			"connectionError": err.Error(),
		})
	}
	return c.JSON(fiber.Map{
		"status":      "connected",
		"environment": res.Environment,
		"hostname":    res.Hostname,
		"connected":   true,
		"connectedAs": who,
	})
}

// osspaceResult is the flattened outcome of a /lc/cmd/osspace call: either a
// redirect url to open, or the provisioned space fields.
type osspaceResult struct {
	Redirect    string
	Environment string
	Hostname    string
	Username    string
	Password    string
	Email       string
}

// fetchOsspace calls infra's GET /lc/cmd/osspace?format=json and flattens the
// {data,error} envelope. It tolerates an older infra that still 302-redirects
// (no format support) by reading the Location header instead.
func fetchOsspace(ctx context.Context, infraBase string) (*osspaceResult, error) {
	endpoint := strings.TrimRight(infraBase, "/") + "/lc/cmd/osspace?format=json"
	client := &http.Client{
		Timeout: 20 * time.Second,
		// Don't follow the OAuth redirect — surface it to the user instead.
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("reach infra at %s: %w", infraBase, err)
	}
	defer resp.Body.Close()

	// Older infra without the JSON opt-in: a plain redirect to Google.
	if resp.StatusCode >= 300 && resp.StatusCode < 400 {
		if loc := resp.Header.Get("Location"); loc != "" {
			return &osspaceResult{Redirect: loc}, nil
		}
		return nil, fmt.Errorf("infra redirected without a Location header")
	}

	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("infra osspace: %d %s", resp.StatusCode, truncateInfra(data))
	}

	var env struct {
		Data  json.RawMessage `json:"data"`
		Error json.RawMessage `json:"error"`
	}
	if err := json.Unmarshal(data, &env); err != nil {
		return nil, fmt.Errorf("invalid infra response: %w", err)
	}
	if len(env.Error) > 0 && string(env.Error) != "null" {
		return nil, fmt.Errorf("infra osspace error: %s", truncateInfra(env.Error))
	}
	var d osspaceResult
	if len(env.Data) > 0 && string(env.Data) != "null" {
		var raw struct {
			Redirect    string `json:"redirect"`
			Environment string `json:"environment"`
			Hostname    string `json:"hostname"`
			Username    string `json:"username"`
			Password    string `json:"password"`
			Email       string `json:"email"`
		}
		if err := json.Unmarshal(env.Data, &raw); err != nil {
			return nil, fmt.Errorf("invalid infra osspace payload: %w", err)
		}
		d = osspaceResult(raw)
	}
	return &d, nil
}

// parsePluginEnv reads a pasted KEY=VALUE env block into a map, tolerating blank
// lines, `#` comments, a leading `export `, and single/double quoted values.
func parsePluginEnv(text string) map[string]string {
	out := map[string]string{}
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimPrefix(line, "export ")
		eq := strings.IndexByte(line, '=')
		if eq <= 0 {
			continue
		}
		key := strings.TrimSpace(line[:eq])
		val := strings.TrimSpace(line[eq+1:])
		val = strings.Trim(val, `"'`)
		out[strings.ToUpper(key)] = val
	}
	return out
}

// deriveInfraBase turns the plugin's INFRA_URL (a NATS url on :4222, possibly a
// comma-separated cluster and possibly carrying credentials) into the infra HTTP
// base http://<host>:8022 that serves /lc/cmd/osspace.
func deriveInfraBase(infraURL string) (string, error) {
	first := strings.TrimSpace(strings.SplitN(infraURL, ",", 2)[0])
	if first == "" {
		return "", errors.New("empty")
	}
	// Strip scheme (nats://, tls://, ...) so we can parse host:port uniformly.
	if i := strings.Index(first, "://"); i >= 0 {
		first = first[i+3:]
	}
	// Strip any userinfo (user:pass@host).
	if at := strings.LastIndexByte(first, '@'); at >= 0 {
		first = first[at+1:]
	}
	// Drop a trailing path if present.
	if slash := strings.IndexByte(first, '/'); slash >= 0 {
		first = first[:slash]
	}
	host := first
	if h, _, err := splitHostPort(first); err == nil {
		host = h
	}
	if host == "" {
		return "", errors.New("no host")
	}
	return "http://" + net.JoinHostPort(host, infraOsspacePort), nil
}

func truncateInfra(b []byte) string {
	s := strings.TrimSpace(string(b))
	if len(s) > 300 {
		return s[:300] + "…"
	}
	return s
}

// splitHostPort wraps net.SplitHostPort but is tolerant of a bare host (no
// port), returning the host and an empty port instead of an error.
func splitHostPort(hostport string) (string, string, error) {
	h, p, err := net.SplitHostPort(hostport)
	if err != nil {
		// Bare host with no ":port".
		if !strings.Contains(hostport, ":") {
			return hostport, "", nil
		}
		return "", "", err
	}
	return h, p, nil
}
