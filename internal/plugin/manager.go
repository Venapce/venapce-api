package plugin

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log"
	"net/url"
	"reflect"
	"strings"
	"sync"
	"unsafe"

	natsHandler "github.com/Inflowenger/go-plugin-sdk/nats"
	"github.com/Inflowenger/go-plugin-sdk/sdkv1"
	"github.com/jackc/pgx/v5/pgxpool"
	nats "github.com/nats-io/nats.go"

	"github.com/Venapce/venapce-api/internal/osctrl"
)

// Env is the infra identity the plugin connects with — the same three values the
// operator pastes into venapce from the FloMorphic panel. Everything else the
// plugin needs (the database, osctrl) is injected into the Manager, not carried
// here.
type Env struct {
	PluginID  string
	InfraURL  string // the plugin's INFRA_URL (nats://host:4222, possibly a cluster)
	InfraCred string // decoded INFRA_CRED (base64 NATS .creds blob)
}

func (e Env) valid() error {
	switch {
	case strings.TrimSpace(e.PluginID) == "":
		return fmt.Errorf("PLUGIN_ID is missing from the FloMorphic plugin env")
	case strings.TrimSpace(e.InfraURL) == "":
		return fmt.Errorf("INFRA_URL is missing from the FloMorphic plugin env")
	case strings.TrimSpace(e.InfraCred) == "":
		return fmt.Errorf("INFRA_CRED is missing from the FloMorphic plugin env")
	}
	return nil
}

// fingerprint identifies a connection env without keeping the credential in the
// key: Start is a no-op when the fingerprint is unchanged.
func (e Env) fingerprint() string {
	sum := sha256.Sum256([]byte(e.PluginID + "|" + e.InfraURL + "|" + e.InfraCred))
	return hex.EncodeToString(sum[:])
}

// Manager owns the lifecycle of the in-process plugin. It is started at boot from
// the stored env and re-started when the operator saves a new one.
type Manager struct {
	pool *pgxpool.Pool
	osc  *osctrl.Manager

	mu       sync.Mutex
	current  *sdkv1.Plugin
	fp       string
	pluginID string
	running  bool
	lastErr  string
}

func NewManager(pool *pgxpool.Pool, osc *osctrl.Manager) *Manager {
	return &Manager{pool: pool, osc: osc}
}

// Status is the public-safe view of the plugin's state, for the settings API.
type Status struct {
	// Running is the latched flag: we started the plugin and hold its connection.
	// It stays true across a transient infra blip and does NOT reflect the live
	// socket — use Connected/ConnState for that.
	Running bool `json:"running"`
	// Connected is a live probe of the underlying NATS connection to infra,
	// evaluated at each Status() call. False means the socket is down/closed even
	// though we still hold the plugin. Note: this reflects the infra link only —
	// removing the plugin's palette row in FloMorphic does not drop this link, so
	// it stays true until infra is unreachable or the credential is revoked.
	Connected bool `json:"connected"`
	// ConnState is the human-readable NATS connection state ("CONNECTED",
	// "RECONNECTING", "CLOSED", "DISCONNECTED", or "unknown"/"none").
	ConnState string `json:"connState,omitempty"`
	PluginID  string `json:"pluginId,omitempty"`
	Error     string `json:"error,omitempty"`
}

func (m *Manager) Status() Status {
	m.mu.Lock()
	defer m.mu.Unlock()
	connected, state := connState(m.current)
	return Status{
		Running:   m.running,
		Connected: connected,
		ConnState: state,
		PluginID:  m.pluginID,
		Error:     m.lastErr,
	}
}

