package stacktest

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
)

var b64 = base64.RawURLEncoding

func split(token string) (head, body, sig string, err error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return "", "", "", fmt.Errorf("token has %d parts, want 3", len(parts))
	}
	return parts[0], parts[1], parts[2], nil
}

func claimsOf(body string) (map[string]any, error) {
	raw, err := b64.DecodeString(body)
	if err != nil {
		return nil, err
	}
	var claims map[string]any
	return claims, json.Unmarshal(raw, &claims)
}

func encode(claims map[string]any) (string, error) {
	raw, err := json.Marshal(claims)
	return b64.EncodeToString(raw), err
}

// TamperClaims changes a token's claims and keeps its signature, which then no
// longer matches.
func TamperClaims(token string, mutate func(claims map[string]any)) (string, error) {
	head, body, sig, err := split(token)
	if err != nil {
		return "", err
	}
	claims, err := claimsOf(body)
	if err != nil {
		return "", err
	}
	mutate(claims)
	body, err = encode(claims)
	return head + "." + body + "." + sig, err
}

// ForgeJWT re-signs a token's claims, unchanged, with a key the pool has never
// seen, under the original key id. The signature is well formed and wrong.
func ForgeJWT(token string) (string, error) {
	head, body, _, err := split(token)
	if err != nil {
		return "", err
	}
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256([]byte(head + "." + body))
	sig, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, sum[:])
	if err != nil {
		return "", err
	}
	return head + "." + body + "." + b64.EncodeToString(sig), nil
}
