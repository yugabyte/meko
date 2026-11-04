// Package auth holds Meko's two credentials. The admin token is the CLI's: serve makes a new one
// at each start and writes it to run/admin.token (0600), so only the OS user who runs Meko can
// read it. The client token is what MCP and REST clients send, kept across restarts (client.go).
package auth

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"fmt"
	"os"
	"strings"

	"github.com/yugabyte/meko/src/meko/internal/paths"
)

// NewToken returns 32 random bytes, hex-encoded.
func NewToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// WriteToken saves token to path, readable only by this user.
func WriteToken(path, token string) error {
	return paths.WriteFile(path, []byte(token+"\n"))
}

// ReadToken reads the token serve wrote.
func ReadToken(path string) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("can't read the admin token: %w", err)
	}
	return strings.TrimSpace(string(b)), nil
}

// CheckBearer reports whether an Authorization header carries token, in constant time.
func CheckBearer(header, token string) bool {
	got, ok := strings.CutPrefix(header, "Bearer ")
	if !ok || token == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(strings.TrimSpace(got)), []byte(token)) == 1
}
