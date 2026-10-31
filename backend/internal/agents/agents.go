// Package agents connects the agents on this computer (Claude Code, Codex, Kiro CLI, OpenCode and
// Cursor) to Meko. It finds each one (its command on PATH, in its usual folders or on the user's login
// shell's PATH), reads from the agent's own config whether its meko MCP server reaches Meko's target, and
// adds or removes that server each agent's own way. Meko keeps no record of its own: the configs are
// read every time, since the user and the agents edit them too. The CLI (meko agent), the server's
// /api/v1/agents routes and the Meko Cloud link's changes (Move) share it (docs/api.md#agents).
package agents

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

// ServerName is Meko's MCP server's name in every agent: AgentK recognizes Meko's tools by it
// (mcp__meko__*).
const ServerName = "meko"

// The targets an agent connects to.
const (
	KindLocal = "local" // this Meko, with its client token
	// KindCloud is a linked Meko Cloud's MCP URL and key, where a Meko before agents stayed on this
	// Meko connected them: only ever a move's from.
	KindCloud = "cloud"
)

// An installed agent's state.
const (
	StateConnected = "connected" // a meko server at the target, sending its token or key
	StateStale     = "stale"     // a meko server for one of Meko's own that isn't exactly the target
	StateElsewhere = "elsewhere" // a meko server at any other URL
	StateNone      = "none"      // no meko server
)

// Target is where agents connect: this Meko's MCP URL and client token, linked to Meko Cloud or
// not (this Meko's /mcp sends their calls there). A move's from may be a Meko Cloud link's.
type Target struct {
	Kind  string
	URL   string
	Token string
}

// ours says whether an MCP URL is one of Meko's own: this computer's loopback, where Meko listens,
// or the linked Meko Cloud.
func (t Target) ours(u string) bool {
	return onThisComputer(u) || t.Kind == KindCloud && sameURL(u, t.URL)
}

// owns says whether a meko server is one Meko put there for the target: on this computer's
// loopback, or at a Meko Cloud link's URL with that link's key. One at Meko Cloud with another key
// is the user's own.
func (t Target) owns(s *server) bool {
	return onThisComputer(s.url) || t.Kind == KindCloud && sameURL(s.url, t.URL) && !s.unresolved &&
		bearerToken(s.auth) == t.Token
}

// state is what a meko server (nil with none) is to the target.
func (t Target) state(s *server) string {
	switch {
	case s == nil:
		return StateNone
	case sameURL(s.url, t.URL) && (s.unresolved || bearerToken(s.auth) == t.Token):
		return StateConnected
	case t.ours(s.url):
		return StateStale
	}
	return StateElsewhere
}

// blank takes the target's token out of text, for an error an agent's CLI echoed.
func (t Target) blank(text string) string {
	if t.Token == "" {
		return text
	}
	return strings.ReplaceAll(text, t.Token, "<token>")
}

func sameURL(a, b string) bool { return strings.TrimRight(a, "/") == strings.TrimRight(b, "/") }

// onThisComputer says whether an MCP URL is on this computer's loopback, where Meko listens.
func onThisComputer(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil {
		return false
	}
	h := u.Hostname()
	return h == "localhost" || net.ParseIP(h).IsLoopback()
}

// server is an agent's meko MCP server as its config has it: the URL and the Authorization header.
// unresolved says the header names an environment variable Meko can't resolve (Claude Code's
// ${VAR}): the token can't be checked, so a server at the target counts as connected.
type server struct {
	url, auth  string
	unresolved bool
}

var bearer = regexp.MustCompile(`(?i)^\s*Bearer\s+(\S+)\s*$`)

// bearerToken is an Authorization header's bearer token, or "".
func bearerToken(auth string) string {
	if m := bearer.FindStringSubmatch(auth); m != nil {
		return m[1]
	}
	return ""
}

// shownURL is a URL as Meko reports it: scheme, host and path, without credentials, a query or a
// fragment, which may hold another server's key. "" stays "".
func shownURL(raw string) string {
	if raw == "" {
		return ""
	}
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Scheme == "" || u.Host == "" {
		return "a URL Meko can't read"
	}
	return u.Scheme + "://" + u.Host + u.EscapedPath()
}

// Agent is an agent Meko knows: its name on the command line, what people call it, its command, and
// how Meko reads, adds and removes its meko server.
type Agent struct {
	Name, Title, Command string
	// also are other commands that mean it's installed (Cursor's CLI beside its editor).
	also []string
	// folders are its own install folders, under the home folder, beside the usual ones; apps are
	// under Applications (inside a macOS app).
	folders, apps []string
	// read returns the user-level meko server (nil with none) and notes on others that win over it
	// (Claude Code's folders).
	read func(t Target) (*server, []string, error)
	// connect points its meko server at the target, run with the command found; notes are what the
	// user should know.
	connect func(ctx context.Context, cmd string, t Target) ([]string, error)
	// disconnect removes its meko server, which is one of Meko's own.
	disconnect func(ctx context.Context, cmd string, t Target) ([]string, error)
}

