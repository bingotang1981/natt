package client

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"strconv"
	"sync"
	"time"

	"natt/pkg/crypto"
	"natt/pkg/protocol"
)

// RProxyListener manages a local port listener for rproxy mode.
// When a local app connects to the localPort, it creates a data connection
// to the server and bridges the two.
type RProxyListener struct {
	name       string
	localPort  int
	remoteIP   string
	remotePort int
	listener   net.Listener
	serverAddr string
	serverPort int
	encKey     []byte
	closeCh    chan struct{}
}

// NewRProxyListener creates and starts an rproxy listener.
func NewRProxyListener(name string, localPort int, remoteIP string, remotePort int,
	serverAddr string, serverPort int, encKey []byte) (*RProxyListener, error) {

	addr := fmt.Sprintf(":%d", localPort)
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("listen rproxy %s on %d: %w", name, localPort, err)
	}

	rp := &RProxyListener{
		name:       name,
		localPort:  localPort,
		remoteIP:   remoteIP,
		remotePort: remotePort,
		listener:   listener,
		serverAddr: serverAddr,
		serverPort: serverPort,
		encKey:     encKey,
		closeCh:    make(chan struct{}),
	}

	go rp.acceptLoop()
	return rp, nil
}

func (rp *RProxyListener) acceptLoop() {
	slog.Info("rproxy listening", "name", rp.name, "port", rp.localPort,
		"remote", net.JoinHostPort(rp.remoteIP, strconv.Itoa(rp.remotePort)))
	for {
		localConn, err := rp.listener.Accept()
		if err != nil {
			select {
			case <-rp.closeCh:
				return
			default:
				slog.Warn("rproxy accept error", "name", rp.name, "error", err)
				return
			}
		}
		go rp.handleConn(localConn)
	}
}

func (rp *RProxyListener) handleConn(localConn net.Conn) {
	defer localConn.Close()

	// Create data connection to server
	addr := net.JoinHostPort(rp.serverAddr, strconv.Itoa(rp.serverPort))
	dataConn, err := net.DialTimeout("tcp", addr, 10*time.Second)
	if err != nil {
		slog.Warn("rproxy dial server failed", "name", rp.name, "error", err)
		return
	}
	defer dataConn.Close()

	// Wrap with encryption
	if rp.encKey != nil {
		cipherConn, cerr := crypto.NewCipherConn(dataConn, rp.encKey)
		if cerr != nil {
			slog.Warn("rproxy wrap cipher", "name", rp.name, "error", cerr)
			return
		}
		dataConn = cipherConn
	}

	// Send DataConnect with rproxy mode
	payload, _ := json.Marshal(map[string]string{
		"mode":       "rproxy",
		"rproxyName": rp.name,
	})
	msg := &protocol.Message{Type: protocol.TypeDataConnect, Payload: payload}
	if err := protocol.WriteMessage(dataConn, msg); err != nil {
		slog.Warn("rproxy write DataConnect", "name", rp.name, "error", err)
		return
	}

	slog.Debug("rproxy tunnel established", "name", rp.name,
		"local", localConn.RemoteAddr())

	// Bridge local conn ↔ data conn
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		io.Copy(dataConn, localConn)
	}()
	go func() {
		defer wg.Done()
		io.Copy(localConn, dataConn)
	}()
	wg.Wait()
	slog.Debug("rproxy tunnel closed", "name", rp.name)
}

// Close stops the rproxy listener.
func (rp *RProxyListener) Close() {
	select {
	case <-rp.closeCh:
		return
	default:
		close(rp.closeCh)
	}
	rp.listener.Close()
	slog.Info("rproxy stopped", "name", rp.name)
}
