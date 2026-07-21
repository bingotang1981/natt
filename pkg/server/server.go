// Package server implements the NAT traversal server.
package server

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"strconv"
	"sync"
	"time"

	"natt/pkg/common"
	"natt/pkg/config"
	"natt/pkg/crypto"
	"natt/pkg/protocol"
)

// Server is the NAT traversal server.
type Server struct {
	cfg          config.ServerConfig
	encryptKey   []byte
	listener     net.Listener
	registry     *Registry
	proxyManager *ProxyManager
	wg           sync.WaitGroup
	quit         chan struct{}
}

// New creates a new Server.
func New(cfg config.ServerConfig) (*Server, error) {
	encryptKey, err := config.DecryptKey(cfg.EncryptKey)
	if err != nil {
		return nil, fmt.Errorf("encryptKey: %w", err)
	}
	if encryptKey != nil {
		slog.Info("encryption enabled (AES-256-GCM)")
	} else {
		slog.Warn("encryption disabled — data transmitted in plaintext")
	}

	return &Server{
		cfg:          cfg,
		encryptKey:   encryptKey,
		registry:     NewRegistry(),
		proxyManager: NewProxyManager(),
		quit:         make(chan struct{}),
	}, nil
}

// Start starts the server and blocks until Stop is called.
func (s *Server) Start() error {
	addr := net.JoinHostPort(s.cfg.BindAddr, strconv.Itoa(s.cfg.BindPort))
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("listen %s: %w", addr, err)
	}
	s.listener = listener
	slog.Info("server started", "addr", addr)

	// Stale pending cleanup ticker
	go func() {
		ticker := time.NewTicker(5 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				s.proxyManager.CleanupStalePending(common.DataConnectTimeout)
			case <-s.quit:
				return
			}
		}
	}()

	for {
		conn, err := listener.Accept()
		if err != nil {
			select {
			case <-s.quit:
				return nil
			default:
				slog.Error("accept error", "error", err)
				continue
			}
		}
		s.wg.Add(1)
		go s.handleConn(conn)
	}
}

// Stop gracefully shuts down the server.
func (s *Server) Stop() {
	close(s.quit)

	// Stop accepting new connections
	if s.listener != nil {
		s.listener.Close()
	}

	// Clean up all proxy listeners
	s.proxyManager.StopAll()

	// Close all registered client control connections
	for _, clientID := range s.registry.List() {
		info := s.registry.Remove(clientID)
		if info != nil {
			info.ControlConn.Close()
		}
	}
}

// Wait waits for all goroutines to finish.
func (s *Server) Wait() {
	s.wg.Wait()
}

// handleConn dispatches a new connection based on its first message.
func (s *Server) handleConn(conn net.Conn) {
	defer s.wg.Done()

	// Wrap with encryption immediately
	if s.encryptKey != nil {
		cipherConn, err := crypto.NewCipherConn(conn, s.encryptKey)
		if err != nil {
			slog.Warn("wrap cipher", "remote", conn.RemoteAddr(), "error", err)
			conn.Close()
			return
		}
		conn = cipherConn
	}

	// Set a read deadline for the first message
	conn.SetReadDeadline(time.Now().Add(10 * time.Second))

	msg, err := protocol.ReadMessage(conn)
	if err != nil {
		slog.Warn("read first message", "remote", conn.RemoteAddr(), "error", err)
		conn.Close()
		return
	}

	// Clear read deadline for long-lived connections
	conn.SetReadDeadline(time.Time{})

	switch msg.Type {
	case protocol.TypeRegister:
		// Control connections are long-lived; defer close at function return.
		defer conn.Close()
		s.handleControlConn(conn, msg)
	case protocol.TypeDataConnect:
		// Data connections are handed off to the bridge goroutines;
		// they manage their own lifecycle. Do NOT close here.
		s.handleDataConn(conn, msg)
	default:
		conn.Close()
		slog.Warn("unknown first message type", "type", msg.Type, "remote", conn.RemoteAddr())
	}
}