// Catalog is every agent Meko knows, in the order lists show them.
var Catalog = []Agent{
	{Name: "claude", Title: "Claude Code", Command: "claude", folders: []string{".claude/local"},
		read: readClaude, connect: connectClaude, disconnect: disconnectClaude},
	{Name: "codex", Title: "Codex", Command: "codex", read: readCodex, connect: connectCodex,
		disconnect: disconnectCodex},
	{Name: "kiro", Title: "Kiro CLI", Command: "kiro-cli", read: kiro.read, connect: kiro.connect,
		disconnect: kiro.disconnect},
	{Name: "opencode", Title: "OpenCode", Command: "opencode", folders: []string{".opencode/bin"},
		read: readOpenCode, connect: connectOpenCode, disconnect: disconnectOpenCode},
	{Name: "cursor", Title: "Cursor", Command: "cursor", also: []string{"cursor-agent"}, apps: []string{cursorApp},
		read: cursor.read, connect: cursor.connect, disconnect: cursor.disconnect},
}

// Names are the catalog's names.
func Names() []string {
	out := make([]string, len(Catalog))
	for i, a := range Catalog {
		out[i] = a.Name
	}
	return out
}

// Lookup finds the agent called name.
func Lookup(name string) (Agent, bool) {
	for _, a := range Catalog {
		if a.Name == name {
			return a, true
		}
	}
	return Agent{}, false
}

// SystemFolders are where installers put commands for everyone, looked in after PATH and the home
// folder's ~/.local/bin: a service runs with a thin PATH. Tests empty it.
var SystemFolders = []string{"/usr/local/bin", "/opt/homebrew/bin"}

// Applications is where macOS apps are installed, for an agent's apps folders. Tests empty it.
var Applications = "/Applications"

// Find is the agent's command, or else one of also: on PATH, else in its usual folders, else on the
// user's login shell's PATH (LoginShell), as an absolute path. The shell runs only when no command
// is found otherwise.
func (a Agent) Find() (string, bool) {
	commands := append([]string{a.Command}, a.also...)
	for _, c := range commands {
		if p, ok := a.find(c); ok {
			return p, true
		}
	}
	for _, c := range commands {
		if p, ok := findIn(loginPath(), c); ok {
			return p, true
		}
	}
	return "", false
}

func (a Agent) find(command string) (string, bool) {
	if p, err := lookPath(command); err == nil {
		return p, true
	}
	var dirs []string
	if home, err := os.UserHomeDir(); err == nil {
		dirs = append(dirs, filepath.Join(home, ".local", "bin"))
		for _, f := range a.folders {
			dirs = append(dirs, filepath.Join(home, f))
		}
	}
	if Applications != "" {
		for _, f := range a.apps {
			dirs = append(dirs, filepath.Join(Applications, f))
		}
	}
	return findIn(append(dirs, SystemFolders...), command)
}

// findIn is the first executable file called name in the folders.
func findIn(dirs []string, name string) (string, bool) {
	for _, d := range dirs {
		p := filepath.Join(d, name)
		if fi, err := os.Stat(p); err == nil && fi.Mode().IsRegular() && fi.Mode().Perm()&0o111 != 0 {
			return p, true
		}
	}
	return "", false
}

// Row is an installed agent as a list shows it. URL is its meko server's, null with none. Notes are
// worth showing: Claude Code's folders whose own meko server wins over the user scope's. It never
// holds a token or key.
type Row struct {
	Name    string   `json:"name"`
	Title   string   `json:"title"`
	Command string   `json:"command"` // ~ for the home folder
	State   string   `json:"state"`
	URL     *string  `json:"url"`
	Notes   []string `json:"notes"`
}

// TargetInfo is the target as a list shows it: never its token or key.
type TargetInfo struct {
	Kind string `json:"kind"`
	URL  string `json:"url"`
}

// List is GET /api/v1/agents's data, and meko agent list --json's: the target, and the agents
// installed, in the catalog's order.
type List struct {
	Target TargetInfo `json:"target"`
	Agents []Row      `json:"agents"`
}

// Result is what a connect or disconnect did: the agent's row after it, and notes on what the user
// should know. Changed says whether its config changed.
type Result struct {
	Agent   Row      `json:"agent"`
	Notes   []string `json:"notes"`
	Changed bool     `json:"-"`
}

