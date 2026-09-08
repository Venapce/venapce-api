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

	"github.com/Venapce/venapce-api/internal/config"
	"github.com/Venapce/venapce-api/internal/db"
	"github.com/Venapce/venapce-api/internal/flomorphic"
	"github.com/Venapce/venapce-api/internal/osctrl"
	"github.com/Venapce/venapce-api/internal/plugin"
)

// Venapce runs as a FloMorphic plugin: all business logic lives in FloMorphic
// workflows, and turnkey osctrl access is brokered by FloMorphic's `infra`. The
// backend mints its own plugin credential from the FloMorphic API
// (FLOMORPHIC_URL + FLOMORPHIC_JWT_SECRET) and stores the returned plugin env; we
// keep what we need and use INFRA_URL's host to drive infra's existing osspace
// flow (Google OAuth -> a provisioned osctrl space).
const flomorphicSettingKey = "flomorphic"

// errFlomorphicUnconfigured is what every route that needs the FloMorphic API says
// when there is no usable client. It names the Settings card first because that is
// now the way to fix it without restarting the container.
const errFlomorphicUnconfigured = "FloMorphic API is not configured — set the FloMorphic URL and JWT secret in Settings (or FLOMORPHIC_URL / FLOMORPHIC_JWT_SECRET in the environment)"

// floClient is the live FloMorphic client, or nil when the access is incomplete.
func (s *Server) floClient() *flomorphic.Client { return s.flo.Get() }

// infraOsspacePort is the HTTP port infra serves its license/command API on. The
// plugin env's INFRA_URL points at the NATS port (4222); the osspace endpoint
// lives on the same host at this port.
const infraOsspacePort = "8022"

// flomorphicConfig is the persisted plugin registration. INFRA_CRED is stored
// only as ciphertext; the rest is public-safe.
type flomorphicConfig struct {
	// ExtensionID is the FloMorphic extension row id — what we sync (build palette
	// nodes) and delete (refresh) against. FloMorphic assigns it on create.
	ExtensionID string `json:"extension_id,omitempty"`
	// PluginID is the inflowv1 identity FloMorphic assigned that row (name-<uuid>);
	// the plugin connects as it and the credential is scoped to it.
	PluginID     string `json:"plugin_id"`
	InfraURLRaw  string `json:"infra_url_raw"` // the plugin's INFRA_URL (nats://host:4222)
	InfraBase    string `json:"infra_base"`    // derived http://host:8022
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
	// Surface the live FloMorphic access (env defaults, overridden by whatever the
	// operator saved in Settings) so the card can show the URL and whether the
	// signing secret is set, and offer "connect" without asking anyone to paste
	// anything. The secret value itself is never returned. apiConfigured is true
	// only when URL and secret are both present (== a usable client).
	apiInfo, err := s.flomorphicAccessView(c.Context())
	if err != nil {
		return err
	}
	if cfg == nil {
		apiInfo["configured"] = false
		return c.JSON(apiInfo)
	}
	// Surface whether a managed osctrl space is already wired up, so the card can
	// show "connected" without a second round-trip.
	osctrlManaged := false
	if oc, _ := s.loadOsctrlConfig(c.Context()); oc != nil {
		osctrlManaged = oc.Managed
	}
	apiInfo["configured"] = true
	apiInfo["extensionId"] = cfg.ExtensionID
	apiInfo["pluginId"] = cfg.PluginID
	apiInfo["infraBase"] = cfg.InfraBase
	apiInfo["osctrlManaged"] = osctrlManaged
	apiInfo["plugin"] = s.plg.Status()
	return c.JSON(apiInfo)
}

// defaultPluginName / defaultPluginDescription label venapce's palette extension
// in FloMorphic. The name is a prefix of the assigned plugin id; the description
// shows on the extension card.
const (
	defaultPluginName        = "venapce"
	defaultPluginDescription = "Venapce — osquery + data nodes"
)

// PUT /api/settings/flomorphic — register venapce as a FloMorphic plugin and make
// its nodes available in the canvas palette. The full lifecycle is driven here:
// ensure the extension row exists (FloMorphic assigns its plugin id), mint a
// credential for that id, connect the in-process plugin, then sync its actions
// into palette nodes. Reuses the stored row on repeat calls.
func (s *Server) putFlomorphicSettings(c fiber.Ctx) error {
	if s.floClient() == nil {
		return fiber.NewError(fiber.StatusBadRequest, errFlomorphicUnconfigured)
	}
	return s.registerPlugin(c, false)
}

// POST /api/settings/flomorphic/plugin/refresh — redefine venapce in FloMorphic:
// delete the existing extension row (and its synced nodes), then register afresh
// (new row + plugin id + credential) and re-sync. This is the "clear and re-add"
// path for picking up a changed action set.
func (s *Server) refreshVenapcePlugin(c fiber.Ctx) error {
	if s.floClient() == nil {
		return fiber.NewError(fiber.StatusBadRequest, errFlomorphicUnconfigured)
	}
	return s.registerPlugin(c, true)
}