// handleControlConn processes a control connection.
func (s *Server) handleControlConn(conn net.Conn, msg *protocol.Message) {
	// Parse Register payload
	var reg struct {
		ClientID string `json:"clientId"`
		Token    string `json:"token"`
		Version  string `json:"version"`
	}
	if err := json.Unmarshal(msg.Payload, &reg); err != nil {
		slog.Warn("invalid Register payload", "error", err)
		return
	}

	// Token authentication
	if s.cfg.Token != "" && reg.Token != s.cfg.Token {
		slog.Warn("auth failed", "clientId", reg.ClientID, "remote", conn.RemoteAddr())
		errPayload, _ := json.Marshal(map[string]string{
			"code":    "AUTH_FAILED",
			"message": "token mismatch",
		})
		protocol.WriteMessage(conn, &protocol.Message{Type: protocol.TypeError, Payload: errPayload})
		return
	}

	// Generate client ID if not provided
	if reg.ClientID == "" {
		reg.ClientID = fmt.Sprintf("client-%d", time.Now().UnixNano())
	}

	// Send RegisterAck
	ackPayload, _ := json.Marshal(map[string]interface{}{
		"clientId": reg.ClientID,
		"accepted": true,
		"message":  "registered successfully",
	})
	ack := &protocol.Message{Type: protocol.TypeRegisterAck, Payload: ackPayload}
	if err := protocol.WriteMessage(conn, ack); err != nil {
		slog.Warn("write RegisterAck", "clientId", reg.ClientID, "error", err)
		return
	}

	// Register the client
	info := &ClientInfo{
		ClientID:    reg.ClientID,
		ControlConn: conn,
		Token:       reg.Token,
	}

	// Evict any existing connection with the same clientId to prevent
	// resource conflicts (port already in use, rproxy already exists, etc.)
	if oldInfo, exists := s.registry.Get(reg.ClientID); exists {
		s.registry.Remove(reg.ClientID)
		s.proxyManager.StopClientProxies(reg.ClientID)
		oldInfo.ControlConn.Close()
		slog.Info("evicted old connection", "clientId", reg.ClientID)
	}

	s.registry.Register(info)
	slog.Info("client registered", "clientId", reg.ClientID, "remote", conn.RemoteAddr())

	defer func() {
		s.registry.Remove(reg.ClientID)
		s.proxyManager.StopClientProxies(reg.ClientID) // clean up proxy ports
		slog.Info("client disconnected", "clientId", reg.ClientID)
	}()

	// Control message loop
	lastActivity := time.Now()
	for {
		conn.SetReadDeadline(time.Now().Add(common.HeartbeatTimeoutMultiplier * common.DefaultHeartbeatInterval))
		msg, err := protocol.ReadMessage(conn)
		if err != nil {
			slog.Info("control connection closed", "clientId", reg.ClientID, "error", err)
			return
		}
		lastActivity = time.Now()

		switch msg.Type {
		case protocol.TypeHeartbeat:
			// Reply with HeartbeatAck
			protocol.WriteMessage(conn, &protocol.Message{Type: protocol.TypeHeartbeatAck})
			_ = lastActivity

		case protocol.TypeProxyRequest:
			var req struct {
				Proxies []struct {
					Name       string `json:"name"`
					LocalIP    string `json:"localIP"`
					LocalPort  int    `json:"localPort"`
					RemotePort int    `json:"remotePort"`
				} `json:"proxies"`
			}
			if err := json.Unmarshal(msg.Payload, &req); err != nil {
				slog.Warn("invalid ProxyRequest", "clientId", reg.ClientID, "error", err)
				continue
			}

			type proxyResult struct {
				Name       string `json:"name"`
				Success    bool   `json:"success"`
				RemotePort int    `json:"remotePort"`
				Error      string `json:"error,omitempty"`
			}
			var results []proxyResult

			for _, p := range req.Proxies {
				ps := ProxyState{
					Name:       p.Name,
					LocalIP:    p.LocalIP,
					LocalPort:  p.LocalPort,
					RemotePort: p.RemotePort,
				}
				actualPort, err := s.proxyManager.StartProxy(ps, conn, reg.ClientID)
				if err != nil {
					slog.Warn("start proxy failed", "name", p.Name, "error", err)
					results = append(results, proxyResult{
						Name: p.Name, Success: false, RemotePort: p.RemotePort, Error: err.Error(),
					})
				} else {
					slog.Info("proxy started", "name", p.Name, "port", actualPort)
					results = append(results, proxyResult{
						Name: p.Name, Success: true, RemotePort: actualPort,
					})
				}
			}

			respPayload, _ := json.Marshal(map[string]interface{}{"results": results})
			protocol.WriteMessage(conn, &protocol.Message{
				Type: protocol.TypeProxyResponse, Payload: respPayload,
			})

		case protocol.TypeConfigQuery:
			slog.Info("config query from client", "clientId", reg.ClientID)
			rules := s.cfg.ClientRulesFor(reg.ClientID)

			// Start proxy listeners on the server side
			type configProxyResult struct {
				Name       string `json:"name"`
				LocalIP    string `json:"localIP"`
				LocalPort  int    `json:"localPort"`
				RemotePort int    `json:"remotePort"`
				Success    bool   `json:"success"`
				Error      string `json:"error,omitempty"`
			}
			var proxyResults []configProxyResult
			for _, p := range rules.Proxies {
				ps := ProxyState{
					Name:       p.Name,
					LocalIP:    p.LocalIP,
					LocalPort:  p.LocalPort,
					RemotePort: p.RemotePort,
				}
				actualPort, err := s.proxyManager.StartProxy(ps, conn, reg.ClientID)
				if err != nil {
					slog.Warn("start proxy failed", "name", p.Name, "error", err)
					proxyResults = append(proxyResults, configProxyResult{
						Name: p.Name, LocalIP: p.LocalIP, LocalPort: p.LocalPort,
						RemotePort: p.RemotePort, Success: false, Error: err.Error(),
					})
				} else {
					slog.Info("proxy started", "name", p.Name, "port", actualPort)
					proxyResults = append(proxyResults, configProxyResult{
						Name: p.Name, LocalIP: p.LocalIP, LocalPort: p.LocalPort,
						RemotePort: actualPort, Success: true,
					})
				}
			}

			// Register rproxy mappings on the server side
			type configRProxyResult struct {
				Name       string `json:"name"`
				LocalPort  int    `json:"localPort"`
				RemoteIP   string `json:"remoteIP"`
				RemotePort int    `json:"remotePort"`
				Success    bool   `json:"success"`
				Error      string `json:"error,omitempty"`
			}
			var rproxyResults []configRProxyResult
			for _, r := range rules.RProxies {
				rs := RProxyState{
					Name:       r.Name,
					RemoteIP:   r.RemoteIP,
					RemotePort: r.RemotePort,
					ClientID:   reg.ClientID,
				}
				if err := s.proxyManager.StartRProxy(rs); err != nil {
					slog.Warn("start rproxy failed", "name", r.Name, "error", err)
					rproxyResults = append(rproxyResults, configRProxyResult{
						Name: r.Name, LocalPort: r.LocalPort,
						RemoteIP: r.RemoteIP, RemotePort: r.RemotePort,
						Success: false, Error: err.Error(),
					})
				} else {
					slog.Info("rproxy registered", "name", r.Name)
					rproxyResults = append(rproxyResults, configRProxyResult{
						Name: r.Name, LocalPort: r.LocalPort,
						RemoteIP: r.RemoteIP, RemotePort: r.RemotePort,
						Success: true,
					})
				}
			}

			respPayload, _ := json.Marshal(map[string]interface{}{
				"proxies":  proxyResults,
				"rproxies": rproxyResults,
			})
			protocol.WriteMessage(conn, &protocol.Message{
				Type: protocol.TypeConfigResponse, Payload: respPayload,
			})

		case protocol.TypeRProxyRequest:
			var rpReq struct {
				RProxies []struct {
					Name       string `json:"name"`
					LocalPort  int    `json:"localPort"`
					RemoteIP   string `json:"remoteIP"`
					RemotePort int    `json:"remotePort"`
				} `json:"rproxies"`
			}
			if err := json.Unmarshal(msg.Payload, &rpReq); err != nil {
				slog.Warn("invalid RProxyRequest", "clientId", reg.ClientID, "error", err)
				continue
			}

			type rproxyResult struct {
				Name    string `json:"name"`
				Success bool   `json:"success"`
				Error   string `json:"error,omitempty"`
			}
			var rpResults []rproxyResult

			for _, r := range rpReq.RProxies {
				rs := RProxyState{
					Name:       r.Name,
					RemoteIP:   r.RemoteIP,
					RemotePort: r.RemotePort,
					ClientID:   reg.ClientID,
				}
				if err := s.proxyManager.StartRProxy(rs); err != nil {
					slog.Warn("start rproxy failed", "name", r.Name, "error", err)
					rpResults = append(rpResults, rproxyResult{Name: r.Name, Success: false, Error: err.Error()})
				} else {
					rpResults = append(rpResults, rproxyResult{Name: r.Name, Success: true})
				}
			}

			rpRespPayload, _ := json.Marshal(map[string]interface{}{"results": rpResults})
			protocol.WriteMessage(conn, &protocol.Message{
				Type: protocol.TypeRProxyResponse, Payload: rpRespPayload,
			})

		case protocol.TypeTunnelClose:
			var tc struct {
				DataConnID string `json:"dataConnId"`
			}
			json.Unmarshal(msg.Payload, &tc)
			slog.Debug("tunnel close from client", "dataConnId", tc.DataConnID)

		case protocol.TypeError:
			slog.Warn("error from client", "clientId", reg.ClientID, "payload", string(msg.Payload))

		default:
			slog.Warn("unknown message type", "type", msg.Type, "clientId", reg.ClientID)
		}
	}
}

