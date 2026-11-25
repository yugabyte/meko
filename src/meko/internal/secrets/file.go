package secrets

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/yugabyte/meko/src/meko/internal/paths"
)

// Files keeps each secret in its own file, secrets/<name> in the data folder: 0600 in a 0700
// folder. A file other users can reach, or that this user doesn't own, is refused rather than
// trusted.
type Files struct{ dir string }

// NewFiles is the file store of the data folder at root.
func NewFiles(root string) *Files { return &Files{dir: filepath.Join(root, "secrets")} }

// Kind is KindFile.
func (f *Files) Kind() string { return KindFile }

// Get reads the secret.
func (f *Files) Get(name string) (string, error) {
	if err := checkName(name); err != nil {
		return "", err
	}
	path := filepath.Join(f.dir, name)
	fi, err := os.Lstat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return "", ErrNotFound
	}
	if err != nil {
		return "", err
	}
	if err = CheckPrivate(path, fi); err != nil {
		return "", err
	}
	b, err := os.ReadFile(path) //nolint:gosec // G304: a file in the data folder's secrets folder
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(b)), nil
}

// Set writes the secret atomically.
func (f *Files) Set(name, value string) error {
	if err := checkName(name); err != nil {
		return err
	}
	if err := checkValue(value); err != nil {
		return err
	}
	if err := os.MkdirAll(f.dir, 0o700); err != nil {
		return err
	}
	if err := os.Chmod(f.dir, 0o700); err != nil {
		return err
	}
	return paths.WriteFile(filepath.Join(f.dir, name), []byte(value+"\n"))
}

// Delete removes the secret.
func (f *Files) Delete(name string) error {
	if err := checkName(name); err != nil {
		return err
	}
	return paths.RemoveIfExists(filepath.Join(f.dir, name))
}

// CheckPrivate refuses a secret's file unless it's a regular file only its owner, this user, can
// read or write.
func CheckPrivate(path string, fi fs.FileInfo) error {
	fix := fmt.Sprintf("Remove %s and set the secret again.", path)
	if !fi.Mode().IsRegular() {
		return fmt.Errorf("%s isn't a regular file. %s", path, fix)
	}
	if fi.Mode().Perm()&0o077 != 0 {
		return fmt.Errorf("%s is open to other users (mode %v): only its owner may read or write it. %s",
			path, fi.Mode().Perm(), fix)
	}
	if st, ok := fi.Sys().(*syscall.Stat_t); ok && int(st.Uid) != os.Getuid() {
		return fmt.Errorf("%s is owned by another user. %s", path, fix)
	}
	return nil
}
