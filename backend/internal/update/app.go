package update

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/yugabyte/meko/backend/internal/paths"
)

// App is a release install's app folder: Meko's, or an extension's that Meko installs from its
// releases (Presenter's, in <data folder>/extensions/presenter/app). Name is the program, which
// names its binary (<version>/bin/<name>) and its link in ~/.local/bin; empty is meko. Package
// names its tarballs (<package>-<version>-<os>-<arch>.tar.gz, one folder <package>-<version>/);
// empty is Name. Presenter's program is meko_presenter, its package presenter. Private unpacks
// releases owner-only (folders and programs 0700, files 0600), as files in Meko's data folder must
// be: an extension's app folder is in it, Meko's own isn't.
type App struct {
	Root    string
	Name    string
	Package string
	Private bool
}

func (a App) name() string {
	if a.Name == "" {
		return "meko"
	}
	return a.Name
}

func (a App) pkg() string {
	if a.Package == "" {
		return a.name()
	}
	return a.Package
}

// DefaultRoot is where the installer puts releases: $XDG_DATA_HOME/meko/app, or
// ~/.local/share/meko/app. It's outside the data folder, so meko destroy, which removes the data
// folder whole, never removes the binary the service runs.
func DefaultRoot() (string, error) {
	if x := os.Getenv("XDG_DATA_HOME"); x != "" {
		return filepath.Abs(filepath.Join(x, "meko", "app"))
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".local", "share", "meko", "app"), nil
}

// Running finds the release install the running binary belongs to: it's app/<version>/bin/meko,
// with app/<version>/VERSION. ok is false for a binary built from source.
func Running() (app App, version string, ok bool) {
	exe, err := os.Executable()
	if err != nil {
		return App{}, "", false
	}
	return Of(exe)
}

// Of is Running for the binary at exe.
func Of(exe string) (App, string, bool) {
	real, err := filepath.EvalSymlinks(exe)
	if err != nil {
		return App{}, "", false
	}
	verDir := filepath.Dir(filepath.Dir(real))
	b, err := os.ReadFile(filepath.Join(verDir, "VERSION")) //nolint:gosec // G304: the release's own file
	if err != nil {
		return App{}, "", false
	}
	v := strings.TrimSpace(string(b))
	if filepath.Base(verDir) != v {
		return App{}, "", false
	}
	return App{Root: filepath.Dir(verDir)}, v, true
}

// Dir is a version's folder.
func (a App) Dir(version string) string { return filepath.Join(a.Root, version) }

// Exe is a version's binary.
func (a App) Exe(version string) string { return filepath.Join(a.Dir(version), "bin", a.name()) }

// CurrentExe is the binary app/current points at: what the service runs.
func (a App) CurrentExe() string { return filepath.Join(a.Root, "current", "bin", a.name()) }

// Current is the version app/current points at ("" if none).
func (a App) Current() string {
	t, err := os.Readlink(filepath.Join(a.Root, "current"))
	if err != nil {
		return ""
	}
	return filepath.Base(t)
}

// Repoint points app/current at version, atomically (a new link renamed over the old one).
func (a App) Repoint(version string) error {
	if _, err := os.Stat(a.Exe(version)); err != nil {
		return fmt.Errorf("version %s isn't installed in %s: %w", version, a.Root, err)
	}
	tmp := filepath.Join(a.Root, ".current-"+randomSuffix())
	if err := os.Symlink(version, tmp); err != nil {
		return err
	}
	if err := os.Rename(tmp, filepath.Join(a.Root, "current")); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

func randomSuffix() string {
	b := make([]byte, 4)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// LinkCommand points <home>/.local/bin/<name> at app/current/bin/<name>, so it's a command.
func (a App) LinkCommand(home string) (string, error) {
	bin := filepath.Join(home, ".local", "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil { //nolint:gosec // G301: the user's own bin folder
		return "", err
	}
	link := filepath.Join(bin, a.name())
	tmp := link + ".tmp-" + randomSuffix()
	if err := os.Symlink(a.CurrentExe(), tmp); err != nil {
		return "", err
	}
	if err := os.Rename(tmp, link); err != nil {
		_ = os.Remove(tmp)
		return "", err
	}
	return link, nil
}

// MayLink reports whether LinkCommand may make <home>/.local/bin/<name>: there's none, or it's a
// link into this app folder, or a dangling one. A link into another app folder is another install's
// (an extension's release for another of this user's Meko data folders), and stays.
func (a App) MayLink(home string) bool {
	link := filepath.Join(home, ".local", "bin", a.name())
	t, err := os.Readlink(link)
	if err != nil {
		_, lerr := os.Lstat(link)
		return errors.Is(lerr, fs.ErrNotExist)
	}
	if strings.HasPrefix(t, a.Root+string(filepath.Separator)) {
		return true
	}
	_, serr := os.Stat(link)
	return serr != nil
}

// UnlinkCommand removes <home>/.local/bin/<name> if it points into this app folder.
func (a App) UnlinkCommand(home string) error {
	link := filepath.Join(home, ".local", "bin", a.name())
	t, err := os.Readlink(link)
	if err != nil || !strings.HasPrefix(t, a.Root+string(filepath.Separator)) {
		return nil //nolint:nilerr // not ours to remove
	}
	return paths.RemoveIfExists(link)
}

// Versions lists the installed versions, oldest first.
func (a App) Versions() []string {
	entries, err := os.ReadDir(a.Root)
	if err != nil {
		return nil
	}
	var vs []Version
	for _, e := range entries {
		if v, ok := ParseVersion(e.Name()); ok && e.IsDir() {
			vs = append(vs, v)
		}
	}
	sort.Slice(vs, func(i, j int) bool { return vs[i].Less(vs[j]) })
	out := make([]string, len(vs))
	for i, v := range vs {
		out[i] = v.String()
	}
	return out
}

// Prune deletes installed versions other than keep (the current and previous ones, and any
// version a pending update names). It returns what it deleted.
func (a App) Prune(keep ...string) []string {
	k := map[string]bool{}
	for _, v := range keep {
		k[v] = true
	}
	var gone []string
	for _, v := range a.Versions() {
		if !k[v] && os.RemoveAll(a.Dir(v)) == nil {
			gone = append(gone, v)
		}
	}
	return gone
}

func (a App) badFile() string { return filepath.Join(a.Root, "bad-versions") }

// BadVersions are versions that were rolled back; they're never offered again.
func (a App) BadVersions() map[string]bool {
	b, err := os.ReadFile(a.badFile())
	out := map[string]bool{}
	if err != nil {
		return out
	}
	for _, l := range strings.Split(string(b), "\n") {
		if l = strings.TrimSpace(l); l != "" {
			out[l] = true
		}
	}
	return out
}

func (a App) addBadVersion(v string) error {
	f, err := os.OpenFile(a.badFile(), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintln(f, v)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	return err
}

// Remove deletes the app folder, every version in it.
func (a App) Remove() error {
	if a.Root == "" || filepath.Base(a.Root) != "app" {
		return errors.New("refusing to remove a folder that isn't an app folder")
	}
	if err := os.RemoveAll(a.Root); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}
