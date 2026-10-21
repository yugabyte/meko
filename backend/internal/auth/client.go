package auth

import (
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

// The client token is what MCP and REST clients (Claude Code, AgentK, the CLI's own MCP calls)
// send: one per install, made the first time it's needed and kept across restarts, because
// clients store it once. It's the same kind of secret as the admin token, which changes at each
// start and stays the CLI's alone.

// EnsureClientToken returns the client token at path, making it the first time. Making it is
// atomic (a hard link of a complete temp file), so serve and the CLI asking at once agree on one
// token. A token file other users have any access to, or that this user doesn't own, is refused
// rather than trusted.
func EnsureClientToken(path string) (string, error) {
	tok, err := ReadClientToken(path)
	if !errors.Is(err, fs.ErrNotExist) {
		return tok, err
	}
	if tok, err = NewToken(); err != nil {
		return "", err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*")
	if err != nil {
		return "", err
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	if err = tmp.Chmod(0o600); err == nil {
		_, err = tmp.WriteString(tok + "\n")
	}
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return "", err
	}
	// Link fails if the file appeared meanwhile; then the other writer's token is the one.
	if err = os.Link(tmp.Name(), path); err != nil && !errors.Is(err, fs.ErrExist) {
		return "", err
	}
	return ReadClientToken(path)
}

// ReadClientToken reads the client token at path; a missing file is fs.ErrNotExist.
func ReadClientToken(path string) (string, error) {
	fi, err := os.Lstat(path)
	if err != nil {
		return "", err
	}
	fix := fmt.Sprintf("Remove %s; Meko makes a new one at its next start "+
		"(clients set up with the old one need the new one).", path)
	if !fi.Mode().IsRegular() {
		return "", fmt.Errorf("%s isn't a regular file. %s", path, fix)
	}
	if fi.Mode().Perm()&0o077 != 0 {
		return "", fmt.Errorf("%s is open to other users (mode %v): only its owner may read or write it. %s", path,
			fi.Mode().Perm(), fix)
	}
	if st, ok := fi.Sys().(*syscall.Stat_t); ok && int(st.Uid) != os.Getuid() {
		return "", fmt.Errorf("%s is owned by another user. %s", path, fix)
	}
	b, err := os.ReadFile(path) //nolint:gosec // G304: the data folder's own token file
	if err != nil {
		return "", err
	}
	tok := strings.TrimSpace(string(b))
	if raw, err := hex.DecodeString(tok); err != nil || len(raw) != 32 {
		return "", fmt.Errorf("%s doesn't hold a token. %s", path, fix)
	}
	return tok, nil
}
