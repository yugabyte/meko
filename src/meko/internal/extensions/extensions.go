// Package extensions keeps the list of extensions this Meko runs, in extensions.json in the data
// folder, and runs their own commands to start and stop them. An extension has its own service;
// Meko installs it from its releases (meko extension install), updates it (Update), runs probation
// for a new version (meko _launch), starts it with meko start and stops it with meko stop, and never
// restarts it when Meko itself restarts or updates.
//
// The contract is the extension's CLI: `<command> start`, `<command> stop`, `<command> status`,
// `<command> install` (pointing it at this Meko), `<command> version --json`, and the catalog's
// RestartArgs, CheckArgs, BusyArgs, RemoveArgs and UninstallArgs; and its launcher, which hands the commands in
// Probation to meko _launch. Every command Meko runs of an extension gets MEKO_HOME set to Meko's
// data folder, so the extension knows where its own folder in it is (Dir). AgentK's program is
// me_kode; Presenter's me_present. Their names start with me_, so a process list and the services
// say they're Meko's. Neither is a command on PATH: people run them through Meko, as meko kode and
// meko present (Catalog.Verb), which runs the registered program.
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
	"sync"
	"time"

	"github.com/BurntSushi/toml"

	"github.com/yugabyte/meko/src/meko/internal/paths"
	"github.com/yugabyte/meko/src/meko/internal/update"
)

// Known are the extensions meko extension install can install, by name. Both are Managed: Meko
// installs and updates their releases. A build from source on PATH is used as it is, and never
// updated.
var Known = map[string]Catalog{
	"agentk": {
		Title: "AgentK",
		Description: "Runs Claude Code, OpenCode and Kiro sessions across your projects, from task to merged PR, " +
			"with Meko as their shared memory.",
		Summary: "runs coding agents across your projects, from task to merged PR",
		Command: "me_kode",
		Verb:    "kode",
		// Its names before, newest first: me_koder, agentk, and agent-k-local before the rename to
		// AgentK. Its releases still carry all three, as links to me_kode, for the services and older
		// Mekos that run them; Rename moves an install onto the new name.
		OldCommands: []string{"me_koder", "agentk", "agent-k-local"},
		// Its releases are tagged agentk-v<version>, and v<version> before releases were tagged by product.
		TagPrefixes: []string{"agentk-v", "v"},
		Managed:     true,
		// Its home before it moved into Meko's data folder, where an install that hasn't moved yet runs.
		OldHomes:      []string{".agent-k-local"},
		RemoveArgs:    []string{"uninstall", "--non-interactive"},
		UninstallArgs: []string{"uninstall", "--purge", "--delete-worktrees", "--non-interactive"},
		WebUI:         true,
		// An update restarts only its server: its session host, and every session in it, keeps going.
		RestartArgs: []string{"restart"},
		CheckArgs:   [][]string{{"selftest"}},
		Probation:   []string{"serve"},
		// 75: another server holds its home, which restarting won't change; not a failed start.
		CleanExits: []int{75},
		// Its own updater kept update-pending.json and bad-versions in its home.
		LegacyRecords: true,
		// Its session host names the version it started from; it outlives server updates.
		RunningRecords: []string{"run/host.json"},
		// Its own [update] channel = "off" keeps Update automatically off until it's turned on.
		LegacyUpdatesOff: "config.toml",
	},
	// Presenter (extensions/present): interactive, branching presentations built from datapacks.
	// Its program is me_present. Its releases (present-v<version>) carry
	// present-<version>-<os>-<arch>.tar.gz and SHA256SUMS. A build from source on PATH is used as
	// it is, and never updated.
	"present": {
		Title: "Presenter",
		Description: "Builds presentations that branch with the room from your datapacks: Claude drafts the story " +
			"map and the slides, and you pick each path live.",
		Summary:       "presentations that branch with the room, built from your datapacks",
		Command:       "me_present",
		Verb:          "present",
		TagPrefixes:   []string{"present-v"},
		Managed:       true,
		RemoveArgs:    []string{"uninstall", "--non-interactive"},
		UninstallArgs: []string{"uninstall", "--purge", "--non-interactive"},
		WebUI:         true,
		Probation:     []string{"serve"},
		// A presentation is live: an automatic update waits for the next check.
		BusyArgs: []string{"_busy"},
	},
}

