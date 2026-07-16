package protocol

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
)

// MessageType is the single byte identifying a message.
type MessageType byte

const (
	TypeRegister       MessageType = 0x01
	TypeRegisterAck    MessageType = 0x02
	TypeProxyRequest   MessageType = 0x03
	TypeProxyResponse  MessageType = 0x04
	TypeHeartbeat      MessageType = 0x05
	TypeHeartbeatAck   MessageType = 0x06
	TypeTunnelOpen     MessageType = 0x07
	TypeDataConnect    MessageType = 0x08
	TypeTunnelClose    MessageType = 0x09
	TypeError          MessageType = 0x0A
	TypeRProxyRequest  MessageType = 0x0B
	TypeRProxyResponse MessageType = 0x0C
	TypeConfigQuery    MessageType = 0x0D
	TypeConfigResponse MessageType = 0x0E
)

// Message represents a framed protocol message.
type Message struct {
	Type    MessageType
	Payload []byte // JSON-encoded payload
}

// String returns a human-readable representation.
func (m *Message) String() string {
	return fmt.Sprintf("Message{Type: 0x%02X, Payload: %s}", byte(m.Type), string(m.Payload))
}

// Encode serializes a Message into a byte slice:
// [1 byte type][4 bytes payload length (BigEndian)][payload].
func Encode(msg *Message) []byte {
	length := len(msg.Payload)
	buf := make([]byte, 1+4+length)
	buf[0] = byte(msg.Type)
	buf[1] = byte(length >> 24)
	buf[2] = byte(length >> 16)
	buf[3] = byte(length >> 8)
	buf[4] = byte(length)
	copy(buf[5:], msg.Payload)
	return buf
}

// Decode parses a byte slice into a Message.
func Decode(data []byte) (*Message, error) {
	if len(data) < 5 {
		return nil, fmt.Errorf("protocol: frame too short (%d bytes)", len(data))
	}
	msgType := MessageType(data[0])
	length := int(data[1])<<24 | int(data[2])<<16 | int(data[3])<<8 | int(data[4])
	if 5+length > len(data) {
		return nil, fmt.Errorf("protocol: frame payload truncated (need %d, have %d)", 5+length, len(data))
	}
	return &Message{
		Type:    msgType,
		Payload: data[5 : 5+length],
	}, nil
}

// ReadMessage reads one complete protocol message from a CipherConn-wrapped
// connection. It reads a full encrypted packet through the conn, decrypts it
// (handled transparently by CipherConn), then parses the inner protocol frame.
func ReadMessage(conn net.Conn) (*Message, error) {
	// Read the 5-byte header (type + 4-byte length)
	var header [5]byte
	if _, err := io.ReadFull(conn, header[:]); err != nil {
		return nil, err
	}

	msgType := MessageType(header[0])
	length := int(header[1])<<24 | int(header[2])<<16 | int(header[3])<<8 | int(header[4])

	// Read the payload
	payload := make([]byte, length)
	if length > 0 {
		if _, err := io.ReadFull(conn, payload); err != nil {
			return nil, err
		}
	}

	return &Message{Type: msgType, Payload: payload}, nil
}

// WriteMessage writes a Message to a CipherConn-wrapped connection.
// The message is serialized and then encrypted transparently by CipherConn.
func WriteMessage(conn net.Conn, msg *Message) error {
	data := Encode(msg)
	_, err := conn.Write(data)
	return err
}

// --- JSON payload helpers ---

// ParsePayload unmarshals the JSON payload into the given struct.
func ParsePayload(msg *Message, v interface{}) error {
	if len(msg.Payload) == 0 {
		return nil
	}
	return json.Unmarshal(msg.Payload, v)
}

// JSONPayload marshals v to JSON and returns a Message with the given type.
func JSONPayload(msgType MessageType, v interface{}) (*Message, error) {
	data, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	return &Message{Type: msgType, Payload: data}, nil
}
