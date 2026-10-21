// Package paths lays out Meko Local's data folder and picks where it lives.
//
//	<root>/            0700
//	  config.toml      optional
//	  meko.db          the only database (0600)
//	  run/             pid, port, lock, admin token, unix sockets
//	    children/      each child's pid and process group, for stop
//	  logs/            meko-local.log, start.log, engine.log
//	  models/          embedding model files, downloaded on first start
//	  engine/          the engine's own state (mem0's config folder)
package paths

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/user"
	"path/filepath"
	"syscall"
)

// Layout is every path in one data folder.
type Layout struct {
	Root        string
	Config      string
	DB          string
	RunDir      string
	LogDir      string
	Lock        string
	Pid         string
	Port        string
	Token       string
	Log         string
	StartLog    string
	StoreSock   string // the store API the engine calls
	EngineSock  string // the engine's API
	ChildrenDir string
	ModelsDir   string
	EngineDir   string
	EngineLog   string
}

// New returns the layout of the data folder at root (an absolute path).
func New(root string) Layout {
	run := filepath.Join(root, "run")
	logs := filepath.Join(root, "logs")
	return Layout{
		Root:        root,
		Config:      filepath.Join(root, "config.toml"),
		DB:          filepath.Join(root, "meko.db"),
		RunDir:      run,
		LogDir:      logs,
		Lock:        filepath.Join(run, "meko-local.lock"),
		Pid:         filepath.Join(run, "meko-local.pid"),
		Port:        filepath.Join(run, "meko-local.port"),
		Token:       filepath.Join(run, "admin.token"),
		Log:         filepath.Join(logs, "meko-local.log"),
		StartLog:    filepath.Join(logs, "start.log"),
		StoreSock:   filepath.Join(run, "store.sock"),
		EngineSock:  filepath.Join(run, "engine.sock"),
		ChildrenDir: filepath.Join(run, "children"),
		ModelsDir:   filepath.Join(root, "models"),
		EngineDir:   filepath.Join(root, "engine"),
		EngineLog:   filepath.Join(logs, "engine.log"),
	}
}

// MaxSocketPath is the longest unix socket path every supported OS takes (macOS allows 104 bytes,
// Linux 108, both counting the final NUL).
const MaxSocketPath = 103

// Ensure creates the folders, readable only by this user. A data folder whose sockets would be too
// long a path for the OS is refused.
func (l Layout) Ensure() error {
	if len(l.EngineSock) > MaxSocketPath {
		return fmt.Errorf("the data folder %s is too deep: its sockets' paths would be longer than %d bytes. "+
			"Choose a shorter path with --data-dir or %s", l.Root, MaxSocketPath, HomeEnv)
	}
	for _, d := range []string{l.Root, l.RunDir, l.ChildrenDir, l.LogDir, l.ModelsDir, l.EngineDir} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return err
		}
		if err := os.Chmod(d, 0o700); err != nil {
			return err
		}
	}
	return nil
}

// HomeEnv names the environment variable that sets the data folder.
const HomeEnv = "MEKO_LOCAL_HOME"

// networkFS are filesystems SQLite's locking and flock can't be trusted on. The names are the
// ones AgentK Local's installer checks (extensions/agentk/src/server/install.ts): Linux mount types and macOS
// statfs names.
var networkFS = map[string]bool{
	"nfs": true, "nfs4": true, "cifs": true, "smb2": true, "smbfs": true, "fuse.sshfs": true,
	"afs": true, "lustre": true, "gpfs": true, "afpfs": true, "webdav": true, "macfuse": true,
	"osxfuse": true,
}

// FSType returns the filesystem type of the folder that holds path. Tests replace it.
var FSType = fsType

