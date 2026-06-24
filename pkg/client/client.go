// Package client implements the NAT traversal client.
package client

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"strconv"
	"sync"
	"time"

	"natt/pkg/config"
	"natt/pkg/crypto"
	"natt/pkg/protocol"
)

// Client is the NAT traversal client.
type Client struct {
	cfg     config.ClientConfig
	encKey  []byte
	control net.Conn
	proxies []config.ProxyRule
	dataMgr *DataConnManager

	heartStop chan struct{}
	heartWg   sync.WaitGroup

	tunnels         map[string]context.CancelFunc // dataConnId → cancel
	tunnelsMu       sync.Mutex
	rproxyListeners []*RProxyListener

	mu     sync.Mutex
	active bool
}

// New creates a new Client.
func New(cfg config.ClientConfig) (*Client, error) {
	encKey, err := config.DecryptKey(cfg.EncryptKey)
	if err != nil {
		return nil, fmt.Errorf("encryptKey: %w", err)
	}
	if encKey != nil {
		slog.Info("encryption enabled (AES-256-GCM)")
	} else {
		slog.Warn("encryption disabled — data transmitted in plaintext")
	}

	return &Client{
		cfg:             cfg,
		encKey:          encKey,
		proxies:         cfg.Proxies,
		dataMgr:         NewDataConnManager(cfg.ServerAddr, cfg.ServerPort, encKey),
		tunnels:         make(map[string]context.CancelFunc),
		rproxyListeners: make([]*RProxyListener, 0),
	}, nil
}

// Run connects to the server, registers, sets up proxies, and enters the
// control message loop. It reconnects automatically on disconnection.
func (c *Client) Run() error {
	c.active = true // mark as running; only Stop() can set this to false
	for {
		if err := c.connectAndServe(); err != nil {
			slog.Error("connection error", "error", err)
		}

		// Reconnect with exponential backoff
		if !c.reconnectLoop() {
			return fmt.Errorf("client stopped")
		}
	}
}

// Stop gracefully shuts down the client.
func (c *Client) Stop() {
	c.mu.Lock()
	c.active = false
	if c.control != nil {
		c.control.Close()
	}
	c.mu.Unlock()

	c.stopHeartbeat()
	c.closeAllTunnels()
	c.closeAllRProxy()
}

func (c *Client) connectAndServe() error {
	addr := net.JoinHostPort(c.cfg.ServerAddr, strconv.Itoa(c.cfg.ServerPort))
	slog.Info("connecting to server", "addr", addr)

	conn, err := net.DialTimeout("tcp", addr, 10*time.Second)
	if err != nil {
		return fmt.Errorf("dial %s: %w", addr, err)
	}

	// Enable TCP keepalive so dead connections are detected promptly
	if tcpConn, ok := conn.(*net.TCPConn); ok {
		tcpConn.SetKeepAlive(true)
		tcpConn.SetKeepAlivePeriod(75 * time.Second)
	}

	// Wrap with encryption immediately
	if c.encKey != nil {
		cipherConn, cerr := crypto.NewCipherConn(conn, c.encKey)
		if cerr != nil {
			conn.Close()
			return fmt.Errorf("wrap cipher: %w", cerr)
		}
		conn = cipherConn
	}

	c.mu.Lock()
	c.control = conn
	c.active = true
	c.mu.Unlock()

	defer func() {
		c.mu.Lock()
		c.control = nil
		c.mu.Unlock()
		conn.Close()
		c.stopHeartbeat()
		c.closeAllTunnels()
		c.closeAllRProxy()
	}()

	// Register
	if err := c.register(conn); err != nil {
		return err
	}
	slog.Info("registered with server")

	// Send proxy request
	if err := c.requestProxies(conn); err != nil {
		return err
	}
	slog.Info("proxy request sent", "count", len(c.proxies))

	// Send rproxy request and start local listeners
	if len(c.cfg.RProxies) > 0 {
		if err := c.requestRProxies(conn); err != nil {
			return err
		}
		slog.Info("rproxy request sent", "count", len(c.cfg.RProxies))
		if err := c.startRProxyListeners(conn); err != nil {
			return err
		}
	}

	// Start heartbeat
	c.startHeartbeat(conn)

	// Enter control message loop
	return c.messageLoop(conn)
}

func (c *Client) register(conn net.Conn) error {
	payload, _ := json.Marshal(map[string]string{
		"clientId": fmt.Sprintf("client-%d", time.Now().UnixNano()),
		"token":    c.cfg.Token,
		"version":  "2.0.0",
	})
	msg := &protocol.Message{Type: protocol.TypeRegister, Payload: payload}
	if err := protocol.WriteMessage(conn, msg); err != nil {
		return fmt.Errorf("send Register: %w", err)
	}

	ack, err := protocol.ReadMessage(conn)
	if err != nil {
		return fmt.Errorf("read RegisterAck: %w", err)
	}
	if ack.Type != protocol.TypeRegisterAck {
		return fmt.Errorf("expected RegisterAck, got 0x%02X", byte(ack.Type))
	}

	var resp struct {
		Accepted bool   `json:"accepted"`
		Message  string `json:"message"`
	}
	json.Unmarshal(ack.Payload, &resp)
	if !resp.Accepted {
		return fmt.Errorf("registration rejected: %s", resp.Message)
	}
	return nil
}

