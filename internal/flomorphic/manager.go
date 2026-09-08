package flomorphic

import (
	"strings"
	"sync"
)

// Access is everything venapce needs to reach a FloMorphic install: the API base,
// the shared HS256 secret it signs admin tokens with, and the host infra answers
// on (NATS :4222 for the plugin, osspace :8022 for the osctrl broker) — the value
// shipped as INFRA_URL when minting the plugin credential.
//
// It is seeded from the deploy environment (FLOMORPHIC_URL / FLOMORPHIC_JWT_SECRET
// / INFRA_HOST) and can be replaced at runtime from Settings, so an operator who
// installed without FloMorphic — or whose FloMorphic later moved — can point
// venapce at one without editing .env and recreating the container.
type Access struct {
	URL       string
	JWTSecret string
	InfraHost string
}

// Configured reports whether the access is complete enough to build a client.
func (a Access) Configured() bool {
	return strings.TrimSpace(a.URL) != "" && strings.TrimSpace(a.JWTSecret) != ""
}

// Manager holds the live client together with the access it was built from,
// rebuilt whenever the operator changes the FloMorphic connection. Get returns
// nil while that access is incomplete, so callers keep treating "not configured"
// as a nil client.
type Manager struct {
	mu     sync.RWMutex
	access Access
	client *Client
}

// NewManager builds a manager for the given access (typically the environment's).
func NewManager(a Access) *Manager {
	m := &Manager{}
	m.Set(a)
	return m
}

// Set replaces the access and rebuilds the live client from it.
func (m *Manager) Set(a Access) {
	a.URL = strings.TrimRight(strings.TrimSpace(a.URL), "/")
	a.JWTSecret = strings.TrimSpace(a.JWTSecret)
	a.InfraHost = strings.TrimSpace(a.InfraHost)
	client := NewClient(a.URL, a.JWTSecret)
	m.mu.Lock()
	m.access, m.client = a, client
	m.mu.Unlock()
}

// Get is the live client, or nil when FloMorphic access is not configured.
func (m *Manager) Get() *Client {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.client
}

// Access is the values the live client was built from.
func (m *Manager) Access() Access {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.access
}
