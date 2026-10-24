package auth

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"sync"

	"golang.org/x/crypto/scrypt"
)

// The owner's login guards the web listener with remote access. Its password hashes have AgentK's
// format, scrypt$N$r$p$salt$hash with standard base64, so an AgentK login Meko imports keeps working.
const (
	scryptN      = 1 << 15
	scryptR      = 8
	scryptP      = 1
	scryptKeyLen = 64
	// MinPasswordLen is the shortest password a login takes.
	MinPasswordLen = 12
)

var usernameRE = regexp.MustCompile(`^[A-Za-z0-9._@-]{1,64}$`)

// CheckUsername says what's wrong with a username, or "" when it's fine.
func CheckUsername(u string) string {
	if !usernameRE.MatchString(u) {
		return "Use up to 64 characters for the username: letters, digits, dots, @, dashes or underscores."
	}
	return ""
}

// CheckPassword says what's wrong with a new password, or "" when it's fine.
func CheckPassword(p string) string {
	if len([]rune(p)) < MinPasswordLen {
		return fmt.Sprintf("Use at least %d characters for the password.", MinPasswordLen)
	}
	return ""
}

// HashPassword hashes a password with a new salt.
func HashPassword(password string) (string, error) {
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	key, err := scrypt.Key([]byte(password), salt, scryptN, scryptR, scryptP, scryptKeyLen)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("scrypt$%d$%d$%d$%s$%s", scryptN, scryptR, scryptP,
		base64.StdEncoding.EncodeToString(salt), base64.StdEncoding.EncodeToString(key)), nil
}

// VerifyPassword reports whether password matches a stored hash, in constant time. A hash it can't
// read, or one with parameters out of reach, never matches.
func VerifyPassword(password, stored string) bool {
	parts := strings.Split(stored, "$")
	if len(parts) != 6 || parts[0] != "scrypt" {
		return false
	}
	n, errN := strconv.Atoi(parts[1])
	r, errR := strconv.Atoi(parts[2])
	p, errP := strconv.Atoi(parts[3])
	salt, errS := base64.StdEncoding.DecodeString(parts[4])
	want, errH := base64.StdEncoding.DecodeString(parts[5])
	// The bounds keep an imported hash from asking for more memory than a login is worth.
	if errN != nil || errR != nil || errP != nil || errS != nil || errH != nil ||
		n < 2 || n > 1<<20 || r < 1 || r > 32 || p < 1 || p > 16 || len(want) < 16 || len(want) > 128 {
		return false
	}
	got, err := scrypt.Key([]byte(password), salt, n, r, p, len(want))
	return err == nil && subtle.ConstantTimeCompare(got, want) == 1
}

var (
	dummyOnce sync.Once
	dummyHash string
)

// BurnPasswordCheck spends the time of checking a real password, so a wrong username can't be told
// from a wrong password by timing.
func BurnPasswordCheck(password string) {
	dummyOnce.Do(func() { dummyHash, _ = HashPassword("not-the-password") })
	VerifyPassword(password, dummyHash)
}
