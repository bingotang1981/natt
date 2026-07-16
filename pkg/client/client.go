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
	proxies []config.ProxyRule // received from server via ConfigResponse
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

	// Query config from server (replaces local proxy/rproxy configuration)
	rules, err := c.configQuery(conn)
	if err != nil {
		return err
	}
	// Convert received proxies to config.ProxyRule for tunnel lookup
	c.proxies = make([]config.ProxyRule, 0, len(rules.Proxies))
	for _, p := range rules.Proxies {
		if p.Success {
			c.proxies = append(c.proxies, config.ProxyRule{
				Name:       p.Name,
				LocalIP:    p.LocalIP,
				LocalPort:  p.LocalPort,
				RemotePort: p.RemotePort,
			})
		}
	}
	slog.Info("config received from server",
		"proxies", len(rules.Proxies),
		"rproxies", len(rules.RProxies))

	// Start rproxy local listeners based on server-provided config
	if len(rules.RProxies) > 0 {
		var rproxyRules []config.RProxyRule
		for _, r := range rules.RProxies {
			if r.Success {
				rproxyRules = append(rproxyRules, config.RProxyRule{
					Name:       r.Name,
					LocalPort:  r.LocalPort,
					RemoteIP:   r.RemoteIP,
					RemotePort: r.RemotePort,
				})
			}
		}
		if err := c.startRProxyListeners(conn, rproxyRules); err != nil {
			return err
		}
	}

	// Start heartbeat
	c.startHeartbeat(conn)

	// Enter control message loop
	return c.messageLoop(conn)
}

func (c *Client) register(conn net.Conn) error {
	clientID := c.cfg.ClientID
	if clientID == "" {
		clientID = fmt.Sprintf("client-%d", time.Now().UnixNano())
	}
	payload, _ := json.Marshal(map[string]string{
		"clientId": clientID,
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

func (c *Client) configQuery(conn net.Conn) (*configQueryResult, error) {
	clientID := c.cfg.ClientID
	if clientID == "" {
		clientID = fmt.Sprintf("client-%d", time.Now().UnixNano())
	}
	payload, _ := json.Marshal(map[string]string{
		"clientId": clientID,
	})
	msg := &protocol.Message{Type: protocol.TypeConfigQuery, Payload: payload}
	if err := protocol.WriteMessage(conn, msg); err != nil {
		return nil, fmt.Errorf("send ConfigQuery: %w", err)
	}

	resp, err := protocol.ReadMessage(conn)
	if err != nil {
		return nil, fmt.Errorf("read ConfigResponse: %w", err)
	}
	if resp.Type != protocol.TypeConfigResponse {
		return nil, fmt.Errorf("expected ConfigResponse, got 0x%02X", byte(resp.Type))
	}

	var result configQueryResult
	if err := json.Unmarshal(resp.Payload, &result); err != nil {
		return nil, fmt.Errorf("parse ConfigResponse: %w", err)
	}

	// Log results
	for _, p := range result.Proxies {
		if p.Success {
			slog.Info("proxy active", "name", p.Name, "port", p.RemotePort)
		} else {
			slog.Warn("proxy failed", "name", p.Name, "error", p.Error)
		}
	}
	for _, r := range result.RProxies {
		if r.Success {
			slog.Info("rproxy registered", "name", r.Name, "remote", net.JoinHostPort(r.RemoteIP, strconv.Itoa(r.RemotePort)))
		} else {
			slog.Warn("rproxy failed", "name", r.Name, "error", r.Error)
		}
	}

	return &result, nil
}

// receivedProxy describes a proxy rule received from the server ConfigResponse.
type receivedProxy struct {
	Name       string `json:"name"`
	LocalIP    string `json:"localIP"`
	LocalPort  int    `json:"localPort"`
	RemotePort int    `json:"remotePort"`
	Success    bool   `json:"success"`
	Error      string `json:"error,omitempty"`
}

// receivedRProxy describes an rproxy rule received from the server ConfigResponse.
type receivedRProxy struct {
	Name       string `json:"name"`
	LocalPort  int    `json:"localPort"`
	RemoteIP   string `json:"remoteIP"`
	RemotePort int    `json:"remotePort"`
	Success    bool   `json:"success"`
	Error      string `json:"error,omitempty"`
}

// configQueryResult is the parsed ConfigResponse payload.
type configQueryResult struct {
	Proxies  []receivedProxy  `json:"proxies"`
	RProxies []receivedRProxy `json:"rproxies"`
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

// startRProxyListeners starts local TCP listeners for each rproxy rule.
func (c *Client) startRProxyListeners(conn net.Conn, rproxies []config.RProxyRule) error {
	for _, r := range rproxies {
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
