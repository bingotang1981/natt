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

	// MaxPayloadSize caps the plaintext payload of a single protocol frame
	// (and, plus framing overhead, of a single encrypted packet on the wire).
	// It defends against memory exhaustion from malicious peers declaring
	// huge lengths (up to ~4 GiB) in the length prefix. Real traffic is far
	// smaller: data packets are bounded by io.Copy's 32 KiB buffer and
	// control messages are a few KiB at most.
	MaxPayloadSize = 4 << 20 // 4 MiB
)
