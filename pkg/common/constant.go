package common

import "time"

const (
	// DefaultBindPort is the default server listening port.
	DefaultBindPort = 7000

	// DefaultHeartbeatInterval is the interval between heartbeats.
	DefaultHeartbeatInterval = 45 * time.Second

	// HeartbeatTimeoutMultiplier is how many intervals without ack before timeout.
	HeartbeatTimeoutMultiplier = 3

	// DefaultReconnectBaseDelay is the initial reconnection delay.
	DefaultReconnectBaseDelay = 500 * time.Millisecond

	// DefaultReconnectMaxDelay is the maximum reconnection delay.
	DefaultReconnectMaxDelay = 60 * time.Second

	// DataConnectTimeout is how long the server waits for a DataConnect handshake.
	DataConnectTimeout = 10 * time.Second

	// TunnelCloseGracePeriod is how long to wait for TunnelClose acknowledgement.
	TunnelCloseGracePeriod = 3 * time.Second

	// ShutdownGracePeriod is the overall graceful shutdown timeout.
	ShutdownGracePeriod = 10 * time.Second
)
