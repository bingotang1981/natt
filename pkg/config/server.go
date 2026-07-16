package config

import (
	"encoding/json"
	"fmt"
	"os"
)

// ServerConfig is the configuration for the NAT server.
type ServerConfig struct {
	BindAddr   string                    `json:"bindAddr"`
	BindPort   int                       `json:"bindPort"`
	Token      string                    `json:"token"`
	EncryptKey string                    `json:"encryptKey"`
	LogLevel   string                    `json:"logLevel"`
	LogFile    string                    `json:"logFile"`
	Clients    map[string]ClientRules    `json:"clients"`
}

// DefaultServerConfig returns a ServerConfig with sensible defaults.
func DefaultServerConfig() ServerConfig {
	return ServerConfig{
		BindAddr: "0.0.0.0",
		BindPort: 7000,
		LogLevel: "info",
		Clients:  make(map[string]ClientRules),
	}
}

// LoadServerConfig loads a JSON config file and returns the parsed config.
// If path is empty, it returns the default config.
func LoadServerConfig(path string) (ServerConfig, error) {
	cfg := DefaultServerConfig()
	if path == "" {
		return cfg, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return cfg, fmt.Errorf("read server config %s: %w", path, err)
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		return cfg, fmt.Errorf("parse server config %s: %w", path, err)
	}
	return cfg, cfg.validate()
}

func (c *ServerConfig) validate() error {
	if c.BindPort < 1 || c.BindPort > 65535 {
		return fmt.Errorf("bindPort %d out of range", c.BindPort)
	}
	return nil
}

// ClientRulesFor returns the proxy/rproxy rules for the given clientId.
// Returns an empty ClientRules if the clientId is not found.
func (c *ServerConfig) ClientRulesFor(clientID string) ClientRules {
	if c.Clients != nil {
		if rules, ok := c.Clients[clientID]; ok {
			return rules
		}
	}
	return ClientRules{}
}
