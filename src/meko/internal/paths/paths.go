// Package paths lays out Meko's data folder and picks where it lives.
//
//	<root>/            0700
//	  config.toml      optional
//	  web.toml, cloud.toml  settings Meko chose itself: the web UI's, the Meko Cloud link's URLs
//	  meko.db          the only database (0600)
//	  client.token     the token MCP and REST clients send, kept across restarts (0600)
//	  run/             pid, port, lock, admin token, unix sockets
//	    children/      each child's pid and process group, for stop
//	  logs/            meko.log, start.log, context-engine.log, service.log (the service's stderr)
//	  secrets/         secrets kept in files ([secrets] store = "file"); the client token stays in client.token
//	  extensions.json  the extensions meko start and stop run
//	  extensions/      each extension's own folder (extensions/kode is Kode's home); meko destroy
//	                   keeps it unless told to remove the extensions too
//	  update.json      what the last update check found
//	  models/          embedding model files, downloaded on first start
//	  context-engine/  the context engine's own state (mem0's config folder)
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
	Root              string
	Config            string
	DB                string
	RunDir            string
	LogDir            string
	Lock              string
	Pid               string
	Port              string
	Token             string // the admin token, new at each start
	ClientToken       string // the clients' token, kept across restarts
	Log               string
	StartLog          string
	ServiceLog        string // the service's stderr: errors from before serve's own log opens
	StoreSock         string // the store API the context engine calls
	ContextEngineSock string // the context engine's API
	ChildrenDir       string
	ModelsDir         string
	ContextEngineDir  string
	ContextEngineLog  string
	// KnowledgeFiles holds knowledge files' bytes, beside the database.
	KnowledgeFiles string
	// Extensions holds each extension's own folder (extensions.Dir); it's the extensions' data, not Meko's.
	Extensions string
	// Certs holds the self-signed certificate browsers get with remote access (internal/tlscert).
	Certs string
	// FirstCommands is the first commands run after a first install, for the usage stats
	// (internal/callhome, firstcommands.go): only on an install that began with it.
	FirstCommands string
}

// New returns the layout of the data folder at root (an absolute path).
func New(root string) Layout {
	run := filepath.Join(root, "run")
	logs := filepath.Join(root, "logs")
	return Layout{
		Root:              root,
		Config:            filepath.Join(root, "config.toml"),
		DB:                filepath.Join(root, "meko.db"),
		RunDir:            run,
		LogDir:            logs,
		Lock:              filepath.Join(run, "meko.lock"),
		Pid:               filepath.Join(run, "meko.pid"),
		Port:              filepath.Join(run, "meko.port"),
		Token:             filepath.Join(run, "admin.token"),
		ClientToken:       filepath.Join(root, "client.token"),
		Log:               filepath.Join(logs, "meko.log"),
		StartLog:          filepath.Join(logs, "start.log"),
		ServiceLog:        filepath.Join(logs, "service.log"),
		StoreSock:         filepath.Join(run, "store.sock"),
		ContextEngineSock: filepath.Join(run, "ctx_eng.sock"),
		ChildrenDir:       filepath.Join(run, "children"),
		ModelsDir:         filepath.Join(root, "models"),
		ContextEngineDir:  filepath.Join(root, "context-engine"),
		ContextEngineLog:  filepath.Join(logs, "context-engine.log"),

		KnowledgeFiles: filepath.Join(root, "knowledgebase-files"),
		Extensions:     filepath.Join(root, "extensions"),
		Certs:          filepath.Join(root, "certs"),
		FirstCommands:  filepath.Join(root, "first-commands.jsonl"),
	}
}

// MaxSocketPath is the longest unix socket path every supported OS takes (macOS allows 104 bytes,
// Linux 108, both counting the final NUL).
const MaxSocketPath = 103

// Ensure creates the folders, readable only by this user. A data folder whose sockets would be too
// long a path for the OS is refused.
func (l Layout) Ensure() error {
	if len(l.ContextEngineSock) > MaxSocketPath {
		return fmt.Errorf("the data folder %s is too deep: its sockets' paths would be longer than %d bytes. "+
			"Choose a shorter path with --data-dir or %s", l.Root, MaxSocketPath, HomeEnv)
	}
	for _, d := range []string{l.Root, l.RunDir, l.ChildrenDir, l.LogDir, l.ModelsDir, l.ContextEngineDir,
		l.KnowledgeFiles} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return err
		}
		if err := os.Chmod(d, 0o700); err != nil {
			return err
		}
	}
	return nil
}

// MoveRenamed gives the files a release from before the context engine's rename (when it was the
// memory engine) left in the data folder their new names, so meko logs keeps the engine's history.
// Each moves once: only while the new name is free (an empty folder, as Ensure makes, counts as
// free). serve runs it only once its release is past probation, with the data folder locked: a Meko
// rolled back before then must still find its files under their old names. A failed move isn't an
// error, since the context engine just starts a new log and state folder.
func (l Layout) MoveRenamed() {
	for _, m := range l.renames() {
		if movable(m[0], m[1]) {
			_ = os.Rename(m[0], m[1])
		}
	}
}

