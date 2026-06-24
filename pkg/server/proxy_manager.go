package server

import (
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"sync"
	"time"

	"natt/pkg/protocol"
)

// pendingConn represents an external user connection waiting to be paired
// with a client data connection.
type pendingConn struct {
	conn    net.Conn
	created time.Time
	proxy   ProxyState
}

// ProxyState holds the state of one proxy mapping.
type ProxyState struct {
	Name       string
	LocalIP    string
	LocalPort  int
	RemotePort int
	ClientID   string // client that owns this proxy
	Listener   net.Listener
}

// RProxyState holds the state of one reverse proxy mapping.
type RProxyState struct {
	Name       string
	RemoteIP   string
	RemotePort int
	ClientID   string
}

// ProxyManager manages proxy/rproxy port mappings and bridges connections.
type ProxyManager struct {
	mu           sync.RWMutex
	proxies      map[int]*ProxyState     // remotePort → proxy
	clientPorts  map[string][]int        // clientID → list of remote ports
	pending      map[string]*pendingConn // dataConnId → pending external conn
	rproxies     map[string]*RProxyState // rproxy name → rproxy
	clientRProps map[string][]string     // clientID → list of rproxy names
}

// NewProxyManager creates a new ProxyManager.
func NewProxyManager() *ProxyManager {
	return &ProxyManager{
		proxies:      make(map[int]*ProxyState),
		clientPorts:  make(map[string][]int),
		pending:      make(map[string]*pendingConn),
		rproxies:     make(map[string]*RProxyState),
		clientRProps: make(map[string][]string),
	}
}

// StartProxy starts listening on remotePort for a proxy mapping.
// When an external user connects, it sends a TunnelOpen to the client
// and waits for a data connection via PairDataConn.
// Returns the actual port being listened on (useful when remotePort is 0).
func (pm *ProxyManager) StartProxy(ps ProxyState, controlConn net.Conn, clientID string) (int, error) {
	pm.mu.Lock()
	defer pm.mu.Unlock()

	// Check if port is already taken (only if a specific port was requested)
	if ps.RemotePort != 0 {
		if _, exists := pm.proxies[ps.RemotePort]; exists {
			return 0, fmt.Errorf("remote port %d already in use", ps.RemotePort)
		}
	}

	addr := fmt.Sprintf(":%d", ps.RemotePort)
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		return 0, fmt.Errorf("listen on %d: %w", ps.RemotePort, err)
	}

	// Get the actual port assigned by the OS
	actualPort := listener.Addr().(*net.TCPAddr).Port
	ps.RemotePort = actualPort
	ps.ClientID = clientID
	ps.Listener = listener
	pm.proxies[actualPort] = &ps
	pm.clientPorts[clientID] = append(pm.clientPorts[clientID], actualPort)

	go pm.acceptLoop(&ps, controlConn, clientID)
	return actualPort, nil
}

func (pm *ProxyManager) acceptLoop(ps *ProxyState, controlConn net.Conn, clientID string) {
	for {
		extConn, err := ps.Listener.Accept()
		if err != nil {
			// Listener closed (proxy stopped)
			return
		}

		dataConnID := pm.allocateConnID()
		slog.Debug("external connection accepted",
			"proxy", ps.Name,
			"remote", extConn.RemoteAddr(),
			"dataConnId", dataConnID,
		)

		// Store pending connection
		pm.mu.Lock()
		pm.pending[dataConnID] = &pendingConn{
			conn:    extConn,
			created: time.Now(),
			proxy:   *ps,
		}
		pm.mu.Unlock()

		// Send TunnelOpen via control connection
		payload, _ := json.Marshal(map[string]string{
			"dataConnId": dataConnID,
			"proxyName":  ps.Name,
		})
		msg := &protocol.Message{
			Type:    protocol.TypeTunnelOpen,
			Payload: payload,
		}
		if err := protocol.WriteMessage(controlConn, msg); err != nil {
			slog.Warn("send TunnelOpen", "proxy", ps.Name, "error", err)
			pm.removePending(dataConnID)
			extConn.Close()
		}
	}
}

// PairDataConn pairs an incoming data connection (with DataConnect message)
// with its waiting external connection and starts io.Copy bridging.
// Returns true if paired successfully.
func (pm *ProxyManager) PairDataConn(dataConnID string, dataConn net.Conn) bool {
	pm.mu.Lock()
	p, ok := pm.pending[dataConnID]
	if !ok {
		pm.mu.Unlock()
		return false
	}
	delete(pm.pending, dataConnID)
	pm.mu.Unlock()

	extConn := p.conn
	slog.Debug("pairing data connection",
		"dataConnId", dataConnID,
		"proxy", p.proxy.Name,
	)

	// Bridge: external conn ↔ data conn
	go func() {
		var wg sync.WaitGroup
		wg.Add(2)

		go func() {
			defer wg.Done()
			_, err := io.Copy(dataConn, extConn)
			if err != nil {
				slog.Debug("bridge ext→data closed", "dataConnId", dataConnID, "error", err)
			}
		}()
		go func() {
			defer wg.Done()
			_, err := io.Copy(extConn, dataConn)
			if err != nil {
				slog.Debug("bridge data→ext closed", "dataConnId", dataConnID, "error", err)
			}
		}()

		wg.Wait()
		dataConn.Close()
		extConn.Close()
		slog.Debug("bridge closed", "dataConnId", dataConnID)
	}()

	return true
}

