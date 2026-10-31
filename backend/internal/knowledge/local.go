package knowledge

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// LocalFile is a file on this machine checked for an upload: open, with its name, size, SHA-256 and
// first bytes. The caller closes File; it's read to the end.
type LocalFile struct {
	File *os.File
	Name string
	Size int64
	Hash string
	Head []byte
}

// OpenLocal opens a file on this machine to upload it, as the server's user: an absolute path to a
// regular file of a type Meko takes, within the size limit. Its errors wrap ErrInvalid but for a
// read failing partway.
func OpenLocal(path string) (*LocalFile, error) {
	if !filepath.IsAbs(path) {
		return nil, fmt.Errorf("%w: path must be absolute", ErrInvalid)
	}
	name := filepath.Base(path)
	if _, err := TypeOf(name); err != nil {
		return nil, err
	}
	// Stat first: opening a FIFO or a device would block before the check could run.
	fi, err := os.Stat(path)
	if err != nil {
		return nil, fmt.Errorf("%w: can't read %s: %v", ErrInvalid, path, errors.Unwrap(err))
	}
	if !fi.Mode().IsRegular() {
		return nil, fmt.Errorf("%w: %s isn't a regular file", ErrInvalid, path)
	}
	f, err := os.Open(path) //nolint:gosec // G304: reading the caller's own file is the point
	if err != nil {
		return nil, fmt.Errorf("%w: can't open %s: %v", ErrInvalid, path, errors.Unwrap(err))
	}
	lf, err := checkLocal(f, name)
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	return lf, nil
}

func checkLocal(f *os.File, name string) (*LocalFile, error) {
	fi, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !fi.Mode().IsRegular() { // replaced since the first check
		return nil, fmt.Errorf("%w: %s isn't a regular file", ErrInvalid, f.Name())
	}
	if err = CheckSize(fi.Size()); err != nil {
		return nil, err
	}
	head := make([]byte, min(fi.Size(), SniffBytes))
	if _, err = io.ReadFull(f, head); err != nil {
		return nil, err
	}
	h := sha256.New()
	h.Write(head)
	if _, err = io.Copy(h, f); err != nil {
		return nil, err
	}
	return &LocalFile{File: f, Name: name, Size: fi.Size(), Hash: hex.EncodeToString(h.Sum(nil)), Head: head}, nil
}