// renames are the files MoveRenamed moves: each one's old name and new name.
func (l Layout) renames() [][2]string {
	return [][2]string{
		{filepath.Join(l.LogDir, "memory_engine.log"), l.ContextEngineLog},
		{filepath.Join(l.Root, "memory_engine"), l.ContextEngineDir},
	}
}

// movable reports whether MoveRenamed would move from to to: from is there, and to is free (missing,
// or an empty folder, which it removes).
func movable(from, to string) bool {
	if _, err := os.Lstat(from); err != nil {
		return false
	}
	if fi, err := os.Lstat(to); err == nil {
		return fi.IsDir() && os.Remove(to) == nil // Remove takes only an empty folder
	} else if !os.IsNotExist(err) {
		return false
	}
	return true
}

// BeforeRenames is l with the context engine's log and state folder at their old names while
// MoveRenamed hasn't moved them yet: what a start on probation uses. It mayn't move them (a Meko
// rolled back must find them), and starting the engine under the new names would leave them taken,
// so MoveRenamed, at the first start past probation, could never move the old ones.
func (l Layout) BeforeRenames() Layout {
	if m := l.renames(); movable(m[0][0], m[0][1]) {
		l.ContextEngineLog = m[0][0]
	}
	if m := l.renames(); movable(m[1][0], m[1][1]) {
		l.ContextEngineDir = m[1][0]
	}
	return l
}

// HomeEnv names the environment variable that sets the data folder.
const HomeEnv = "MEKO_HOME"

// networkFS are filesystems SQLite's locking and flock can't be trusted on. The names are the
// ones Kode checks (src/extensions/agentk/src/server/fstype.ts, and its scripts/install.sh): Linux
// mount types and macOS statfs names.
var networkFS = map[string]bool{
	"nfs": true, "nfs4": true, "cifs": true, "smb2": true, "smbfs": true, "fuse.sshfs": true,
	"afs": true, "lustre": true, "gpfs": true, "afpfs": true, "webdav": true, "macfuse": true,
	"osxfuse": true,
}

// FSType returns the filesystem type of the folder that holds path. Tests replace it.
var FSType = fsType

// Resolve picks the data folder: explicit (from --data-dir) first, then MEKO_HOME, then
// ~/.meko. A folder the user named on a network filesystem is refused; the default moves
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
	def := filepath.Join(home, ".meko")
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
// $XDG_STATE_HOME/meko if that's local, else /var/tmp/<user>/meko (as Kode does
// for its state.db). A relative XDG_STATE_HOME is ignored, as the XDG spec says (and as
// Kode's mekoHome and install.sh do).
func localFallback() (string, error) {
	if xdg := os.Getenv("XDG_STATE_HOME"); xdg != "" && filepath.IsAbs(xdg) {
		dir := filepath.Join(xdg, "meko")
		if networkType(dir) == "" {
			return dir, nil
		}
	}
	u, err := user.Current()
	if err != nil {
		return "", err
	}
	dir := filepath.Join(SharedTmp, u.Username, "meko")
	if err := EnsurePrivateDir(dir); err != nil {
		return "", err
	}
	return dir, nil
}

// EnsurePrivateDir creates dir in a shared place like /var/tmp, where another user could have
// made the folders first. It refuses unless dir and its parent are real folders (not symlinks)
// owned by this user and the parent isn't writable by others (group write is fine for the user's
// own primary group, as Kode allows), then makes dir 0700.
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

// LooksLikeDataDir reports whether dir is a Meko data folder, so it's safe to remove whole: it
// holds meko.db, and it isn't / or the home folder. Only a server makes meko.db. Other files aren't
// proof: the CLI's own commands make run/meko.lock (status, stop) and Ensure's folders and
// client.token (token) in whatever folder --data-dir names.
func LooksLikeDataDir(dir string) bool {
	dir = filepath.Clean(dir)
	if dir == "/" {
		return false
	}
	if home, err := os.UserHomeDir(); err == nil && SameDir(dir, home) {
		return false
	}
	fi, err := os.Lstat(New(dir).DB)
	return err == nil && fi.Mode().IsRegular()
}

// SameDir reports whether a and b are the same folder (or file): spelled alike, which holds for one
// that's gone too (a service naming a data folder destroy removed), or, following links, the same
// folder itself. One folder can have spellings links don't explain, such as a bind mount, or an
// automount of a local disk, that puts one disk at two paths.
func SameDir(a, b string) bool {
	if a != "" && filepath.Clean(a) == filepath.Clean(b) {
		return true
	}
	fa, err := os.Stat(a)
	if err != nil {
		return false
	}
	fb, err := os.Stat(b)
	return err == nil && os.SameFile(fa, fb)
}

// DirSize is the total size of the regular files under dir, leaving out the folders in skip.
// Files it can't read are skipped.
func DirSize(dir string, skip ...string) int64 {
	var n int64
	_ = filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err == nil && d.IsDir() {
			for _, s := range skip {
				if p == s {
					return filepath.SkipDir
				}
			}
		}
		if err == nil && d.Type().IsRegular() {
			if fi, ierr := d.Info(); ierr == nil {
				n += fi.Size()
			}
		}
		return nil
	})
	return n
}

// RemoveIfExists removes path, ignoring a file that's already gone.
func RemoveIfExists(path string) error {
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}