// registerPlugin ensures venapce is registered as a FloMorphic extension, its
// plugin connected, and its palette nodes synced. When recreate is true it first
// deletes the stored extension row so a brand-new one (and plugin id) is issued.
func (s *Server) registerPlugin(c fiber.Ctx, recreate bool) error {
	ctx := c.Context()
	existing, err := s.loadFlomorphicConfig(ctx)
	if err != nil {
		return err
	}

	// Refresh = clear the old row (takes its synced nodes with it) so we register
	// clean. Best effort: a missing row is fine, and a delete failure still lets us
	// try to create a new one.
	if recreate && existing != nil && existing.ExtensionID != "" {
		if err := s.floClient().DeleteExtension(ctx, existing.ExtensionID); err != nil {
			return fiber.NewError(fiber.StatusBadGateway, "delete existing FloMorphic extension: "+err.Error())
		}
		existing = nil
	}

	// Reuse the stored row when we have one, else register a new extension. The
	// plugin id comes from FloMorphic (it ignores any we'd send), so it is the
	// row's assigned id in both cases.
	extID, pluginID := "", ""
	if existing != nil {
		extID, pluginID = existing.ExtensionID, existing.PluginID
	}
	if extID == "" || pluginID == "" {
		ext, err := s.floClient().CreateExtension(ctx, defaultPluginName, defaultPluginDescription)
		if err != nil {
			return fiber.NewError(fiber.StatusBadGateway, "register extension in FloMorphic: "+err.Error())
		}
		extID, pluginID = ext.ID, ext.PluginID
	}

	// Mint a credential for the assigned plugin id, pointing INFRA_URL at the host
	// venapce can actually reach (INFRA_HOST); FloMorphic's env renderer lets a
	// declared INFRA_URL win, and deriveInfraBase yields the matching osspace base.
	var extra []flomorphic.EnvVar
	if url := config.InfraNatsURL(s.flo.Access().InfraHost); url != "" {
		extra = append(extra, flomorphic.EnvVar{Key: "INFRA_URL", Value: url})
	}
	envText, _, err := s.floClient().MintPluginEnv(ctx, pluginID, extra)
	if err != nil {
		return fiber.NewError(fiber.StatusBadGateway, "mint plugin credential from FloMorphic: "+err.Error())
	}

	// Persist the registration (INFRA_CRED encrypted) and connect the plugin.
	if err := s.storePluginEnv(ctx, extID, envText); err != nil {
		return err
	}

	resp := fiber.Map{"configured": true, "extensionId": extID, "pluginId": pluginID}
	if err := s.startVenapcePlugin(ctx); err != nil {
		// Without a connected plugin there is nothing for FloMorphic to sync, so
		// stop here and report — the registration is saved and can be retried.
		resp["pluginError"] = err.Error()
		resp["plugin"] = s.plg.Status()
		return c.JSON(resp)
	}

	// Build the palette nodes from the now-connected plugin's @actions. The plugin
	// has just connected, so FloMorphic may need a moment to reach it — retry a few
	// times before giving up.
	sync, err := s.syncWithRetry(ctx, extID)
	if err != nil {
		resp["syncError"] = err.Error()
	} else {
		resp["nodes"] = sync.Added
	}
	resp["plugin"] = s.plg.Status()
	if probe := s.pluginProbe(ctx, extID); probe != nil {
		resp["probe"] = probe
	}
	return c.JSON(resp)
}

// storePluginEnv parses a minted plugin dotenv and persists it as the registration
// (INFRA_CRED encrypted, extension id kept for sync/delete).
func (s *Server) storePluginEnv(ctx context.Context, extID, envText string) error {
	env := parsePluginEnv(envText)
	infraURL := env["INFRA_URL"]
	if infraURL == "" {
		return fiber.NewError(fiber.StatusBadGateway, "FloMorphic returned a plugin env with no INFRA_URL")
	}
	infraBase, err := deriveInfraBase(infraURL)
	if err != nil {
		return fiber.NewError(fiber.StatusBadGateway, "could not read a host from INFRA_URL: "+err.Error())
	}

	credEnc := ""
	if cred := env["INFRA_CRED"]; cred != "" {
		if credEnc, err = s.box.Encrypt(cred); err != nil {
			return err
		}
	} else if existing, _ := s.loadFlomorphicConfig(ctx); existing != nil {
		credEnc = existing.InfraCredEnc // keep the previous cred if the mint omits it
	}

	cfg := flomorphicConfig{
		ExtensionID:  extID,
		PluginID:     env["PLUGIN_ID"],
		InfraURLRaw:  infraURL,
		InfraBase:    infraBase,
		InfraCredEnc: credEnc,
	}
	value, err := json.Marshal(cfg)
	if err != nil {
		return err
	}
	if _, err := s.q.UpsertSetting(ctx, db.UpsertSettingParams{Key: flomorphicSettingKey, Value: value}); err != nil {
		return err
	}
	return nil
}

