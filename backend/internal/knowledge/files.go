package knowledge

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Files is the folder knowledge files' bytes live in, knowledgebase-files/ beside meko.db: one
// file per datapack and content, at <datapack id>/<sha256>. Folders are 0700 and files 0600. A
// file is written to a temporary name in its datapack's folder and renamed into place, so a file
// at its final name is always whole.
type Files struct{ root string }

// NewFiles is the folder at root. It's made (0700) when the first file arrives.
func NewFiles(root string) *Files { return &Files{root: root} }

// tempPrefix starts the names of files still being written.
const tempPrefix = ".upload-"

// ErrTooLarge is a file longer than the size it was received with.
var ErrTooLarge = errors.New("the file is larger than its declared size")

// Root is the folder.
func (f *Files) Root() string { return f.root }

// dir is a datapack's folder. A datapack id is a UUID, so it never holds a path separator; one
// that does is refused rather than let out of the folder.
func (f *Files) dir(datapack string) (string, error) {
	if datapack == "" || strings.ContainsAny(datapack, `/\`) || datapack == "." || datapack == ".." {
		return "", fmt.Errorf("%q isn't a datapack id", datapack)
	}
	return filepath.Join(f.root, datapack), nil
}

// Path is where a datapack's file with this content lives.
func (f *Files) Path(datapack, hash string) (string, error) {
	if !IsHash(hash) {
		return "", fmt.Errorf("%q isn't a SHA-256", hash)
	}
	d, err := f.dir(datapack)
	if err != nil {
		return "", err
	}
	return filepath.Join(d, hash), nil
}

// Received is a file written to a temporary name, waiting to be kept or discarded.
type Received struct {
	Hash string // SHA-256 of its bytes, lowercase hex
	Size int64
	Head []byte // its first SniffBytes bytes
	temp string
}

// Receive writes r to a temporary file in the datapack's folder, hashing it as it goes, and refuses
// (ErrTooLarge, with nothing left behind) one longer than limit bytes.
func (f *Files) Receive(datapack string, r io.Reader, limit int64) (*Received, error) {
	d, err := f.dir(datapack)
	if err != nil {
		return nil, err
	}
	if err = os.MkdirAll(d, 0o700); err != nil {
		return nil, err
	}
	tmp, err := os.CreateTemp(d, tempPrefix+"*")
	if err != nil {
		return nil, err
	}
	rec := &Received{temp: tmp.Name()}
	h := sha256.New()
	head := &headWriter{max: SniffBytes}
	n, err := io.Copy(io.MultiWriter(tmp, h, head), io.LimitReader(r, limit+1))
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err == nil && n > limit {
		err = ErrTooLarge
	}
	if err != nil {
		_ = os.Remove(rec.temp)
		return nil, err
	}
	rec.Hash, rec.Size, rec.Head = hex.EncodeToString(h.Sum(nil)), n, head.b
	return rec, nil
}

// Keep moves a received file to its final name. The same content already there is the same file.
func (f *Files) Keep(datapack string, rec *Received) error {
	dst, err := f.Path(datapack, rec.Hash)
	if err != nil {
		return err
	}
	return os.Rename(rec.temp, dst)
}

// Discard removes a received file that won't be kept.
func (f *Files) Discard(rec *Received) {
	if rec != nil {
		_ = os.Remove(rec.temp)
	}
}

// Open opens a datapack's file.
func (f *Files) Open(datapack, hash string) (*os.File, error) {
	p, err := f.Path(datapack, hash)
	if err != nil {
		return nil, err
	}
	return os.Open(p) //nolint:gosec // G304: a path inside the data folder, from a checked id and hash
}

// Read reads a datapack's file.
func (f *Files) Read(datapack, hash string) ([]byte, error) {
	p, err := f.Path(datapack, hash)
	if err != nil {
		return nil, err
	}
	return os.ReadFile(p) //nolint:gosec // G304: a path inside the data folder, from a checked id and hash
}

// Remove removes a datapack's file; one already gone is fine.
func (f *Files) Remove(datapack, hash string) error {
	p, err := f.Path(datapack, hash)
	if err != nil {
		return err
	}
	if err = os.Remove(p); errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	return err
}

// RemoveDatapack removes a datapack's folder and every file in it.
func (f *Files) RemoveDatapack(datapack string) error {
	d, err := f.dir(datapack)
	if err != nil {
		return err
	}
	return os.RemoveAll(d)
}

// staleTemp is how old a temporary file must be before Sweep removes it: older than any upload
// still being written.
const staleTemp = time.Hour

// Sweep removes what no document needs: files keep says no to (keep(datapack, hash) false),
// folders of datapacks that are gone (keep(datapack, "") false), temporary files a crash left, and
// anything else in the folder. It returns how many it removed.
func (f *Files) Sweep(keep func(datapack, hash string) (bool, error)) (int, error) {
	dirs, err := os.ReadDir(f.root)
	if errors.Is(err, fs.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	n := 0
	for _, d := range dirs {
		p := filepath.Join(f.root, d.Name())
		if !d.IsDir() {
			if err := os.Remove(p); err != nil {
				return n, err
			}
			n++
			continue
		}
		ok, err := keep(d.Name(), "")
		if err != nil {
			return n, err
		}
		if !ok {
			if err = os.RemoveAll(p); err != nil {
				return n, err
			}
			n++
			continue
		}
		files, err := os.ReadDir(p)
		if err != nil {
			return n, err
		}
		for _, e := range files {
			fp := filepath.Join(p, e.Name())
			drop := e.IsDir()
			switch {
			case drop:
			case strings.HasPrefix(e.Name(), tempPrefix):
				info, ierr := e.Info()
				drop = ierr == nil && time.Since(info.ModTime()) > staleTemp
			case !IsHash(e.Name()):
				drop = true
			default:
				if ok, err = keep(d.Name(), e.Name()); err != nil {
					return n, err
				}
				drop = !ok
			}
			if drop {
				if err = os.RemoveAll(fp); err != nil {
					return n, err
				}
				n++
			}
		}
	}
	return n, nil
}

// headWriter keeps the first max bytes written to it.
type headWriter struct {
	b   []byte
	max int
}

func (w *headWriter) Write(p []byte) (int, error) {
	if room := w.max - len(w.b); room > 0 {
		w.b = append(w.b, p[:min(room, len(p))]...)
	}
	return len(p), nil
}
