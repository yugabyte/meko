package update

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"crypto/x509"
	"embed"
	"encoding/base64"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// SumsSig is a release's signature of its SHA256SUMS: one line for each key that signed it,
// "<key id> <base64 Ed25519 signature>". The release workflows write it with openssl, with the key
// only a run from main can use, and sign with two keys while one replaces the other.
const SumsSig = "SHA256SUMS.sig"

// maxSig bounds a SumsSig: a line is about 106 bytes.
const maxSig = 64 << 10

var (
	// errBadSignature is a SumsSig that's malformed, or has a line by a key Meko knows that doesn't
	// verify: always refused.
	errBadSignature = errors.New("the signature doesn't verify")
	// errUnknownKey is a SumsSig whose lines are all by keys Meko doesn't know, such as one made
	// after a key change this Meko is too old for: treated like no signature.
	errUnknownKey = errors.New("signed with a key this Meko doesn't know")
)

//go:embed keys/*.pub
var keyFiles embed.FS

// releaseKeys are the public keys a release's SumsSig is trusted from: keys/*.pub.
var releaseKeys = func() []ed25519.PublicKey {
	keys, err := parseKeys(keyFiles)
	if err != nil {
		panic(err)
	}
	return keys
}()

// parseKeys reads every keys/*.pub in fsys: PEM Ed25519 public keys.
func parseKeys(fsys fs.FS) ([]ed25519.PublicKey, error) {
	names, err := fs.Glob(fsys, "keys/*.pub")
	if err != nil {
		return nil, err
	}
	if len(names) == 0 {
		return nil, errors.New("no release keys in keys/")
	}
	keys := make([]ed25519.PublicKey, 0, len(names))
	for _, name := range names {
		data, err := fs.ReadFile(fsys, name)
		if err != nil {
			return nil, err
		}
		block, _ := pem.Decode(data)
		if block == nil || block.Type != "PUBLIC KEY" {
			return nil, fmt.Errorf("%s isn't a PEM public key", name)
		}
		pub, err := x509.ParsePKIXPublicKey(block.Bytes)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", name, err)
		}
		k, ok := pub.(ed25519.PublicKey)
		if !ok {
			return nil, fmt.Errorf("%s isn't an Ed25519 key", name)
		}
		keys = append(keys, k)
	}
	return keys, nil
}

// KeyID names a public key in a SumsSig: the first 8 bytes, in hex, of the sha256 of its 32 bytes.
func KeyID(pub ed25519.PublicKey) string {
	h := sha256.Sum256(pub)
	return hex.EncodeToString(h[:8])
}

// VerifySumsSignature checks a SHA256SUMS file against its SumsSig with keys. It's nil when a line
// by one of keys verifies and none fails, errUnknownKey when no line is by one of keys, and
// errBadSignature when a line by one of keys doesn't verify or the file is malformed.
func VerifySumsSignature(sumsFile, sigFile string, keys []ed25519.PublicKey) error {
	sums, err := os.ReadFile(sumsFile) //nolint:gosec // G304: a file we downloaded
	if err != nil {
		return err
	}
	fi, err := os.Stat(sigFile)
	if err != nil {
		return err
	}
	if fi.Size() > maxSig {
		return errBadSignature
	}
	sig, err := os.ReadFile(sigFile) //nolint:gosec // G304: a file we downloaded
	if err != nil {
		return err
	}
	known := make(map[string]ed25519.PublicKey, len(keys))
	for _, k := range keys {
		known[KeyID(k)] = k
	}
	lines, verified := 0, false
	for line := range strings.SplitSeq(string(sig), "\n") {
		if line == "" {
			continue
		}
		lines++
		id, b64, ok := strings.Cut(line, " ")
		if !ok || !isKeyID(id) {
			return errBadSignature
		}
		s, err := base64.StdEncoding.DecodeString(b64)
		if err != nil || len(s) != ed25519.SignatureSize {
			return errBadSignature
		}
		k, ok := known[id]
		if !ok {
			continue
		}
		if !ed25519.Verify(k, sums, s) {
			return errBadSignature
		}
		verified = true
	}
	switch {
	case lines == 0:
		return errBadSignature
	case !verified:
		return errUnknownKey
	}
	return nil
}

// isKeyID says whether s is a KeyID: 16 lowercase hex digits.
func isKeyID(s string) bool {
	if len(s) != 16 {
		return false
	}
	for _, r := range s {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return false
		}
	}
	return true
}

// checkSignature checks rel's SHA256SUMS, downloaded to sums, against its SumsSig with the
// client's keys. A signature that doesn't verify is refused. A release with no signature, or one
// signed only with keys Meko doesn't know, is refused unless the client allows unsigned releases;
// then it passes, and the notice for the client's Notice is returned.
func (c *Client) checkSignature(ctx context.Context, rel Release, sums string) (notice string, err error) {
	why, hint := "isn't signed (it has no "+SumsSig+")", ""
	if a, ok := rel.Asset(SumsSig); ok {
		sig := filepath.Join(filepath.Dir(sums), SumsSig)
		if err := c.Download(ctx, a, sig); err != nil {
			return "", err
		}
		keys := c.Keys
		if keys == nil {
			keys = releaseKeys
		}
		err := VerifySumsSignature(sums, sig, keys)
		switch {
		case err == nil:
			return "", nil
		case errors.Is(err, errBadSignature):
			return "", fmt.Errorf("%s's %s doesn't match Meko's release keys, so it won't be installed", rel.Tag, SumsSig)
		case errors.Is(err, errUnknownKey):
			why, hint = "is signed with a key this Meko doesn't know", "install Meko again with its one-line install, or "
		default:
			return "", err
		}
	}
	if !c.AllowUnsigned {
		return "", fmt.Errorf("%s %s: %s[update] require_signature = false installs it anyway", rel.Tag, why, hint)
	}
	return fmt.Sprintf("%s %s: installing it without that check, as [update] require_signature = false says",
		rel.Tag, why), nil
}
