// Package config provides configuration loading for server and client.
// Config files use JSON format to avoid external dependencies.
package config

import (
	"encoding/hex"
	"fmt"
)

// ProxyRule defines a single port mapping rule (proxy mode).
// External users connect to server:remotePort, traffic is forwarded to client:localPort.
type ProxyRule struct {
	Name       string `json:"name"`
	LocalIP    string `json:"localIP"`
	LocalPort  int    `json:"localPort"`
	RemotePort int    `json:"remotePort"`
}

// RProxyRule defines a single reverse proxy rule (rproxy mode).
// Local apps connect to client:localPort, traffic is forwarded to server:remoteIP:remotePort.
type RProxyRule struct {
	Name       string `json:"name"`
	LocalPort  int    `json:"localPort"`
	RemoteIP   string `json:"remoteIP"`
	RemotePort int    `json:"remotePort"`
}

// DecryptKey decodes a hex-encoded AES-256 key. Returns nil if the key is empty.
func DecryptKey(hexKey string) ([]byte, error) {
	if hexKey == "" {
		return nil, nil
	}
	key, err := hex.DecodeString(hexKey)
	if err != nil {
		return nil, fmt.Errorf("decode encryptKey: %w", err)
	}
	if len(key) != 32 {
		return nil, fmt.Errorf("encryptKey must decode to 32 bytes, got %d", len(key))
	}
	return key, nil
}
