package client

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"strconv"
	"sync"
	"time"

	"natt/pkg/crypto"
	"natt/pkg/protocol"
)

// DataConnManager manages data connections for tunnels.
type DataConnManager struct {
	serverAddr string
	serverPort int
	encryptKey []byte
}

// NewDataConnManager creates a new DataConnManager.
func NewDataConnManager(serverAddr string, serverPort int, encryptKey []byte) *DataConnManager {
	return &DataConnManager{
		serverAddr: serverAddr,
		serverPort: serverPort,
		encryptKey: encryptKey,
	}
}

// StartTunnel establishes a new data connection to the server, sends DataConnect,
// connects to the local service, and bridges both connections.
// The context can be used to cancel the tunnel (closes both connections).
func (dm *DataConnManager) StartTunnel(ctx context.Context, dataConnID, proxyName, localIP string, localPort int) {
	addr := net.JoinHostPort(dm.serverAddr, strconv.Itoa(dm.serverPort))
	dataConn, err := net.DialTimeout("tcp", addr, 10*time.Second)
	if err != nil {
		slog.Warn("dial data conn failed",
			"dataConnId", dataConnID,
			"proxy", proxyName,
			"error", err,
		)
		return
	}

	// Monitor context cancellation as early as possible so an early cancel
	// (e.g. control connection dropped mid-dial) still closes the data conn.
	done := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			dataConn.Close()
		case <-done:
		}
	}()

	// Wrap with encryption
	if dm.encryptKey != nil {
		cipherConn, cerr := crypto.NewCipherConn(dataConn, dm.encryptKey)
		if cerr != nil {
			slog.Warn("wrap data conn cipher", "dataConnId", dataConnID, "error", cerr)
			dataConn.Close()
			close(done)
			return
		}
		dataConn = cipherConn
	}

	// Send DataConnect handshake
	payload, _ := json.Marshal(map[string]string{
		"dataConnId": dataConnID,
	})
	msg := &protocol.Message{Type: protocol.TypeDataConnect, Payload: payload}
	if err := protocol.WriteMessage(dataConn, msg); err != nil {
		slog.Warn("write DataConnect failed", "dataConnId", dataConnID, "error", err)
		dataConn.Close()
		close(done)
		return
	}

	// If the context was already cancelled during the dial, abort now.
	select {
	case <-ctx.Done():
		slog.Warn("tunnel cancelled before local connect", "dataConnId", dataConnID)
		dataConn.Close()
		close(done)
		return
	default:
	}

	// Connect to local service
	localConn, err := (&LocalConnector{}).Connect(localIP, localPort)
	if err != nil {
		slog.Warn("connect local service failed",
			"dataConnId", dataConnID,
			"proxy", proxyName,
			"local", net.JoinHostPort(localIP, strconv.Itoa(localPort)),
			"error", err,
		)
		dataConn.Close()
		close(done)
		return
	}

	slog.Debug("tunnel established",
		"dataConnId", dataConnID,
		"proxy", proxyName,
	)

	// Bridge: data conn ↔ local conn.
	// When either direction ends (peer EOF/error), close both sides so the
	// other direction unblocks immediately instead of lingering forever.
	// close(done) also releases the early-cancel watcher above.
	// StartTunnel blocks here until the tunnel ends: this keeps the caller's
	// context.CancelFunc reachable (via the c.tunnels map) for the whole
	// lifetime of the tunnel, so closeAllTunnels() can tear it down on
	// control-connection loss instead of leaving an orphaned tunnel.
	var closeOnce sync.Once
	closeBoth := func() {
		closeOnce.Do(func() {
			dataConn.Close()
			localConn.Close()
			close(done)
		})
	}

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		defer closeBoth()
		_, err := io.Copy(dataConn, localConn)
		if err != nil {
			slog.Debug("bridge local→data closed", "dataConnId", dataConnID, "error", err)
		}
	}()
	go func() {
		defer wg.Done()
		defer closeBoth()
		_, err := io.Copy(localConn, dataConn)
		if err != nil {
			slog.Debug("bridge data→local closed", "dataConnId", dataConnID, "error", err)
		}
	}()
	wg.Wait()
	slog.Debug("tunnel closed", "dataConnId", dataConnID)
}
