// Package crypto provides AES-256-GCM encryption for TCP connections.
//
// CipherConn wraps a net.Conn and transparently encrypts all writes
// and decrypts all reads using AES-256-GCM with a pre-shared key.
//
// Wire format (outer layer):
//
//	[4 bytes packet length (BigEndian)][12 bytes nonce][GCM ciphertext+tag]
//
// The inner protocol frame (type + length + payload) is the plaintext
// passed through CipherConn and is never visible on the wire.
package crypto

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"time"
)

const (
	keyLen    = 32 // AES-256 key length
	nonceLen  = 12 // GCM standard nonce length
	tagLen    = 16 // GCM authentication tag length
	headerLen = 4  // length prefix bytes
)

// CipherConn wraps a net.Conn with AES-256-GCM encryption.
// An internal buffer holds decrypted plaintext so that callers
// reading in multiple steps (e.g. header then payload) don't lose data.
type CipherConn struct {
	conn net.Conn
	gcm  cipher.AEAD
	buf  bytes.Buffer // buffered decrypted plaintext
}

// NewCipherConn wraps conn with AES-256-GCM using the provided key.
// The key must be exactly 32 bytes.
func NewCipherConn(conn net.Conn, key []byte) (net.Conn, error) {
	if len(key) != keyLen {
		return nil, errors.New("crypto: key must be exactly 32 bytes")
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return &CipherConn{
		conn: conn,
		gcm:  gcm,
	}, nil
}

// Read reads decrypted plaintext into buf.
// It reads and decrypts one full encrypted packet from the underlying
// connection, buffers the plaintext, then copies into buf.
// Multiple calls to Read will consume from the buffer first.
func (c *CipherConn) Read(buf []byte) (int, error) {
	if c.buf.Len() == 0 {
		if err := c.readNextPacket(); err != nil {
			return 0, err
		}
	}
	return c.buf.Read(buf)
}

// readNextPacket reads one encrypted packet, decrypts it,
// and writes the plaintext into the internal buffer.
func (c *CipherConn) readNextPacket() error {
	// Read the 4-byte length prefix
	var header [headerLen]byte
	if _, err := io.ReadFull(c.conn, header[:]); err != nil {
		return err
	}
	packetLen := binary.BigEndian.Uint32(header[:])
	if packetLen < nonceLen+tagLen {
		return errors.New("crypto: packet too short")
	}

	// Read the full encrypted packet
	packet := make([]byte, packetLen)
	if _, err := io.ReadFull(c.conn, packet); err != nil {
		return err
	}

	nonce := packet[:nonceLen]
	ciphertext := packet[nonceLen:]

	plaintext, err := c.gcm.Open(nil, nonce, ciphertext, nil)
	if err != nil {
		return err // GCM auth failure — key mismatch or corruption
	}

	// Buffer the plaintext for subsequent small reads
	c.buf.Reset()
	_, err = c.buf.Write(plaintext)
	return err
}

// Write encrypts data with a random nonce and writes it as a framed packet.
func (c *CipherConn) Write(data []byte) (int, error) {
	nonce := make([]byte, nonceLen)
	if _, err := rand.Read(nonce); err != nil {
		return 0, err
	}

	ciphertext := c.gcm.Seal(nil, nonce, data, nil)

	frameLen := len(nonce) + len(ciphertext)
	frame := make([]byte, headerLen+frameLen)
	binary.BigEndian.PutUint32(frame[:headerLen], uint32(frameLen))
	copy(frame[headerLen:], nonce)
	copy(frame[headerLen+nonceLen:], ciphertext)

	if _, err := c.conn.Write(frame); err != nil {
		return 0, err
	}
	return len(data), nil
}

// Close closes the underlying connection.
func (c *CipherConn) Close() error { return c.conn.Close() }

// LocalAddr returns the local network address.
func (c *CipherConn) LocalAddr() net.Addr { return c.conn.LocalAddr() }

// RemoteAddr returns the remote network address.
func (c *CipherConn) RemoteAddr() net.Addr { return c.conn.RemoteAddr() }

// SetDeadline sets the read and write deadlines.
func (c *CipherConn) SetDeadline(t time.Time) error { return c.conn.SetDeadline(t) }

// SetReadDeadline sets the read deadline.
func (c *CipherConn) SetReadDeadline(t time.Time) error { return c.conn.SetReadDeadline(t) }

// SetWriteDeadline sets the write deadline.
func (c *CipherConn) SetWriteDeadline(t time.Time) error { return c.conn.SetWriteDeadline(t) }