// KnownNames lists the extensions meko extension install knows, in order.
func KnownNames() []string {
	names := make([]string, 0, len(Known))
	for n := range Known {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// UserCommand is how people run the extension's command args: meko <Verb> args (meko kode status),
// or its program's name for one Meko has no verb for.
func (c Catalog) UserCommand(args string) string {
	if c.Verb != "" {
		return "meko " + c.Verb + " " + args
	}
	return c.Command + " " + args
}

// Catalog says how to find and install a known extension.
type Catalog struct {
	Title       string
	Description string   // what it does, in a sentence, for the web UI's Extensions page
	Summary     string   // what it does, in a few lowercase words, for meko install's picker
	Command     string   // the program's name: a release's bin/<Command>, or a build from source's on PATH
	Verb        string   // the meko command that runs it: meko <Verb> <its command>
	OldCommands []string // names it had before, looked for after Command
	TagPrefixes []string // its release tags, newest naming first
	// Managed says Meko installs it from its releases' tarballs into App and updates it: the
	// <name>-<version>-<os>-<arch>.tar.gz (named for the extension, holding bin/<Command>) and
	// SHA256SUMS of the newest release with TagPrefixes. After unpacking, Meko runs `<command>
	// install`, which sets it up (its services, its questions).
	Managed bool
	// RestartArgs, when set, restart a running extension into the version app/current names, and
	// return once it answers: an update switches while it runs, then runs them. Without them an
	// update stops it, switches, and starts it.
	RestartArgs []string
	// CheckArgs are commands run on a staged release's binary before it's switched to.
	CheckArgs [][]string
	// Probation are the commands (their first argument) meko _launch runs as a parent: it counts a
	// new version's starts, rolls back one that fails MaxFailedStarts times, and finishes the update
	// once it has run update.HealthyAfter.
	Probation []string
	// CleanExits are exit codes that aren't failed starts, besides 0.
	CleanExits []int
	// BusyArgs, when set, ask whether it's in use: exit 0 means busy, and an automatic update waits
	// for the next check. Updates someone asks for don't ask.
	BusyArgs []string
	// LegacyRecords says its own updater once kept update-pending.json and bad-versions in Dir,
	// which App reads too (update.App.Legacy).
	LegacyRecords bool
	// RunningRecords are files in Dir where a running process names the version it runs from
	// (update.App.Running): pruning keeps it.
	RunningRecords []string
	// LegacyUpdatesOff is a TOML file in Dir whose [update] channel = "off" (its own updater's
	// switch) makes Update automatically start off, until it's set (AutoUpdate).
	LegacyUpdatesOff string
	// OldHomes are folders under the home folder a release install of it lived in before its home
	// moved into Meko's data folder (Dir); Find looks for the command in their app/current/bin.
	OldHomes []string
	// RemoveArgs uninstall it without asking and keep its data: stop it and remove its services,
	// what meko extension delete runs.
	RemoveArgs []string
	// UninstallArgs uninstall it and delete all its data without asking: what meko destroy runs
	// when it removes the extensions too, and meko extension delete --purge.
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
	return c.AppIn(filepath.Join(Dir(root, name), "app"), name)
}

// AppIn is App for the app folder at appRoot, which meko _launch knows from the binary it runs.
func (c Catalog) AppIn(appRoot, name string) update.App {
	a := update.App{Root: appRoot, Name: c.Command, OldNames: c.OldCommands, Package: name, Private: true,
		Checks: c.CheckArgs}
	if c.LegacyRecords {
		a.Legacy = filepath.Dir(appRoot)
	}
	for _, f := range c.RunningRecords {
		a.Running = append(a.Running, filepath.Join(filepath.Dir(appRoot), f))
	}
	return a
}

// releaseOf is the version of the release install command runs (through links), and whether that
// release is in this data folder's app folder. A command that isn't a release install (a build from
// source) has no version.
func (c Catalog) releaseOf(root, name, command string) (string, bool) {
	app, v, ok := update.Of(command)
	if !ok {
		return "", false
	}
	// Compare the folders, not their paths: Meko's --data-dir and the command's links can spell one
	// folder two ways (a bind mount, or macOS's /tmp and /private/tmp).
	return v, paths.SameDir(app.Root, c.App(root, name).Root)
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
	// AutoUpdate is the Extensions page's Update automatically; missing is on.
	AutoUpdate *bool `json:"auto_update,omitempty"`
}

// Auto reports whether Meko installs its updates on its own, as the entry says: missing is on.
// Catalog.AutoUpdate also reads an extension's own old switch.
func (e Entry) Auto() bool { return e.AutoUpdate == nil || *e.AutoUpdate }

// AutoUpdate reports whether Meko installs an installed extension's updates on its own: the entry's
// switch, or while that's unset, off when the extension's own updater was turned off
// (LegacyUpdatesOff), else on.
func (c Catalog) AutoUpdate(root, name string, e Entry) bool {
	if e.AutoUpdate != nil || c.LegacyUpdatesOff == "" {
		return e.Auto()
	}
	var old struct {
		Update struct {
			Channel string `toml:"channel"`
		} `toml:"update"`
	}
	if _, err := toml.DecodeFile(filepath.Join(Dir(root, name), c.LegacyUpdatesOff), &old); err != nil {
		return true
	}
	return old.Update.Channel != "off"
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

// Damaged says a registered extension is damaged: its command is missing, and Catalog.Find doesn't
// find it again. It follows the extension's title in a row or an error.
func Damaged(name, command string) string {
	return fmt.Sprintf("is damaged (%s is missing); meko extension delete %s forgets it", command, name)
}

// FindAgain looks for a registered extension whose command is missing (Catalog.Find) and saves
// where it found it. It returns the command, or "" when the extension is damaged.
func (r *Registry) FindAgain(name, home, root string) string {
	cat, ok := Known[name]
	if !ok {
		return ""
	}
	cmd := cat.Find(name, home, root)
	if cmd == "" {
		return ""
	}
	e := r.Entries[name]
	e.Command = cmd
	r.Entries[name] = e
	_ = r.Save() // found again next time if this fails
	return cmd
}

// Rename moves release installs of this data folder's extensions onto running app/current's
// program by its current name, with no command on PATH: the installed versions get the new name
// (update.App.AdoptAll), the registry entry runs app/current/bin/<command> instead of a
// ~/.local/bin link into this app folder (any of its names, from before extensions were run through
// meko <verb>) or an old name, and those links go. Another data folder's links, and a build from
// source, are left alone. Meko's server runs it at start, since updates never run an extension's
// install.
func (r *Registry) Rename(root, home string) error {
	var errs []error
	for _, name := range r.Names() {
		cat, ok := Known[name]
		if !ok || !cat.Managed {
			continue
		}
		app := cat.App(root, name)
		if app.Current() == "" {
			continue // not a release install of this data folder
		}
		if err := app.AdoptAll(); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", cat.Title, err))
			continue
		}
		e := r.Entries[name]
		_, ours := cat.releaseOf(root, name, e.Command)
		if e.Command != app.CurrentExe() && (ours || (cat.isOldName(e.Command) && !executable(e.Command))) {
			e.Command = app.CurrentExe()
			r.Entries[name] = e
			if err := r.Save(); err != nil {
				errs = append(errs, err)
				continue
			}
		}
		errs = append(errs, cat.UnlinkCommands(app, home))
	}
	return errors.Join(errs...)
}

// isOldName reports whether command is named one of the extension's old command names.
func (c Catalog) isOldName(command string) bool {
	for _, o := range c.OldCommands {
		if filepath.Base(command) == o {
			return true
		}
	}
	return false
}

// UnlinkCommands removes the ~/.local/bin links, by any of the extension's names, that point into
// app (update.App.UnlinkCommandAs): Meko once made them, and people run it as meko <verb> now.
func (c Catalog) UnlinkCommands(app update.App, home string) error {
	names := append([]string{c.Command}, c.OldCommands...)
	errs := make([]error, 0, len(names))
	for _, n := range names {
		errs = append(errs, app.UnlinkCommandAs(home, n))
	}
	return errors.Join(errs...)
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
	return e.runAction(root, action, nil, stdout, stderr)
}

// SummaryEnv, set to 1 for the start and stop meko start and meko stop run, asks the extension to
// print one row in Meko's summary format instead of its usual lines (docs/install.md, "The summary
// rows").
const SummaryEnv = "MEKO_SUMMARY"

// RunSummary runs `<command> <action>` as Run does, with SummaryEnv set: for meko start and stop.
func (e Entry) RunSummary(root, action string, stdout, stderr io.Writer) error {
	return e.runAction(root, action, []string{SummaryEnv + "=1"}, stdout, stderr)
}

func (e Entry) runAction(root, action string, env []string, stdout, stderr io.Writer) error {
	d := StartTimeout
	if action == "stop" {
		d = StopTimeout
	}
	return e.run(root, []string{action}, d, env, stdout, stderr)
}

// RunArgs runs `<command> <args>` for one extension, bounded by d, with its output passed through.
func (e Entry) RunArgs(root string, args []string, d time.Duration, stdout, stderr io.Writer) error {
	return e.run(root, args, d, nil, stdout, stderr)
}

// BusyTimeout bounds `<command> <BusyArgs>`.
var BusyTimeout = 15 * time.Second

// Busy reports whether the extension says it's in use (its catalog's BusyArgs exit 0). One without
// BusyArgs, or that doesn't answer in time, isn't.
func (c Catalog) Busy(root string, e Entry) bool {
	if len(c.BusyArgs) == 0 {
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), BusyTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, e.Command, c.BusyArgs...) //nolint:gosec // G204: see run
	cmd.Env = CommandEnv(root, e.Command)
	return cmd.Run() == nil
}

// Uninstall runs `<command> <args>` (the catalog's RemoveArgs or UninstallArgs), with its output
// passed through.
func (e Entry) Uninstall(root string, args []string, stdout, stderr io.Writer) error {
	return e.run(root, args, UninstallTimeout, nil, stdout, stderr)
}

func (e Entry) run(root string, args []string, d time.Duration, env []string, stdout, stderr io.Writer) error {
	ctx, cancel := context.WithTimeout(context.Background(), d)
	defer cancel()
	// G204: the extension's own command, from the registry.
	cmd := exec.CommandContext(ctx, e.Command, args...) //nolint:gosec
	cmd.Stdout, cmd.Stderr, cmd.Env, cmd.WaitDelay = stdout, stderr, append(CommandEnv(root, e.Command), env...), PipeWait
	if err := cmd.Run(); err != nil && !errors.Is(err, exec.ErrWaitDelay) {
		what := e.Command + " " + strings.Join(args, " ")
		if ctx.Err() != nil {
			return fmt.Errorf("%s took longer than %s", what, d)
		}
		return fmt.Errorf("%s: %w", what, err)
	}
	return nil
}

// StatusTimeout bounds each `<command> status` that meko status runs (Registry.Statuses): a running
// extension answers in well under a second, and one that doesn't answer in time is shown as such.
var StatusTimeout = 3 * time.Second

// statusPipeWait is how long a status's output is read after it exits or is killed, at most.
const statusPipeWait = time.Second

// StatusResult is what `<command> status` said: whether it succeeded, its first line, and whether
// it was killed for taking longer than it was given (then Running is false and Line is whatever it
// printed by then).
type StatusResult struct {
	Running  bool
	Line     string
	TimedOut bool
}

// Status runs `<command> status` quietly and reports whether it succeeded, with its first line. It
// waits up to 15 s: an update decides from it whether to stop the extension, and a slow one taken
// for stopped would be switched while it runs.
func (e Entry) Status(root string) (bool, string) {
	r := e.StatusWithin(context.Background(), root, 15*time.Second)
	return r.Running, r.Line
}

// StatusWithin runs `<command> status` quietly, bounded by d, or until ctx is done. A process the
// status started that still holds its output after the status exits (the launchers exec Node, so
// it's one Node started) isn't killed, and isn't waited for more than statusPipeWait: a status
// prints a few lines, and has printed them by the time it exits. What the exit code says stands
// when the status exited by itself, even if waiting for its output ran past d.
func (e Entry) StatusWithin(ctx context.Context, root string, d time.Duration) StatusResult {
	ctx, cancel := context.WithTimeout(ctx, d)
	defer cancel()
	var out bytes.Buffer
	cmd := exec.CommandContext(ctx, e.Command, "status") //nolint:gosec // G204: see run
	cmd.Stdout, cmd.Stderr, cmd.Env = &out, &out, CommandEnv(root, e.Command)
	cmd.WaitDelay = min(PipeWait, statusPipeWait)
	_ = cmd.Run()
	first, _, _ := bytes.Cut(bytes.TrimSpace(out.Bytes()), []byte("\n"))
	ps := cmd.ProcessState
	exited := ps != nil && ps.Exited()
	return StatusResult{Running: exited && ps.Success(), Line: string(first), TimedOut: !exited && ctx.Err() != nil}
}

// Statuses starts `<command> status` for every registered extension at once, each bounded by d, and
// returns a function that waits for them all and gives each one's result by name. Canceling ctx
// stops the ones still running.
func (r *Registry) Statuses(ctx context.Context, root string, d time.Duration) func() map[string]StatusResult {
	var mu sync.Mutex
	var wg sync.WaitGroup
	out := make(map[string]StatusResult, len(r.Entries))
	for name, e := range r.Entries {
		wg.Go(func() {
			res := e.StatusWithin(ctx, root, d)
			mu.Lock()
			out[name] = res
			mu.Unlock()
		})
	}
	return func() map[string]StatusResult {
		wg.Wait()
		return out
	}
}

// Find looks for the extension's program: a build from source on PATH or in ~/.local/bin, by its
// name, then by its old ones (FindCommand), else app/current/bin/<name> of its release install, in
// its folder in Meko's data folder at root (Dir), then in its old homes. It returns the absolute
// path, or "". A command there that runs a release install (a link Meko once made, which may be
// another data folder's) is passed over for the release's own path.
func (c Catalog) Find(name, home, root string) string {
	names := append([]string{c.Command}, c.OldCommands...)
	for _, n := range names {
		if p := FindCommand(n, home); p != "" {
			if v, _ := c.releaseOf(root, name, p); c.Managed && v != "" {
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
