// Package extensions keeps the list of extensions this Meko runs, in extensions.json in the data
// folder, and runs their own commands to start and stop them. An extension has its own service and
// its own updates; Meko installs it (meko install_extension), starts it with meko start and stops
// it with meko stop, and never restarts it when Meko itself restarts or updates.
//
// The contract is the extension's CLI: `<command> start`, `<command> stop` and `<command> status`,
// and its uninstall (Catalog.UninstallArgs) for meko destroy --with-extensions. Every command Meko
// runs of an extension gets MEKO_HOME set to Meko's data folder, so the extension knows where its
// own folder in it is (Dir). AgentK's agentk is the first.
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
)

// Known are the extensions meko install_extension can install, by name.
var Known = map[string]Catalog{
	"agentk": {
		Title:   "AgentK",
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
}

// Catalog says how to find and install a known extension.
type Catalog struct {
	Title       string
	Command     string   // the program's name, looked for on PATH and in ~/.local/bin
	OldCommands []string // names it had before, looked for after Command
	TagPrefixes []string // its release tags, newest naming first
	Installer   string   // the release asset that installs it
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
)

// Env is the environment an extension's command runs with: Meko's own, with MEKO_HOME naming
// Meko's data folder at root (the last value of a key is the one a program sees).
func Env(root string) []string { return append(os.Environ(), paths.HomeEnv+"="+root) }

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
	cmd.Stdout, cmd.Stderr, cmd.Env = stdout, stderr, Env(root)
	if err := cmd.Run(); err != nil {
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
	cmd.Stdout, cmd.Stderr, cmd.Env = &out, &out, Env(root)
	err := cmd.Run()
	first, _, _ := bytes.Cut(bytes.TrimSpace(out.Bytes()), []byte("\n"))
	return err == nil, string(first)
}

// Find looks for the extension's program by its name, then by its old ones (FindCommand), then
// in the app/current/bin of its release install: its folder in Meko's data folder at root (Dir),
// then its old homes. It returns the absolute path, or "".
func (c Catalog) Find(name, home, root string) string {
	names := append([]string{c.Command}, c.OldCommands...)
	for _, n := range names {
		if p := FindCommand(n, home); p != "" {
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
	cmd.Stdout, cmd.Stderr, cmd.Env = &out, &errOut, Env(root)
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
