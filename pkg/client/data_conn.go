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

	// Wrap with encryption
	if dm.encryptKey != nil {
		cipherConn, cerr := crypto.NewCipherConn(dataConn, dm.encryptKey)
		if cerr != nil {
			slog.Warn("wrap data conn cipher", "dataConnId", dataConnID, "error", cerr)
			dataConn.Close()
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
		return
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
		return
	}

	slog.Debug("tunnel established",
		"dataConnId", dataConnID,
		"proxy", proxyName,
	)

	// Monitor context cancellation to close the tunnel
	go func() {
		<-ctx.Done()
		dataConn.Close()
		localConn.Close()
	}()

	// Bridge: data conn ↔ local conn
	var wg sync.WaitGroup
	wg.Add(2)

	go func() {
		defer wg.Done()
		_, err := io.Copy(dataConn, localConn)
		if err != nil {
			slog.Debug("bridge local→data closed", "dataConnId", dataConnID, "error", err)
		}
	}()
	go func() {
		defer wg.Done()
		_, err := io.Copy(localConn, dataConn)
		if err != nil {
			slog.Debug("bridge data→local closed", "dataConnId", dataConnID, "error", err)
		}
	}()

	go func() {
		wg.Wait()
		dataConn.Close()
		localConn.Close()
		slog.Debug("tunnel closed", "dataConnId", dataConnID)
	}()
}