// Resolve picks the data folder: explicit (from --data-dir) first, then MEKO_LOCAL_HOME, then
// ~/.meko-local. A folder the user named on a network filesystem is refused; the default moves
// to local disk instead.
func Resolve(explicit string) (string, error) {
	named := explicit
	if named == "" {
		named = os.Getenv(HomeEnv)
	}
	if named != "" {
		abs, err := filepath.Abs(named)
		if err != nil {
			return "", err
		}
		if t := networkType(abs); t != "" {
			return "", fmt.Errorf("%s is on a network filesystem (%s), where SQLite's locking doesn't work. "+
				"Choose a folder on a local disk with --data-dir or %s", abs, t, HomeEnv)
		}
		return abs, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("can't find your home folder: %w. Set --data-dir or %s", err, HomeEnv)
	}
	def := filepath.Join(home, ".meko-local")
	if networkType(def) == "" {
		return def, nil
	}
	return localFallback()
}

// networkType is the network filesystem path is on, or "" if it's local or unknown.
func networkType(path string) string {
	t, err := FSType(existingAncestor(path))
	if err != nil || !networkFS[t] {
		return ""
	}
	return t
}

func existingAncestor(path string) string {
	for p := path; ; p = filepath.Dir(p) {
		if _, err := os.Stat(p); err == nil || filepath.Dir(p) == p { //nolint:gosec // G703: our own data folder path
			return p
		}
	}
}

// SharedTmp is the shared local folder localFallback uses; tests replace it.
var SharedTmp = "/var/tmp"

// localFallback is a local-disk data folder for a home folder on a network filesystem:
// $XDG_STATE_HOME/meko-local if that's local, else /var/tmp/<user>/meko-local (as AgentK does
// for its state.db).
func localFallback() (string, error) {
	if xdg := os.Getenv("XDG_STATE_HOME"); xdg != "" {
		dir := filepath.Join(xdg, "meko-local")
		if networkType(dir) == "" {
			return dir, nil
		}
	}
	u, err := user.Current()
	if err != nil {
		return "", err
	}
	dir := filepath.Join(SharedTmp, u.Username, "meko-local")
	if err := EnsurePrivateDir(dir); err != nil {
		return "", err
	}
	return dir, nil
}

// EnsurePrivateDir creates dir in a shared place like /var/tmp, where another user could have
// made the folders first. It refuses unless dir and its parent are real folders (not symlinks)
// owned by this user and the parent isn't writable by others (group write is fine for the user's
// own primary group, as AgentK allows), then makes dir 0700.
func EnsurePrivateDir(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	fix := "Remove it, or set --data-dir to a local folder only you can write to."
	for _, d := range []string{filepath.Dir(dir), dir} {
		fi, err := os.Lstat(d)
		if err != nil {
			return err
		}
		if fi.Mode()&fs.ModeSymlink != 0 || !fi.IsDir() {
			return fmt.Errorf("%s is not a folder (it may be a symlink). %s", d, fix)
		}
		if st, ok := fi.Sys().(*syscall.Stat_t); ok && int(st.Uid) != os.Getuid() {
			return fmt.Errorf("%s is owned by another user. %s", d, fix)
		}
		if d != dir && othersCanWrite(fi) {
			return fmt.Errorf("%s is writable by other users. %s", d, fix)
		}
	}
	return os.Chmod(dir, 0o700)
}

// othersCanWrite reports whether anyone but this user can write in a folder: it's world-writable,
// or group-writable for a group that isn't the user's own primary group.
func othersCanWrite(fi fs.FileInfo) bool {
	perm := fi.Mode().Perm()
	if perm&0o002 != 0 {
		return true
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	return perm&0o020 != 0 && (!ok || int(st.Gid) != os.Getgid())
}

// WriteFile writes data to path atomically (a temp file renamed over it), readable only by
// this user.
func WriteFile(path string, data []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(tmp.Name()) }() // gone after the rename
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close() // already failing; the temp file is removed
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close() // already failing; the temp file is removed
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// RemoveIfExists removes path, ignoring a file that's already gone.
func RemoveIfExists(path string) error {
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}
