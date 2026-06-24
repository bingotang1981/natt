package config

import (
	"encoding/json"
	"fmt"
	"os"
	"time"

	"natt/pkg/common"
)

// ClientConfig is the configuration for the NAT client.
type ClientConfig struct {
	ServerAddr string       `json:"serverAddr"`
	ServerPort int          `json:"serverPort"`
	Token      string       `json:"token"`
	EncryptKey string       `json:"encryptKey"`
	LogLevel   string       `json:"logLevel"`
	LogFile    string       `json:"logFile"`
	Proxies    []ProxyRule  `json:"proxies"`
	RProxies   []RProxyRule `json:"rproxies"`

	// Optional overrides with defaults
	HeartbeatIntervalMs  int `json:"heartbeatIntervalMs"`
	ReconnectBaseDelayMs int `json:"reconnectBaseDelayMs"`
	ReconnectMaxDelayMs  int `json:"reconnectMaxDelayMs"`
}

// HeartbeatInterval returns the heartbeat duration or the default.
func (c *ClientConfig) HeartbeatInterval() time.Duration {
	if c.HeartbeatIntervalMs > 0 {
		return time.Duration(c.HeartbeatIntervalMs) * time.Millisecond
	}
	return common.DefaultHeartbeatInterval
}

// ReconnectBaseDelay returns the base reconnection delay or the default.
func (c *ClientConfig) ReconnectBaseDelay() time.Duration {
	if c.ReconnectBaseDelayMs > 0 {
		return time.Duration(c.ReconnectBaseDelayMs) * time.Millisecond
	}
	return common.DefaultReconnectBaseDelay
}

// ReconnectMaxDelay returns the maximum reconnection delay or the default.
func (c *ClientConfig) ReconnectMaxDelay() time.Duration {
	if c.ReconnectMaxDelayMs > 0 {
		return time.Duration(c.ReconnectMaxDelayMs) * time.Millisecond
	}
	return common.DefaultReconnectMaxDelay
}

// DefaultClientConfig returns a ClientConfig with sensible defaults.
func DefaultClientConfig() ClientConfig {
	return ClientConfig{
		ServerPort: common.DefaultBindPort,
		LogLevel:   "info",
	}
}

// LoadClientConfig loads a JSON config file and returns the parsed config.
func LoadClientConfig(path string) (ClientConfig, error) {
	cfg := DefaultClientConfig()
	if path == "" {
		return cfg, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return cfg, fmt.Errorf("read client config %s: %w", path, err)
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		return cfg, fmt.Errorf("parse client config %s: %w", path, err)
	}
	return cfg, cfg.validate()
}

func (c *ClientConfig) validate() error {
	if c.ServerAddr == "" {
		return fmt.Errorf("serverAddr is required")
	}
	if c.ServerPort < 1 || c.ServerPort > 65535 {
		return fmt.Errorf("serverPort %d out of range", c.ServerPort)
	}
	for _, p := range c.Proxies {
		if p.Name == "" {
			return fmt.Errorf("proxy name is required")
		}
		if p.LocalPort < 1 || p.LocalPort > 65535 {
			return fmt.Errorf("proxy %s: localPort %d out of range", p.Name, p.LocalPort)
		}
		if p.RemotePort < 1 || p.RemotePort > 65535 {
			return fmt.Errorf("proxy %s: remotePort %d out of range", p.Name, p.RemotePort)
		}
	}
	for _, r := range c.RProxies {
		if r.Name == "" {
			return fmt.Errorf("rproxy name is required")
		}
		if r.LocalPort < 1 || r.LocalPort > 65535 {
			return fmt.Errorf("rproxy %s: localPort %d out of range", r.Name, r.LocalPort)
		}
		if r.RemoteIP == "" {
			return fmt.Errorf("rproxy %s: remoteIP is required", r.Name)
		}
		if r.RemotePort < 1 || r.RemotePort > 65535 {
			return fmt.Errorf("rproxy %s: remotePort %d out of range", r.Name, r.RemotePort)
		}
	}
	return nil
}