func (c *Client) requestProxies(conn net.Conn) error {
	type proxyItem struct {
		Name       string `json:"name"`
		LocalIP    string `json:"localIP"`
		LocalPort  int    `json:"localPort"`
		RemotePort int    `json:"remotePort"`
	}
	var items []proxyItem
	for _, p := range c.proxies {
		ip := p.LocalIP
		if ip == "" {
			ip = "127.0.0.1"
		}
		items = append(items, proxyItem{
			Name: p.Name, LocalIP: ip,
			LocalPort: p.LocalPort, RemotePort: p.RemotePort,
		})
	}
	payload, _ := json.Marshal(map[string]interface{}{"proxies": items})
	msg := &protocol.Message{Type: protocol.TypeProxyRequest, Payload: payload}
	if err := protocol.WriteMessage(conn, msg); err != nil {
		return fmt.Errorf("send ProxyRequest: %w", err)
	}

	resp, err := protocol.ReadMessage(conn)
	if err != nil {
		return fmt.Errorf("read ProxyResponse: %w", err)
	}
	if resp.Type != protocol.TypeProxyResponse {
		return fmt.Errorf("expected ProxyResponse, got 0x%02X", byte(resp.Type))
	}

	var result struct {
		Results []struct {
			Name       string `json:"name"`
			Success    bool   `json:"success"`
			RemotePort int    `json:"remotePort"`
			Error      string `json:"error,omitempty"`
		} `json:"results"`
	}
	json.Unmarshal(resp.Payload, &result)
	var failed []string
	for _, r := range result.Results {
		if r.Success {
			slog.Info("proxy active", "name", r.Name, "port", r.RemotePort)
		} else {
			slog.Warn("proxy failed", "name", r.Name, "error", r.Error)
			failed = append(failed, r.Name)
		}
	}
	if len(failed) > 0 {
		return fmt.Errorf("proxy(s) rejected: %v", failed)
	}
	return nil
}

func (c *Client) messageLoop(conn net.Conn) error {
	for {
		// Set read deadline so we don't block forever on a dead connection.
		// If no message arrives within 3 heartbeat intervals, assume the
		// connection is dead and trigger a reconnect.
		timeout := c.cfg.HeartbeatInterval() * 3
		conn.SetReadDeadline(time.Now().Add(timeout))

		msg, err := protocol.ReadMessage(conn)
		if err != nil {
			return fmt.Errorf("read message: %w", err)
		}

		switch msg.Type {
		case protocol.TypeTunnelOpen:
			var to struct {
				DataConnID string `json:"dataConnId"`
				ProxyName  string `json:"proxyName"`
			}
			if err := json.Unmarshal(msg.Payload, &to); err != nil {
				slog.Warn("invalid TunnelOpen payload", "error", err)
				continue
			}

			// Find proxy rule by name
			var proxy *config.ProxyRule
			for i, p := range c.proxies {
				if p.Name == to.ProxyName {
					proxy = &c.proxies[i]
					break
				}
			}
			if proxy == nil {
				slog.Warn("unknown proxy name", "name", to.ProxyName)
				continue
			}

			ip := proxy.LocalIP
			if ip == "" {
				ip = "127.0.0.1"
			}

			// Create a cancellable context for this tunnel
			ctx, cancel := context.WithCancel(context.Background())

			// Track the tunnel so it can be cleaned up on disconnect
			c.tunnelsMu.Lock()
			// Cancel any existing tunnel with the same ID (shouldn't happen)
			if oldCancel, exists := c.tunnels[to.DataConnID]; exists {
				oldCancel()
			}
			c.tunnels[to.DataConnID] = cancel
			c.tunnelsMu.Unlock()

			go func() {
				c.dataMgr.StartTunnel(ctx, to.DataConnID, to.ProxyName, ip, proxy.LocalPort)
				// Tunnel finished; remove from tracking
				c.tunnelsMu.Lock()
				delete(c.tunnels, to.DataConnID)
				c.tunnelsMu.Unlock()
			}()

		case protocol.TypeHeartbeatAck:
			// Heartbeat acknowledged; reset timeout counter (implicit)

		case protocol.TypeError:
			slog.Warn("server error", "payload", string(msg.Payload))

		default:
			slog.Debug("unhandled message", "type", msg.Type)
		}
	}
}