// syncWithRetry asks FloMorphic to sync the plugin's actions into palette nodes,
// retrying briefly because the plugin has just (re)connected and FloMorphic's
// live @actions probe can race the subscription coming up.
func (s *Server) syncWithRetry(ctx context.Context, extID string) (flomorphic.SyncResult, error) {
	var lastErr error
	for attempt := 0; attempt < 4; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return flomorphic.SyncResult{}, ctx.Err()
			case <-time.After(600 * time.Millisecond):
			}
		}
		res, err := s.floClient().SyncExtension(ctx, extID)
		if err == nil {
			return res, nil
		}
		lastErr = err
	}
	return flomorphic.SyncResult{}, lastErr
}

// pluginProbe reports the plugin's connectivity as FloMorphic sees it — the
// authoritative reference — by asking FloMorphic to reach the plugin over
// inflowv1 (@actions). It is best effort with a short timeout and returns nil when
// there is no extension row to probe.
func (s *Server) pluginProbe(ctx context.Context, extID string) fiber.Map {
	if s.floClient() == nil || strings.TrimSpace(extID) == "" {
		return nil
	}
	pctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	actions, err := s.floClient().ProbeExtension(pctx, extID)
	if err != nil {
		return fiber.Map{"reachable": false, "error": err.Error()}
	}
	return fiber.Map{"reachable": true, "actions": actions}
}

// POST /api/settings/flomorphic/plugin/check — report the plugin's connectivity
// as FloMorphic sees it (a live @actions round-trip through FloMorphic).
func (s *Server) checkVenapcePlugin(c fiber.Ctx) error {
	cfg, err := s.loadFlomorphicConfig(c.Context())
	if err != nil {
		return err
	}
	if cfg == nil || cfg.ExtensionID == "" {
		return fiber.NewError(fiber.StatusBadRequest, "venapce is not registered in FloMorphic yet — connect first")
	}
	probe := s.pluginProbe(c.Context(), cfg.ExtensionID)
	return c.JSON(fiber.Map{"probe": probe, "plugin": s.plg.Status()})
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
		return fiber.NewError(fiber.StatusBadRequest, "no plugin env stored — connect FloMorphic in Settings first")
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

	// Idempotent by design: an osctrl space is provisioned once per inflowenger
	// license (infra keys the space off the license ID carried in the plugin env),
	// so there is never a second space to create. Once we've wired one up, a repeat
	// "connect" click must NOT re-enter infra's osspace/OAuth flow — that would send
	// the operator back through Google sign-in and leave the front polling forever
	// for a space they already own. Re-probe the stored space and return it as the
	// latest registered one.
	if oc, _ := s.loadOsctrlConfig(c.Context()); oc != nil && oc.Managed {
		return s.reportManagedOsspace(c, oc)
	}

	res, err := fetchOsspace(c.Context(), cfg.InfraBase)
	if err != nil {
		return fiber.NewError(fiber.StatusBadGateway, err.Error())
	}

	// When a space already exists for this license (an installed instance whose
	// space was created earlier), infra hands back the credentials immediately —
	// often alongside a `redirect` to the space's own page. A present space always
	// wins over that redirect: there is nothing left to provision, so wire it up as
	// connected instead of sending the front into a polling loop against the
	// redirect. Only fall through to `pending` when infra gave us no usable space.
	if res.Hostname != "" && res.Username != "" && res.Password != "" {
		return s.wireOsspace(c, res)
	}
	if res.Redirect != "" {
		return c.JSON(fiber.Map{"status": "pending", "redirect": res.Redirect})
	}
	return fiber.NewError(fiber.StatusBadGateway, "infra returned an incomplete osctrl space")
}

// wireOsspace persists a space infra returned as a managed osctrl connection,
// probes its login, and reports it as connected. Split out of connectOsspace so
// the "space returned directly" path is shared regardless of whether infra also
// sent a redirect.
func (s *Server) wireOsspace(c fiber.Ctx, res *osspaceResult) error {
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

// reportManagedOsspace re-probes an osctrl space that was already provisioned for
// this license and returns it in the same shape as a fresh osspace connect, so a
// repeat "connect" click resolves to an immediate success instead of kicking off
// a new provisioning/OAuth round. It rebuilds the live client from the stored
// (encrypted) credentials rather than re-fetching from infra.
func (s *Server) reportManagedOsspace(c fiber.Ctx, cfg *osctrlConfig) error {
	pass, err := s.box.Decrypt(cfg.PasswordEnc)
	if err != nil {
		return err
	}
	client := osctrl.NewClient(cfg.URL, cfg.Username, pass, cfg.Environment)
	s.osc.Set(client)

	resp := fiber.Map{
		"status":      "connected",
		"environment": cfg.Environment,
		"hostname":    strings.TrimPrefix(strings.TrimPrefix(cfg.URL, "https://"), "http://"),
	}
	if who, err := client.TestLogin(c.Context()); err != nil {
		resp["connected"] = false
		resp["connectionError"] = err.Error()
	} else {
		resp["connected"] = true
		resp["connectedAs"] = who
	}
	return c.JSON(resp)
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
