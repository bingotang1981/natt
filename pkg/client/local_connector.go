package client

import (
	"fmt"
	"net"
	"strconv"
	"time"
)

// LocalConnector connects to a local service.
type LocalConnector struct{}

// Connect dials the local service at the given address.
func (lc *LocalConnector) Connect(localIP string, localPort int) (net.Conn, error) {
	addr := net.JoinHostPort(localIP, strconv.Itoa(localPort))
	conn, err := net.DialTimeout("tcp", addr, 5*time.Second)
	if err != nil {
		return nil, fmt.Errorf("connect local %s: %w", addr, err)
	}
	return conn, nil
}
