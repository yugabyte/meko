package update

import (
	"archive/tar"
	"bufio"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// Stage downloads a release of the app's program into the app folder: the tarball and SHA256SUMS,
// the checksum checked, unpacked beside the installed versions and checked to run here (its
// `<name> version --json` must name the version), as app/<version>. Nothing in use changes.
func (a App) Stage(ctx context.Context, c *Client, rel Release, version string) error {
	goos, arch := Platform()
	name := PackageTarball(a.name(), version, goos, arch)
	tarAsset, ok := rel.Asset(name)
	sumAsset, ok2 := rel.Asset(Sums)
	if !ok || !ok2 {
		return fmt.Errorf("release %s has no %s or %s", rel.Tag, name, Sums)
	}
	if err := os.MkdirAll(a.Root, 0o700); err != nil {
		return err
	}
	dl, err := os.MkdirTemp(a.Root, ".download-")
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(dl) }()
	tarball, sums := filepath.Join(dl, name), filepath.Join(dl, Sums)
	if err := c.Download(ctx, tarAsset, tarball); err != nil {
		return err
	}
	if err := c.Download(ctx, sumAsset, sums); err != nil {
		return err
	}
	if err := VerifySum(tarball, sums); err != nil {
		return err
	}
	return a.Unpack(tarball, version)
}

// VerifySum checks file against its line in a SHA256SUMS file.
func VerifySum(file, sumsFile string) error {
	f, err := os.Open(sumsFile) //nolint:gosec // G304: a file we downloaded
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }() // only read
	want := ""
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) == 2 && strings.TrimPrefix(fields[1], "*") == filepath.Base(file) {
			want = fields[0]
		}
	}
	if want == "" {
		return fmt.Errorf("%s has no entry for %s", filepath.Base(sumsFile), filepath.Base(file))
	}
	h := sha256.New()
	in, err := os.Open(file) //nolint:gosec // G304: a file we downloaded
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }() // only read
	if _, err := io.Copy(h, in); err != nil {
		return err
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != want {
		return fmt.Errorf("checksum mismatch for %s: got %s, want %s", filepath.Base(file), got, want)
	}
	return nil
}

// Unpack unpacks a release tarball (one folder, <name>-<version>/) to app/<version>, checking that
// its binary runs here first. An installed copy of the same version is replaced.
func (a App) Unpack(tarball, version string) error {
	staging, err := os.MkdirTemp(a.Root, ".staging-")
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(staging) }()
	if err := extract(tarball, staging); err != nil {
		return err
	}
	top := filepath.Join(staging, a.name()+"-"+version)
	if err := CheckBinary(filepath.Join(top, "bin", a.name()), version); err != nil {
		return err
	}
	//nolint:gosec // G306: a version number isn't secret
	if err := os.WriteFile(filepath.Join(top, "VERSION"), []byte(version+"\n"), 0o644); err != nil {
		return err
	}
	dest := a.Dir(version)
	old := ""
	if _, err := os.Stat(dest); err == nil {
		old = dest + ".old-" + randomSuffix()
		if err := os.Rename(dest, old); err != nil {
			return err
		}
	}
	if err := os.Rename(top, dest); err != nil {
		if old != "" {
			_ = os.Rename(old, dest) // keep the copy that was there
		}
		return err
	}
	if old != "" {
		_ = os.RemoveAll(old)
	}
	return nil
}

// CheckBinary runs `<exe> version --json` and checks it names version: a binary that can't run on
// this machine never becomes current. Meko's and Presenter's both answer {"version": "<version>"}.
func CheckBinary(exe, version string) error {
	name := filepath.Base(exe)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, exe, "version", "--json").Output() //nolint:gosec // G204: the staged release
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return fmt.Errorf("%s %s doesn't run on this machine: %w: %s", name, version, err,
				strings.TrimSpace(string(ee.Stderr)))
		}
		return fmt.Errorf("%s %s doesn't run on this machine: %w", name, version, err)
	}
	var v struct {
		Version string `json:"version"`
	}
	if err := json.Unmarshal(out, &v); err != nil || v.Version != version {
		return fmt.Errorf("the staged %s says it's version %q, not %s", name, v.Version, version)
	}
	return nil
}

// extract unpacks a .tar.gz into dir. Entries that would land outside it, and links pointing out
// of it, are refused.
func extract(tarball, dir string) error {
	f, err := os.Open(tarball) //nolint:gosec // G304: a file we downloaded
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }() // only read
	gz, err := gzip.NewReader(f)
	if err != nil {
		return err
	}
	defer func() { _ = gz.Close() }()
	tr := tar.NewReader(gz)
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		name := filepath.Clean(h.Name)
		if filepath.IsAbs(name) || name == ".." || strings.HasPrefix(name, ".."+string(filepath.Separator)) {
			return fmt.Errorf("the tarball has an entry outside its folder: %s", h.Name)
		}
		target := filepath.Join(dir, name)
		switch h.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o755); err != nil { //nolint:gosec // G301: release files aren't secret
				return err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil { //nolint:gosec // G301: see above
				return err
			}
			mode := os.FileMode(h.Mode) & 0o755                                       //nolint:gosec // G115: a tar mode, masked
			out, err := os.OpenFile(target, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, mode) //nolint:gosec // G304: checked above
			if err != nil {
				return err
			}
			_, err = io.Copy(out, io.LimitReader(tr, maxDownload)) //nolint:gosec // G110: bounded
			if cerr := out.Close(); err == nil {
				err = cerr
			}
			if err != nil {
				return err
			}
		case tar.TypeSymlink:
			link := filepath.Join(filepath.Dir(target), h.Linkname) //nolint:gosec // G305: checked on the next line
			if filepath.IsAbs(h.Linkname) || !strings.HasPrefix(link, dir+string(filepath.Separator)) {
				return fmt.Errorf("the tarball has a link out of its folder: %s -> %s", h.Name, h.Linkname)
			}
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil { //nolint:gosec // G301: see above
				return err
			}
			if err := os.Symlink(h.Linkname, target); err != nil {
				return err
			}
		default:
			// Nothing else belongs in a release (no devices, no hard links).
		}
	}
}
