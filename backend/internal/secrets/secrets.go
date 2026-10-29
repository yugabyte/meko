// Package secrets keeps Meko's secrets: the client token and the Meko Cloud link's key now, and
// provider keys and connector tokens later. On macOS they're in the login Keychain; elsewhere (and
// on a Mac set to [secrets] store = "file") they're files in the data folder, readable only by
// their owner.
//
// A secret never reaches a log or an error message, and reaches a command line only where an
// installer takes it as a flag: meko extension install passes the client token to AgentK's
// installer as --meko-key, as REVIEW.md allows.
package secrets

import (
	"errors"
	"fmt"
	"regexp"
	"runtime"
)

// ErrNotFound means the store holds no secret by that name.
var ErrNotFound = errors.New("no such secret")

// Store kinds.
const (
	KindKeychain = "keychain"
	KindFile     = "file"
)

// Store holds named secrets.
type Store interface {
	// Get returns the secret, or ErrNotFound.
	Get(name string) (string, error)
	// Set stores the secret, replacing any it had.
	Set(name, value string) error
	// Delete removes the secret; a missing one isn't an error.
	Delete(name string) error
	// Kind is KindKeychain or KindFile.
	Kind() string
}

// DefaultKind is the store an install uses unless config names one: the Keychain on macOS, files
// elsewhere.
func DefaultKind() string {
	if runtime.GOOS == "darwin" {
		return KindKeychain
	}
	return KindFile
}

// Open returns the store of the given kind ("" for DefaultKind) for the data folder at root. Each
// data folder has its own secrets, in the Keychain too.
func Open(kind, root string) (Store, error) {
	if kind == "" {
		kind = DefaultKind()
	}
	switch kind {
	case KindFile:
		return NewFiles(root), nil
	case KindKeychain:
		if runtime.GOOS != "darwin" && securityPath == defaultSecurity {
			return nil, fmt.Errorf("the Keychain is only on macOS: set [secrets] store = %q", KindFile)
		}
		return NewKeychain(root), nil
	}
	return nil, fmt.Errorf("secrets.store %q isn't one of %q, %q", kind, KindKeychain, KindFile)
}

var validName = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)

func checkName(name string) error {
	if !validName.MatchString(name) {
		return fmt.Errorf("bad secret name %q", name)
	}
	return nil
}

// A value is printable ASCII with no spaces, quotes or backslashes, which every token and API key
// Meko keeps is; that keeps it safe to pass to the Keychain's command reader.
var validValue = regexp.MustCompile(`^[!#-\[\]-~]+$`)

func checkValue(value string) error {
	if len(value) > 4096 || !validValue.MatchString(value) {
		return errors.New("a secret must be 1 to 4096 printable characters with no spaces, quotes or backslashes")
	}
	return nil
}
