package osctrl

import "sync"

// Manager holds the current live osctrl client, rebuilt whenever the operator
// changes the connection settings. Get returns nil when osctrl isn't configured.
type Manager struct {
	mu     sync.RWMutex
	client *Client
}

func NewManager() *Manager { return &Manager{} }

func (m *Manager) Set(c *Client) {
	m.mu.Lock()
	m.client = c
	m.mu.Unlock()
}

func (m *Manager) Get() *Client {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.client
}

func (m *Manager) Configured() bool { return m.Get() != nil }
