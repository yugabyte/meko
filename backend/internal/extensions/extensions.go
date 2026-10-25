// Package extensions keeps the list of extensions this Meko runs, in extensions.json in the data
// folder, and runs their own commands to start and stop them. An extension has its own service and
// its own updates; Meko installs it (meko install_extension), starts it with meko start and stops
// it with meko stop, and never restarts it when Meko itself restarts or updates.
//
// The contract is the extension's CLI: `<command> start`, `<command> stop` and `<command> status`,
// and its uninstall (Catalog.UninstallArgs) for meko destroy --with-extensions. Every command Meko
// runs of an extension gets MEKO_HOME set to Meko's data folder, so the extension knows where its
// own folder in it is (Dir). AgentK's agentk is the first; Presenter's presenter the second.
package extensions

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/yugabyte/meko/backend/internal/paths"
	"github.com/yugabyte/meko/backend/internal/update"
)

// Known are the extensions meko install_extension can install, by name.
var Known = map[string]Catalog{
	"agentk": {
		Title: "AgentK",
		Description: "Runs Claude Code, OpenCode and Kiro sessions across your projects, from task to merged PR, " +
			"with Meko as their shared memory.",
		Command: "agentk",
		// Its name before the rename to AgentK, which installs from then may still have alone.
		OldCommands: []string{"agent-k-local"},
		// Its releases are tagged agentk-v<version>, and v<version> before releases were tagged by product.
		TagPrefixes: []string{"agentk-v", "v"},
		Installer:   "install.sh",
		// Its home before it moved into Meko's data folder, where an install that hasn't moved yet runs.
		OldHomes:      []string{".agent-k-local"},
		UninstallArgs: []string{"uninstall", "--purge", "--delete-worktrees", "--non-interactive"},
		WebUI:         true,
	},
	// Presenter (extensions/presenter): interactive, branching presentations built from datapacks.
	// Its command is meko_presenter. Its releases (presenter-v<version>) carry
	// presenter-<version>-<os>-<arch>.tar.gz and SHA256SUMS, which Meko installs and updates itself
	// (Managed). A build from source on PATH is used as it is, and never updated.
	"presenter": {
		Title: "Presenter",
		Description: "Builds presentations that branch with the room from your datapacks: Claude drafts the story " +
			"map and the slides, and you pick each path live.",
		Command:       "meko_presenter",
		TagPrefixes:   []string{"presenter-v"},
		Managed:       true,
		UninstallArgs: []string{"uninstall", "--purge", "--non-interactive"},
		WebUI:         true,
	},
}

