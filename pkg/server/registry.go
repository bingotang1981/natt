package server

import (
	"net"
	"sync"
)

// ClientInfo holds metadata about a connected client.
type ClientInfo struct {
	ClientID    string
	ControlConn net.Conn
	Token       string
}

// Registry manages active client connections (thread-safe).
type Registry struct {
	mu      sync.RWMutex
	clients map[string]*ClientInfo // clientID → info
}

// NewRegistry creates a new Registry.
func NewRegistry() *Registry {
	return &Registry{clients: make(map[string]*ClientInfo)}
}

// Register adds a client to the registry.
func (r *Registry) Register(info *ClientInfo) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.clients[info.ClientID] = info
}

// Get returns the client info for the given clientID.
func (r *Registry) Get(clientID string) (*ClientInfo, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	info, ok := r.clients[clientID]
	return info, ok
}

// Remove removes a client from the registry and returns its info.
func (r *Registry) Remove(clientID string) *ClientInfo {
	r.mu.Lock()
	defer r.mu.Unlock()
	info, ok := r.clients[clientID]
	if ok {
		delete(r.clients, clientID)
	}
	return info
}

// List returns all registered client IDs.
func (r *Registry) List() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	ids := make([]string, 0, len(r.clients))
	for id := range r.clients {
		ids = append(ids, id)
	}
	return ids
}

// Count returns the number of registered clients.
func (r *Registry) Count() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.clients)
}
