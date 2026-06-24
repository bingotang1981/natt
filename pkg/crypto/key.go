package crypto

import (
	"crypto/rand"
	"encoding/hex"
	"io"
)

// GenerateKey generates a cryptographically random 32-byte AES-256 key
// and returns it as a hex-encoded string.
func GenerateKey() (string, error) {
	key := make([]byte, keyLen)
	if _, err := io.ReadFull(rand.Reader, key); err != nil {
		return "", err
	}
	return hex.EncodeToString(key), nil
}
