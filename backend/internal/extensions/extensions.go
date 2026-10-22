// Package extensions keeps the list of extensions this Meko runs, in extensions.json in the data
// folder, and runs their own commands to start and stop them. An extension has its own service and
// its own updates; Meko installs it (meko install_extension), starts it with meko start and stops
// it with meko stop, and never restarts it when Meko itself restarts or updates.
//
// The contract is the extension's CLI: `<command> start`, `<command> stop` and `<command> status`.
// AgentK's agentk is the first.
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
	},
}

// Catalog says how to find and install a known extension.
type Catalog struct {
	Title       string
	Command     string   // the program's name, looked for on PATH and in ~/.local/bin
	OldCommands []string // names it had before, looked for after Command
	TagPrefixes []string // its release tags, newest naming first
	Installer   string   // the release asset that installs it
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

// Timeouts for an extension's commands. Stopping AgentK waits for its server and its sessions.
var (
	StartTimeout = 2 * time.Minute
	StopTimeout  = 2 * time.Minute
)

// Run runs `<command> <action>` for one extension, with its output passed through.
func (e Entry) Run(action string, stdout, stderr io.Writer) error {
	d := StartTimeout
	if action == "stop" {
		d = StopTimeout
	}
	ctx, cancel := context.WithTimeout(context.Background(), d)
	defer cancel()
	// G204: the extension's own command, from the registry.
	cmd := exec.CommandContext(ctx, e.Command, action) //nolint:gosec
	cmd.Stdout, cmd.Stderr = stdout, stderr
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return fmt.Errorf("%s %s took longer than %s", e.Command, action, d)
		}
		return fmt.Errorf("%s %s: %w", e.Command, action, err)
	}
	return nil
}

// Status runs `<command> status` quietly and reports whether it succeeded, with its first line.
func (e Entry) Status() (bool, string) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	var out bytes.Buffer
	cmd := exec.CommandContext(ctx, e.Command, "status") //nolint:gosec // G204: see Run
	cmd.Stdout, cmd.Stderr = &out, &out
	err := cmd.Run()
	first, _, _ := bytes.Cut(bytes.TrimSpace(out.Bytes()), []byte("\n"))
	return err == nil, string(first)
}

// Find looks for the extension's program by its name, then by its old ones (FindCommand). It
// returns the absolute path, or "".
func (c Catalog) Find(home string) string {
	for _, name := range append([]string{c.Command}, c.OldCommands...) {
		if p := FindCommand(name, home); p != "" {
			return p
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
	p := filepath.Join(home, ".local", "bin", name)
	if fi, err := os.Stat(p); err == nil && !fi.IsDir() && fi.Mode()&0o111 != 0 {
		return p
	}
	return ""
}