// Error codes, which the routes answer with.
const (
	CodeUnknown      = "UNKNOWN_AGENT"       // 404
	CodeNotInstalled = "AGENT_NOT_INSTALLED" // 409
	CodeFailed       = "AGENT_CLI_FAILED"    // 502: the agent's CLI, or its config file, failed
)

// Error is a connect or disconnect refused or failed, with a message for people that never holds the
// token or key.
type Error struct {
	Code string
	msg  string
}

func (e *Error) Error() string { return e.msg }

// Manager connects and disconnects the agents. Connect, Disconnect and Move hold a lock across
// processes (the CLI's and the server's), so two never edit an agent's config at once.
type Manager struct {
	// Lock is the lock file (run/agents.lock in the data folder); "" takes none.
	Lock string
	// Target is where agents connect now.
	Target func() (Target, error)
}

// LockWait is how long Connect, Disconnect and Move wait for another's lock.
var LockWait = 2 * time.Minute

// lock takes the lock file's flock, waiting for one another process (or this one) holds.
func (m *Manager) lock(ctx context.Context) (func(), error) {
	if m.Lock == "" {
		return func() {}, nil
	}
	if err := os.MkdirAll(filepath.Dir(m.Lock), 0o700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(m.Lock, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, LockWait)
	defer cancel()
	for {
		err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB)
		if err == nil {
			return func() { _ = f.Close() }, nil // closing the file lets the flock go
		}
		if !errors.Is(err, unix.EWOULDBLOCK) {
			_ = f.Close()
			return nil, err
		}
		select {
		case <-ctx.Done():
			_ = f.Close()
			return nil, errors.New("another meko is connecting or disconnecting an agent: try again when it's done")
		case <-time.After(50 * time.Millisecond):
		}
	}
}

// List reads the installed agents' state.
func (m *Manager) List() (List, error) {
	t, err := m.Target()
	if err != nil {
		return List{}, err
	}
	out := List{Target: TargetInfo{Kind: t.Kind, URL: shownURL(t.URL)}, Agents: []Row{}}
	for _, a := range Catalog {
		if cmd, ok := a.Find(); ok {
			out.Agents = append(out.Agents, a.row(cmd, t))
		}
	}
	return out, nil
}

// row reads an installed agent's state. A config that can't be read reads as no meko server, with a
// note saying why.
func (a Agent) row(cmd string, t Target) Row {
	r := Row{Name: a.Name, Title: a.Title, Command: tilde(cmd), Notes: []string{}}
	s, notes, err := a.read(t)
	if err != nil {
		r.State = StateNone
		r.Notes = append(r.Notes, fmt.Sprintf("Meko can't read %s's settings: %s", a.Title, t.blank(err.Error())))
		return r
	}
	r.State = t.state(s)
	if s != nil {
		u := shownURL(s.url)
		r.URL = &u
	}
	r.Notes = append(r.Notes, notes...)
	return r
}

// tilde writes the home folder in a path as ~.
func tilde(p string) string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" || home == "/" {
		return p
	}
	if rest, ok := strings.CutPrefix(p, home+string(filepath.Separator)); ok {
		return "~/" + rest
	}
	return p
}

// installed finds a known, installed agent, or says why not.
func installed(name string) (Agent, string, error) {
	a, ok := Lookup(name)
	if !ok {
		return Agent{}, "", &Error{Code: CodeUnknown, msg: fmt.Sprintf("Meko doesn't know the agent %q: it knows %s",
			name, strings.Join(Names(), ", "))}
	}
	cmd, ok := a.Find()
	if !ok {
		return Agent{}, "", &Error{Code: CodeNotInstalled, msg: a.Title + " isn't installed"}
	}
	return a, cmd, nil
}

// Connect points the agent's meko server at the target, replacing one it has.
func (m *Manager) Connect(ctx context.Context, name string) (Result, error) {
	a, cmd, err := installed(name)
	if err != nil {
		return Result{}, err
	}
	// The target is read under the lock: a link change's move holds it, so a connect never writes
	// the target from before a change after the move that followed it.
	unlock, err := m.lock(ctx)
	if err != nil {
		return Result{}, err
	}
	defer unlock()
	t, err := m.Target()
	if err != nil {
		return Result{}, err
	}
	notes, err := a.connectTo(ctx, cmd, t)
	if err != nil {
		return Result{}, &Error{Code: CodeFailed,
			msg: "Couldn't connect " + a.Title + ": " + strings.TrimSuffix(err.Error(), ".")}
	}
	if notes == nil {
		notes = []string{}
	}
	return Result{Agent: a.row(cmd, t), Notes: notes, Changed: true}, nil
}