// Start connects the plugin to infra with the given env and subscribes its
// actions. It is idempotent: called again with the same env it does nothing;
// called with a changed env it drains the previous connection (best effort — the
// SDK exposes no teardown) and reconnects. A connect/start failure is recorded on
// the status and returned.
func (m *Manager) Start(env Env) error {
	if err := env.valid(); err != nil {
		return err
	}
	fp := env.fingerprint()

	m.mu.Lock()
	defer m.mu.Unlock()

	if m.running && fp == m.fp {
		return nil // already serving this exact env
	}

	// A genuinely different env: retire the previous connection so its stale
	// subscriptions do not linger. Its subjects are namespaced by the old
	// PLUGIN_ID, so this only reclaims the connection — it never races the new one.
	if m.current != nil {
		drainPlugin(m.current)
		m.current = nil
		m.running = false
	}

	natsURL, err := natsURL(env.InfraURL)
	if err != nil {
		m.lastErr = err.Error()
		return err
	}

	p, err := sdkv1.NewPlugin(
		sdkv1.WithPluginId(env.PluginID),
		sdkv1.WithInfraConnection(natsURL, env.InfraCred),
	)
	if err != nil {
		m.lastErr = fmt.Sprintf("connect to infra: %v", err)
		return fmt.Errorf("venapce plugin: connect to infra: %w", err)
	}

	register(p, m.pool, m.osc)

	if err := p.Start(); err != nil {
		m.lastErr = fmt.Sprintf("start: %v", err)
		return fmt.Errorf("venapce plugin: start: %w", err)
	}

	m.current = p
	m.fp = fp
	m.pluginID = env.PluginID
	m.running = true
	m.lastErr = ""
	log.Printf("venapce plugin %s: connected to infra as %s", version, env.PluginID)
	return nil
}

// natsURL reduces the plugin's INFRA_URL — which may be a comma-separated cluster
// and may carry a scheme — to a single url the SDK's WithInfraConnection can
// parse for its host:port. WithInfraConnection dials nats://<host>, so the scheme
// here is only to make url.Parse yield a Host.
func natsURL(infraURL string) (string, error) {
	first := strings.TrimSpace(strings.SplitN(infraURL, ",", 2)[0])
	if first == "" {
		return "", fmt.Errorf("INFRA_URL is empty")
	}
	if !strings.Contains(first, "://") {
		first = "nats://" + first
	}
	u, err := url.Parse(first)
	if err != nil || u.Host == "" {
		return "", fmt.Errorf("could not read a host from INFRA_URL %q", infraURL)
	}
	return "nats://" + u.Host, nil
}

// connState live-probes the NATS connection behind a plugin and reports whether
// it is connected plus its state name. It reaches the connection through the same
// (unexported) path as drainPlugin, so it is strictly best effort: any failure
// (a changed SDK field, a nil connection) yields (false, "unknown") rather than a
// panic. Returns (false, "none") when no plugin is held.
func connState(p *sdkv1.Plugin) (connected bool, state string) {
	defer func() {
		if recover() != nil {
			connected, state = false, "unknown"
		}
	}()
	nc := liveConn(p)
	if nc == nil {
		if p == nil {
			return false, "none"
		}
		return false, "unknown"
	}
	return nc.IsConnected(), nc.Status().String()
}

// liveConn extracts the *nats.Conn behind a plugin via reflection (the SDK keeps
// its connector unexported). Shared by connState and drainPlugin. Returns nil on
// any mismatch.
func liveConn(p *sdkv1.Plugin) *nats.Conn {
	if p == nil {
		return nil
	}
	field := reflect.ValueOf(p).Elem().FieldByName("infraConn")
	if !field.IsValid() || field.Kind() != reflect.Pointer || field.IsNil() {
		return nil
	}
	conn, ok := reflect.NewAt(field.Type(), unsafe.Pointer(field.UnsafeAddr())).
		Elem().Interface().(*natsHandler.Nats)
	if !ok || conn == nil {
		return nil
	}
	return conn.GetConnection()
}

// drainPlugin closes the NATS connection behind a plugin so its subscriptions are
// released on restart. The SDK (go-plugin-sdk) offers no Close/Drain and keeps its
// *nats.Conn unexported, so this reaches the connection through liveConn and
// drains it. It is strictly best effort: any failure (a changed SDK field, a nil
// connection) is swallowed, because a leaked connection until the next process
// restart is a lesser evil than a panic.
func drainPlugin(p *sdkv1.Plugin) {
	defer func() { _ = recover() }()
	if nc := liveConn(p); nc != nil {
		_ = nc.Drain()
	}
}