// StopProxy stops listening on the given remote port and removes it from the map.
func (pm *ProxyManager) StopProxy(remotePort int) {
	pm.mu.Lock()
	defer pm.mu.Unlock()
	if ps, ok := pm.proxies[remotePort]; ok {
		ps.Listener.Close()
		delete(pm.proxies, remotePort)
		// Also remove from clientPorts tracking
		ports := pm.clientPorts[ps.ClientID]
		for i, p := range ports {
			if p == remotePort {
				pm.clientPorts[ps.ClientID] = append(ports[:i], ports[i+1:]...)
				break
			}
		}
		slog.Info("proxy stopped", "name", ps.Name, "port", remotePort)
	}
}

// StartRProxy registers an rproxy mapping. When a client data connection
// arrives with this rproxy name, the server will dial remoteIP:remotePort.
func (pm *ProxyManager) StartRProxy(rs RProxyState) error {
	pm.mu.Lock()
	defer pm.mu.Unlock()
	if _, exists := pm.rproxies[rs.Name]; exists {
		return fmt.Errorf("rproxy %q already exists", rs.Name)
	}
	pm.rproxies[rs.Name] = &rs
	pm.clientRProps[rs.ClientID] = append(pm.clientRProps[rs.ClientID], rs.Name)
	slog.Info("rproxy registered", "name", rs.Name, "remote", fmt.Sprintf("%s:%d", rs.RemoteIP, rs.RemotePort))
	return nil
}

// GetRProxy looks up an rproxy by name. Returns nil if not found.
func (pm *ProxyManager) GetRProxy(name string) *RProxyState {
	pm.mu.RLock()
	defer pm.mu.RUnlock()
	return pm.rproxies[name]
}

// StopClientProxies stops all proxy/rproxy resources belonging to the given client.
func (pm *ProxyManager) StopClientProxies(clientID string) {
	pm.mu.Lock()
	ports := pm.clientPorts[clientID]
	delete(pm.clientPorts, clientID)
	rpNames := pm.clientRProps[clientID]
	delete(pm.clientRProps, clientID)
	pm.mu.Unlock()

	for _, port := range ports {
		pm.StopProxy(port)
	}

	// Clean up rproxies
	for _, name := range rpNames {
		pm.mu.Lock()
		delete(pm.rproxies, name)
		pm.mu.Unlock()
	}

	if len(ports)+len(rpNames) > 0 {
		slog.Info("cleaned up client resources", "clientId", clientID,
			"proxies", len(ports), "rproxies", len(rpNames))
	}
}

// StopAll stops all proxy listeners, closes pending connections, and clears rproxies.
func (pm *ProxyManager) StopAll() {
	pm.mu.Lock()
	defer pm.mu.Unlock()
	for port, ps := range pm.proxies {
		ps.Listener.Close()
		delete(pm.proxies, port)
	}
	for id, p := range pm.pending {
		p.conn.Close()
		delete(pm.pending, id)
	}
	// Clear all client tracking
	for id := range pm.clientPorts {
		delete(pm.clientPorts, id)
	}
	for id := range pm.rproxies {
		delete(pm.rproxies, id)
	}
	for id := range pm.clientRProps {
		delete(pm.clientRProps, id)
	}
}

func (pm *ProxyManager) removePending(dataConnID string) {
	pm.mu.Lock()
	defer pm.mu.Unlock()
	delete(pm.pending, dataConnID)
}

func (pm *ProxyManager) allocateConnID() string {
	return uuidV4()
}

// uuidV4 generates a random UUID v4 string (e.g. "f47ac10b-58cc-4372-a567-0e02b2c3d479").
func uuidV4() string {
	var buf [16]byte
	rand.Read(buf[:])
	// Set version 4 (4 bits: 0100)
	buf[6] = (buf[6] & 0x0f) | 0x40
	// Set variant bits (2 bits: 10)
	buf[8] = (buf[8] & 0x3f) | 0x80
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x",
		buf[0:4], buf[4:6], buf[6:8], buf[8:10], buf[10:16])
}

// CleanupStalePending removes pending connections that have timed out.
func (pm *ProxyManager) CleanupStalePending(timeout time.Duration) {
	pm.mu.Lock()
	defer pm.mu.Unlock()
	now := time.Now()
	for id, p := range pm.pending {
		if now.Sub(p.created) > timeout {
			slog.Warn("pending connection timed out", "dataConnId", id)
			p.conn.Close()
			delete(pm.pending, id)
		}
	}
}