// closeAllTunnels cancels all active tunnel contexts, which closes
// their data and local connections.
func (c *Client) closeAllTunnels() {
	c.tunnelsMu.Lock()
	defer c.tunnelsMu.Unlock()
	for id, cancel := range c.tunnels {
		cancel()
		delete(c.tunnels, id)
	}
	slog.Debug("all tunnels closed")
}

// requestRProxies sends RProxyRequest for all configured rproxy rules.
func (c *Client) requestRProxies(conn net.Conn) error {
	type rproxyItem struct {
		Name       string `json:"name"`
		LocalPort  int    `json:"localPort"`
		RemoteIP   string `json:"remoteIP"`
		RemotePort int    `json:"remotePort"`
	}
	var items []rproxyItem
	for _, r := range c.cfg.RProxies {
		items = append(items, rproxyItem{
			Name: r.Name, LocalPort: r.LocalPort,
			RemoteIP: r.RemoteIP, RemotePort: r.RemotePort,
		})
	}
	payload, _ := json.Marshal(map[string]interface{}{"rproxies": items})
	msg := &protocol.Message{Type: protocol.TypeRProxyRequest, Payload: payload}
	if err := protocol.WriteMessage(conn, msg); err != nil {
		return fmt.Errorf("send RProxyRequest: %w", err)
	}

	resp, err := protocol.ReadMessage(conn)
	if err != nil {
		return fmt.Errorf("read RProxyResponse: %w", err)
	}
	if resp.Type != protocol.TypeRProxyResponse {
		return fmt.Errorf("expected RProxyResponse, got 0x%02X", byte(resp.Type))
	}

	var result struct {
		Results []struct {
			Name    string `json:"name"`
			Success bool   `json:"success"`
			Error   string `json:"error,omitempty"`
		} `json:"results"`
	}
	json.Unmarshal(resp.Payload, &result)
	var failed []string
	for _, r := range result.Results {
		if r.Success {
			slog.Info("rproxy registered", "name", r.Name)
		} else {
			slog.Warn("rproxy failed", "name", r.Name, "error", r.Error)
			failed = append(failed, r.Name)
		}
	}
	if len(failed) > 0 {
		return fmt.Errorf("rproxy(s) rejected: %v", failed)
	}
	return nil
}

// startRProxyListeners starts local TCP listeners for each rproxy rule.
func (c *Client) startRProxyListeners(conn net.Conn) error {
	for _, r := range c.cfg.RProxies {
		rp, err := NewRProxyListener(r.Name, r.LocalPort, r.RemoteIP, r.RemotePort,
			c.cfg.ServerAddr, c.cfg.ServerPort, c.encKey)
		if err != nil {
			return fmt.Errorf("start rproxy listener %s: %w", r.Name, err)
		}
		c.rproxyListeners = append(c.rproxyListeners, rp)
	}
	return nil
}

// closeAllRProxy stops all rproxy listeners.
func (c *Client) closeAllRProxy() {
	for _, rp := range c.rproxyListeners {
		rp.Close()
	}
	c.rproxyListeners = nil
}

func (c *Client) startHeartbeat(conn net.Conn) {
	c.heartStop = make(chan struct{})
	c.heartWg.Add(1)
	go func() {
		defer c.heartWg.Done()
		ticker := time.NewTicker(c.cfg.HeartbeatInterval())
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				msg := &protocol.Message{Type: protocol.TypeHeartbeat}
				if err := protocol.WriteMessage(conn, msg); err != nil {
					slog.Warn("heartbeat write error", "error", err)
					conn.Close() // unblock messageLoop's ReadMessage
					return
				}
			case <-c.heartStop:
				return
			}
		}
	}()
}

func (c *Client) stopHeartbeat() {
	if c.heartStop != nil {
		close(c.heartStop)
		c.heartStop = nil
	}
	c.heartWg.Wait()
}

func (c *Client) reconnectLoop() bool {
	delay := c.cfg.ReconnectBaseDelay()
	maxDelay := c.cfg.ReconnectMaxDelay()

	for {
		// Check if the client has been stopped
		c.mu.Lock()
		if !c.active {
			c.mu.Unlock()
			return false
		}
		c.mu.Unlock()

		slog.Info("reconnecting", "delay", delay)
		time.Sleep(delay)

		// Try to connect (just to see if server is reachable)
		addr := net.JoinHostPort(c.cfg.ServerAddr, strconv.Itoa(c.cfg.ServerPort))
		testConn, err := net.DialTimeout("tcp", addr, 5*time.Second)
		if err != nil {
			delay = time.Duration(float64(delay) * 1.5)
			if delay > maxDelay {
				delay = maxDelay
			}
			continue
		}
		testConn.Close()

		// Server reachable — break out to re-enter connectAndServe
		return true
	}
}