// handleDataConn processes an incoming data connection (DataConnect message).
func (s *Server) handleDataConn(conn net.Conn, msg *protocol.Message) {
	var dc struct {
		DataConnID string `json:"dataConnId"`
		ClientID   string `json:"clientId"`
		Mode       string `json:"mode"` // "proxy" (default) or "rproxy"
		RProxyName string `json:"rproxyName"`
	}
	if err := json.Unmarshal(msg.Payload, &dc); err != nil {
		slog.Warn("invalid DataConnect payload", "error", err)
		return
	}

	// rproxy mode: server dials remoteIP:remotePort and bridges
	if dc.Mode == "rproxy" || dc.RProxyName != "" {
		rp := s.proxyManager.GetRProxy(dc.RProxyName)
		if rp == nil {
			slog.Warn("rproxy not found", "name", dc.RProxyName)
			return
		}
		remoteAddr := net.JoinHostPort(rp.RemoteIP, strconv.Itoa(rp.RemotePort))
		remoteConn, err := net.DialTimeout("tcp", remoteAddr, 10*time.Second)
		if err != nil {
			slog.Warn("dial remote failed", "rproxy", dc.RProxyName, "remote", remoteAddr, "error", err)
			return
		}
		slog.Debug("rproxy connected", "name", dc.RProxyName, "remote", remoteAddr)

		// Bridge data conn ↔ remote conn
		go func() {
			var wg sync.WaitGroup
			wg.Add(2)
			go func() {
				defer wg.Done()
				io.Copy(remoteConn, conn)
			}()
			go func() {
				defer wg.Done()
				io.Copy(conn, remoteConn)
			}()
			wg.Wait()
			conn.Close()
			remoteConn.Close()
			slog.Debug("rproxy bridge closed", "name", dc.RProxyName)
		}()
		return
	}

	// proxy mode (default): pair with waiting external connection
	if !s.proxyManager.PairDataConn(dc.DataConnID, conn) {
		slog.Warn("no pending connection for dataConnId", "dataConnId", dc.DataConnID)
	}
}