// KnownNames lists the extensions install_extension knows, in order.
func KnownNames() []string {
	names := make([]string, 0, len(Known))
	for n := range Known {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// Catalog says how to find and install a known extension.
type Catalog struct {
	Title       string
	Description string   // what it does, in a sentence, for the web UI's Extensions page
	Command     string   // the program's name, looked for on PATH and in ~/.local/bin
	OldCommands []string // names it had before, looked for after Command
	TagPrefixes []string // its release tags, newest naming first
	Installer   string   // the release asset that installs it, when it installs itself (not Managed)
	// Managed says Meko installs it from its releases' tarballs into App and updates it: the
	// <name>-<version>-<os>-<arch>.tar.gz (named for the extension, holding bin/<Command>) and
	// SHA256SUMS of the newest release with TagPrefixes.
	Managed bool
	// OldHomes are folders under the home folder a release install of it lived in before its home
	// moved into Meko's data folder (Dir); Find looks for the command in their app/current/bin.
	OldHomes []string
	// UninstallArgs uninstall it and delete all its data without asking: what meko destroy runs
	// when it removes the extensions too.
	UninstallArgs []string
	// WebUI says it serves a web UI on a unix socket, which the web listener serves at
	// /ext/<name>/ (internal/extproxy). `<command> _handoff --json` names the socket (Handoff).
	WebUI bool
}

// Dir is the folder an extension keeps its data in, in Meko's data folder at root. AgentK's home
// is there (Meko's rule decides where root is, and AgentK follows it).
func Dir(root, name string) string { return filepath.Join(root, "extensions", name) }

// App is the app folder a Managed extension's releases are installed in: <Dir>/app, one folder per
// version, app/current pointing at the one in use, update-pending.json and bad-versions beside
// them, as Meko's own (internal/update). Its tarballs are named for the extension, its binary for
// its command.
func (c Catalog) App(root, name string) update.App {
	return update.App{Root: filepath.Join(Dir(root, name), "app"), Name: c.Command, Package: name, Private: true}
}

// releaseOf is the version of the release install command runs (through links), and whether that
// release is in this data folder's app folder. A command that isn't a release install (a build from
// source) has no version.
func (c Catalog) releaseOf(root, name, command string) (string, bool) {
	app, v, ok := update.Of(command)
	if !ok {
		return "", false
	}
	// Of resolves links, so compare the real folders (macOS's /tmp is /private/tmp).
	want := c.App(root, name).Root
	if real, err := filepath.EvalSymlinks(want); err == nil {
		want = real
	}
	return v, app.Root == want
}

// Release is the version of a Managed extension's release install that command runs (through
// ~/.local/bin and app/current), or "" for a command that isn't one (a build from source).
func (c Catalog) Release(root, name, command string) string {
	if !c.Managed || command == "" {
		return ""
	}
	if v, ours := c.releaseOf(root, name, command); ours {
		return v
	}
	return ""
}

// Entry is one installed extension.
type Entry struct {
	Command         string    `json:"command"`
	InstalledByMeko bool      `json:"installed_by_meko"`
	AddedAt         time.Time `json:"added_at"`
}

// Registry is extensions.json.
type Registry struct {
	path    string
	Entries map[string]Entry `json:"extensions"`
}

// File is where the registry lives in the data folder at root.
func File(root string) string { return filepath.Join(root, "extensions.json") }

// Load reads the registry; a missing file is an empty one.
func Load(root string) (*Registry, error) {
	r := &Registry{path: File(root), Entries: map[string]Entry{}}
	b, err := os.ReadFile(r.path)
	if errors.Is(err, fs.ErrNotExist) {
		return r, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(b, r); err != nil {
		return nil, fmt.Errorf("%s: %w", r.path, err)
	}
	if r.Entries == nil {
		r.Entries = map[string]Entry{}
	}
	return r, nil
}

// Save writes the registry, readable only by this user.
func (r *Registry) Save() error {
	b, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	return paths.WriteFile(r.path, append(b, '\n'))
}

// Names lists the extensions in order.
func (r *Registry) Names() []string {
	names := make([]string, 0, len(r.Entries))
	for n := range r.Entries {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// Timeouts for an extension's commands. Stopping AgentK waits for its server and its sessions;
// uninstalling it also deletes its worktrees.
var (
	StartTimeout     = 2 * time.Minute
	StopTimeout      = 2 * time.Minute
	UninstallTimeout = 5 * time.Minute
	// InstallTimeout bounds an install the web UI started: the download and the installer.
	InstallTimeout = 15 * time.Minute
	// PipeWait is how long a command's output is read after it exits. Output that isn't a file (the
	// web UI's) is a pipe, which a process the command left running (a server it started without a
	// service) would hold open, so Wait would never return.
	PipeWait = 5 * time.Second
	// ListenTimeout is how long an install or start from the web UI waits for an extension with a
	// web UI to answer on its socket.
	ListenTimeout = 30 * time.Second
)

// Env is the environment an extension's command runs with: Meko's own, with MEKO_HOME naming
// Meko's data folder at root (the last value of a key is the one a program sees).
func Env(root string) []string { return append(os.Environ(), paths.HomeEnv+"="+root) }

// CommandEnv is Env for one extension command. A command npm installed (npm link, npm install -g)
// is a `#!/usr/bin/env node` script in the bin folder of the Node that installed it, and Meko's
// login service has no such Node on its PATH (launchd's is /usr/bin:/bin:/usr/sbin:/sbin), or an
// older one: when the command's folder holds a node, that folder goes first on PATH.
func CommandEnv(root, command string) []string {
	env := Env(root)
	dir := filepath.Dir(command)
	if !executable(filepath.Join(dir, "node")) {
		return env
	}
	path := dir
	if p := os.Getenv("PATH"); p != "" {
		path += string(os.PathListSeparator) + p
	}
	return append(env, "PATH="+path)
}

// Run runs `<command> <action>` for one extension, with its output passed through, for the data
// folder at root.
func (e Entry) Run(root, action string, stdout, stderr io.Writer) error {
	d := StartTimeout
	if action == "stop" {
		d = StopTimeout
	}
	return e.run(root, []string{action}, d, stdout, stderr)
}

// Uninstall runs `<command> <args>` (the catalog's UninstallArgs), with its output passed through.
func (e Entry) Uninstall(root string, args []string, stdout, stderr io.Writer) error {
	return e.run(root, args, UninstallTimeout, stdout, stderr)
}

func (e Entry) run(root string, args []string, d time.Duration, stdout, stderr io.Writer) error {
	ctx, cancel := context.WithTimeout(context.Background(), d)
	defer cancel()
	// G204: the extension's own command, from the registry.
	cmd := exec.CommandContext(ctx, e.Command, args...) //nolint:gosec
	cmd.Stdout, cmd.Stderr, cmd.Env, cmd.WaitDelay = stdout, stderr, CommandEnv(root, e.Command), PipeWait
	if err := cmd.Run(); err != nil && !errors.Is(err, exec.ErrWaitDelay) {
		what := e.Command + " " + strings.Join(args, " ")
		if ctx.Err() != nil {
			return fmt.Errorf("%s took longer than %s", what, d)
		}
		return fmt.Errorf("%s: %w", what, err)
	}
	return nil
}

// Status runs `<command> status` quietly and reports whether it succeeded, with its first line.
func (e Entry) Status(root string) (bool, string) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	var out bytes.Buffer
	cmd := exec.CommandContext(ctx, e.Command, "status") //nolint:gosec // G204: see run
	cmd.Stdout, cmd.Stderr, cmd.Env = &out, &out, CommandEnv(root, e.Command)
	err := cmd.Run()
	first, _, _ := bytes.Cut(bytes.TrimSpace(out.Bytes()), []byte("\n"))
	return err == nil, string(first)
}

// Find looks for the extension's program by its name, then by its old ones (FindCommand), then
// in the app/current/bin of its release install: its folder in Meko's data folder at root (Dir),
// then its old homes. It returns the absolute path, or "". A Managed extension's command on PATH
// or in ~/.local/bin that runs a release of another data folder (one link serves all of a user's
// Meko data folders) isn't this one's.
func (c Catalog) Find(name, home, root string) string {
	names := append([]string{c.Command}, c.OldCommands...)
	for _, n := range names {
		if p := FindCommand(n, home); p != "" {
			if v, ours := c.releaseOf(root, name, p); c.Managed && v != "" && !ours {
				continue
			}
			return p
		}
	}
	homes := []string{Dir(root, name)}
	for _, h := range c.OldHomes {
		homes = append(homes, filepath.Join(home, h))
	}
	for _, h := range homes {
		for _, n := range names {
			if p := filepath.Join(h, "app", "current", "bin", n); executable(p) {
				return p
			}
		}
	}
	return ""
}

// FindCommand looks for an extension's program on PATH, then in ~/.local/bin (a fresh shell may
// not have it on PATH yet). It returns the absolute path, or "".
func FindCommand(name, home string) string {
	if p, err := exec.LookPath(name); err == nil {
		if abs, err := filepath.Abs(p); err == nil {
			return abs
		}
	}
	if p := filepath.Join(home, ".local", "bin", name); executable(p) {
		return p
	}
	return ""
}

func executable(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && !fi.IsDir() && fi.Mode()&0o111 != 0
}

// Handoff is what `<command> _handoff --json` says: where an extension with a web UI listens, and
// the settings and login it had when it served browsers itself, which Meko takes over with its port.
type Handoff struct {
	Socket string  `json:"socket"`
	Legacy *Legacy `json:"legacy"`
}

// Legacy is an extension's own web settings from before Meko served it (AgentK's [server] and [tls],
// and its login), or nil when it had none.
type Legacy struct {
	Bind  string       `json:"bind"`
	Port  int          `json:"port"`
	TLS   *LegacyTLS   `json:"tls"`
	Owner *LegacyOwner `json:"owner"`
}

// LegacyTLS is its certificate: one it made (SelfSigned) or one its config named.
type LegacyTLS struct {
	Cert       string `json:"cert"`
	Key        string `json:"key"`
	SelfSigned bool   `json:"self_signed"`
}

// LegacyOwner is its login: a scrypt hash in Meko's own format, and its two-step secret, if on.
type LegacyOwner struct {
	Username   string `json:"username"`
	PWScrypt   string `json:"pw_scrypt"`
	TOTPSecret string `json:"totp_secret"`
}

// HandoffTimeout bounds `<command> _handoff --json`.
var HandoffTimeout = 15 * time.Second

// Handoff runs `<command> _handoff --json` for the data folder at root. An extension too old to have
// the command fails it, and is asked again later.
func (e Entry) Handoff(root string) (Handoff, error) {
	ctx, cancel := context.WithTimeout(context.Background(), HandoffTimeout)
	defer cancel()
	var out, errOut bytes.Buffer
	cmd := exec.CommandContext(ctx, e.Command, "_handoff", "--json") //nolint:gosec // G204: see run
	cmd.Stdout, cmd.Stderr, cmd.Env = &out, &errOut, CommandEnv(root, e.Command)
	if err := cmd.Run(); err != nil {
		first, _, _ := bytes.Cut(bytes.TrimSpace(errOut.Bytes()), []byte("\n"))
		return Handoff{}, fmt.Errorf("%s _handoff: %w (%s)", e.Command, err, first)
	}
	var h Handoff
	if err := json.Unmarshal(out.Bytes(), &h); err != nil {
		return Handoff{}, fmt.Errorf("%s _handoff: unexpected reply: %w", e.Command, err)
	}
	if h.Socket == "" {
		return Handoff{}, fmt.Errorf("%s _handoff named no socket", e.Command)
	}
	return h, nil
}