// connectTo connects the agent and says which server elsewhere it replaced. Its error never holds
// the target's token.
func (a Agent) connectTo(ctx context.Context, cmd string, t Target) ([]string, error) {
	old, _, _ := a.read(t) // unreadable: connect says what's wrong
	notes, err := a.connect(ctx, cmd, t)
	if err != nil {
		return nil, errors.New(t.blank(err.Error()))
	}
	if t.state(old) == StateElsewhere {
		notes = append([]string{"It replaced the meko server at " + shownURL(old.url)}, notes...)
	}
	return notes, nil
}

// Disconnect removes the agent's meko server when it's one of Meko's own (this computer's, or the
// linked Meko Cloud's). One elsewhere is left as it is, with a note, and one with none changes
// nothing.
func (m *Manager) Disconnect(ctx context.Context, name string) (Result, error) {
	a, cmd, err := installed(name)
	if err != nil {
		return Result{}, err
	}
	unlock, err := m.lock(ctx)
	if err != nil {
		return Result{}, err
	}
	defer unlock()
	t, err := m.Target()
	if err != nil {
		return Result{}, err
	}
	failed := func(err error) error {
		return &Error{Code: CodeFailed, msg: "Couldn't disconnect " + a.Title + ": " +
			strings.TrimSuffix(t.blank(err.Error()), ".")}
	}
	s, _, err := a.read(t)
	switch {
	case err != nil:
		return Result{}, failed(err)
	case s == nil:
		return Result{Agent: a.row(cmd, t), Notes: []string{}}, nil
	case !t.ours(s.url):
		return Result{Agent: a.row(cmd, t),
			Notes: []string{a.Title + "'s meko server points at " + shownURL(s.url) + ", not Meko: left as it is"}}, nil
	}
	notes, err := a.disconnect(ctx, cmd, t)
	if err != nil {
		return Result{}, failed(err)
	}
	if notes == nil {
		notes = []string{}
	}
	return Result{Agent: a.row(cmd, t), Notes: notes, Changed: true}, nil
}

// Moves is what a Meko Cloud link change did to the agents: the ones it moved to the new target, the
// ones whose meko server points elsewhere, left as they are, and the ones it couldn't move.
type Moves struct {
	Moved  []Moved  `json:"moved"`
	Left   []Left   `json:"left"`
	Failed []Failed `json:"failed"`
}

// Moved is an agent a move connected to the new target.
type Moved struct {
	Name  string `json:"name"`
	Title string `json:"title"`
}

// Left is an agent whose meko server points elsewhere.
type Left struct {
	Name  string `json:"name"`
	Title string `json:"title"`
	URL   string `json:"url"`
}

// Failed is an agent a move couldn't connect: it shows as stale.
type Failed struct {
	Name  string `json:"name"`
	Title string `json:"title"`
	Error string `json:"error"`
}

// Move connects to the target to every installed agent whose meko server Meko put there for from
// or to (this computer's, or the Meko Cloud link's with its key: owns) and isn't already connected
// to it: a link change moves back to this Meko the agents a Meko before agents stayed on this Meko
// connected to Meko Cloud, which takes the key out of their configs. One elsewhere is left as it
// is. Errors never hold either target's token or key.
func (m *Manager) Move(ctx context.Context, from, to Target) Moves {
	ctx, cancel := context.WithTimeout(ctx, MoveTimeout)
	defer cancel()
	out := Moves{Moved: []Moved{}, Left: []Left{}, Failed: []Failed{}}
	// The states are read under the lock, after any connect or disconnect running now.
	unlock, err := m.lock(ctx)
	if err != nil {
		for _, a := range Catalog {
			if _, ok := a.Find(); ok {
				out.Failed = append(out.Failed, Failed{a.Name, a.Title, err.Error()})
			}
		}
		return out
	}
	defer unlock()
	for _, a := range Catalog {
		cmd, ok := a.Find()
		if !ok {
			continue
		}
		s, _, err := a.read(to)
		switch {
		case err != nil:
			out.Failed = append(out.Failed, Failed{a.Name, a.Title, from.blank(to.blank(err.Error()))})
			continue
		case s == nil || to.state(s) == StateConnected:
			continue
		case !from.owns(s) && !to.owns(s):
			out.Left = append(out.Left, Left{a.Name, a.Title, shownURL(s.url)})
			continue
		}
		if _, err := a.connectTo(ctx, cmd, to); err != nil {
			out.Failed = append(out.Failed, Failed{a.Name, a.Title,
				strings.TrimSuffix(from.blank(err.Error()), ".")})
			continue
		}
		out.Moved = append(out.Moved, Moved{a.Name, a.Title})
	}
	return out
}

// MoveTimeout bounds a Move: the lock's wait, then each agent's connect, which runs its CLI up to
// three times (Claude Code's remove, add, and add-json when the add fails).
var MoveTimeout = LockWait + time.Duration(len(Catalog))*3*CLITimeout
