// Package config loads Meko's settings. Precedence: flags, then MEKO_* environment
// variables, then config.toml in the data folder, then defaults.
package config

import (
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/BurntSushi/toml"

	"github.com/yugabyte/meko/src/meko/internal/paths"
)

// DefaultPort is Meko's one port, for agents, the CLI and browsers alike: the one AgentK listened
// on before Meko served it.
const DefaultPort = 7777

// Config is every setting.
type Config struct {
	Server             Server             `toml:"server"`
	Log                Log                `toml:"log"`
	ContextEngine      ContextEngine      `toml:"context_engine"`
	ConversationSearch ConversationSearch `toml:"conversation_search"`
	Secrets            Secrets            `toml:"secrets"`
	Update             Update             `toml:"update"`
	KnowledgeBase      KnowledgeBase      `toml:"knowledge_base"`
	WebUI              WebUI              `toml:"web_ui"`
	Web                Web                `toml:"web"`
	TLS                TLS                `toml:"tls"`
	CallHome           CallHome           `toml:"call_home"`
	Storage            Storage            `toml:"storage"`
}

// Web is the [web] table: how browsers reach Meko, for the web UI and the extensions' at
// /ext/<name>/, on the one port ([server] port) agents and the CLI use too.
type Web struct {
	// Access is "local" (the default: 127.0.0.1 over HTTP, for requests from this computer) or
	// "remote" (every address: HTTPS for browsers, and plain HTTP for agents and the CLI on this
	// computer). Neither has a login. Also MEKO_WEB_ACCESS.
	Access string `toml:"access"`
	// Name is the host name links to the web UI use; empty is this host's FQDN (remote) or
	// 127.0.0.1 (local).
	Name string `toml:"name"`
	// Enabled is whether browsers are let in at all (true by default): meko ui stop and meko ui
	// start set it in web.toml (SetWebEnabled), and meko install --no-ui sets it off. Off, the port
	// stays open to agents and the CLI, on 127.0.0.1 only.
	Enabled bool `toml:"enabled"`
}

// webTable is web.toml's [web]: Web, with Enabled unset when the file doesn't name it. A port an
// older Meko wrote there (from the handoff, or install) is ignored, and dropped at the next write.
type webTable struct {
	Access  string `toml:"access"`
	Name    string `toml:"name"`
	Enabled *bool  `toml:"enabled"`
}

// webFile is web.toml.
type webFile struct {
	Web *webTable `toml:"web"`
	TLS *TLS      `toml:"tls"`
}

// Web access modes.
const (
	AccessLocal  = "local"
	AccessRemote = "remote"
)

// TLS is the [tls] table: the certificate browsers get with remote access. Empty uses a
// self-signed pair Meko makes in the data folder's certs/ (internal/tlscert).
type TLS struct {
	Cert string `toml:"cert"` // a PEM file
	Key  string `toml:"key"`  // its PEM private key
}

// CallHome is the [call_home] table: the usage stats Meko sends to YugabyteDB's call home service
// (internal/callhome).
type CallHome struct {
	// Enabled is true by default. false here, MEKO_CALL_HOME=false or YB_DISABLE_CALLHOME (yugabyted's
	// switch) turn usage stats off whatever the web UI's setting says; true only leaves it to the web UI.
	// The variables are read from serve's own environment, which under the login service is the unit's
	// or plist's, not the shell's.
	Enabled bool `toml:"enabled"`
	// URL is where they go. Empty is callhome.DefaultURL, from release installs only. Also
	// MEKO_CALL_HOME_URL.
	URL string `toml:"url"`
	// OffBy names what turned them off ("config.toml", "MEKO_CALL_HOME" or "YB_DISABLE_CALLHOME"),
	// for status and the web UI; empty while Enabled. Not a setting.
	OffBy string `toml:"-"`
}

// Where usage stats were turned off, for CallHome.OffBy.
const (
	OffByFile       = "config.toml"
	OffByEnv        = "MEKO_CALL_HOME"
	OffByYugabyteDB = "YB_DISABLE_CALLHOME"
)

// WebUI is the [web_ui] table: where the built web UI is. Empty means the usual place
// (internal/webui).
type WebUI struct {
	Dir string `toml:"dir"` // also MEKO_WEB_UI_DIR
}

// DefaultMaxContentMB is [storage] max_content_mb's default: the content size Meko's search was
// measured at (make bench), and keeps to.
const DefaultMaxContentMB = 200

// Storage is the [storage] table.
type Storage struct {
	// MaxContentMB is the most content Meko keeps, in MB (MiB): memory text, conversation turns,
	// knowledge files and artifacts, across every datapack here. A write past it is refused. Search
	// slows above the default, 200, the size it was measured at; Meko Cloud is for larger datasets.
	// Read at start, so a change needs meko restart.
	MaxContentMB int `toml:"max_content_mb"`
}

// MaxContentBytes is MaxContentMB in bytes.
func (s Storage) MaxContentBytes() int64 { return int64(s.MaxContentMB) << 20 }

// KnowledgeBase is the [knowledge_base] table.
type KnowledgeBase struct {
	// MaxDistance is the farthest (cosine distance, 0 to 2) a knowledge-base result found by meaning
	// alone may be from the query for context_search to keep it; a reranked keyword or hybrid match
	// is kept past it. 0 means the default, tuned for the embedding model (internal/memory).
	MaxDistance float64 `toml:"max_distance"`
}

// Update is the [update] table: how a release install updates itself.
type Update struct {
	// Channel is "stable" (the default) or "off" (no checks at all). Also MEKO_UPDATE_CHANNEL.
	Channel string `toml:"channel"`
	// Auto installs an update when one is found, while Meko runs as a service (default true). With
	// false, updates are only reported (meko status), and meko update installs them. Also
	// MEKO_UPDATE_AUTO.
	Auto bool `toml:"auto"`
	// IntervalHours is how often to check (default 6).
	IntervalHours float64 `toml:"interval_hours"`
	// Repo is the GitHub repo releases come from (default yugabyte/meko). Also
	// MEKO_UPDATE_REPO.
	Repo string `toml:"repo"`
	// RequireSignature refuses a release whose SHA256SUMS isn't signed by one of Meko's release keys
	// (default true). With false, such a release installs with a notice. A signature that doesn't
	// verify is refused either way. Also MEKO_UPDATE_REQUIRE_SIGNATURE.
	RequireSignature bool `toml:"require_signature"`
}

// Secrets is the [secrets] table: where the client token and other secrets are kept.
type Secrets struct {
	// Store is "keychain" (the default on macOS) or "file" (the default elsewhere: files in the data
	// folder, readable only by their owner). Also MEKO_SECRETS. Changing it doesn't move secrets: the
	// client token is made again, and clients set up with the old one need the new one.
	Store string `toml:"store"`
}

// ConversationSearch is the [conversation_search] table.
type ConversationSearch struct {
	// MaxDistance is the farthest (cosine distance, 0 to 2) a past turn may be from the query to
	// be a hit. 0 means the default, tuned for the embedding model (internal/memory).
	MaxDistance float64 `toml:"max_distance"`
}

// Server is the [server] table.
type Server struct {
	// Port is Meko's one port (default 7777): MCP, the REST API, the web UI and the extensions'.
	// Also MEKO_PORT and --port.
	Port int `toml:"port"`
	// Auth is "token" (the default: /mcp and the data routes need the client or admin token) or
	// "none" (they need no credential; every local user can read and write the memories). Also
	// MEKO_AUTH.
	Auth string `toml:"auth"`
}

// Auth modes.
const (
	AuthToken = "token"
	AuthNone  = "none"
)

// ContextEngine is the [context_engine] table: where the context engine and its Node are. Empty means the usual
// places (internal/contextengine).
type ContextEngine struct {
	Node string `toml:"node"` // also MEKO_NODE
	Dir  string `toml:"dir"`  // the built context engine; also MEKO_CONTEXT_ENGINE_DIR
}

// Log is the [log] table.
type Log struct {
	Level string `toml:"level"`
}

// Default is the configuration with nothing set.
func Default() Config {
	return Config{Server: Server{Port: DefaultPort, Auth: AuthToken}, Log: Log{Level: "info"},
		Web: Web{Access: AccessLocal, Enabled: true},
		Update: Update{Channel: "stable", Auto: true, IntervalHours: 6, Repo: "yugabyte/meko",
			RequireSignature: true},
		CallHome: CallHome{Enabled: true}, Storage: Storage{MaxContentMB: DefaultMaxContentMB}}
}

// Overrides are settings given as flags; zero values mean unset.
type Overrides struct {
	Port     int
	LogLevel string
}

// Load reads config.toml at path (missing is fine), then applies env (os.Getenv in production)
// and the flag overrides.
func Load(path string, env func(string) string, o Overrides) (Config, error) {
	c := Default()
	// web.toml first: config.toml's own [web] and [tls] keys win over what Meko chose (AddWeb).
	chosen, err := readWebFile(WebFile(path))
	if err != nil {
		return Config{}, err
	}
	if chosen.Web != nil {
		c.Web = mergeWeb(c.Web, *chosen.Web)
	}
	if chosen.TLS != nil {
		c.TLS = *chosen.TLS
	}
	md, err := toml.DecodeFile(path, &c)
	switch {
	case errors.Is(err, fs.ErrNotExist):
	case err != nil:
		return Config{}, fmt.Errorf("%s: %w", path, err)
	case md.IsDefined("web", "port"):
		return Config{}, fmt.Errorf("%s: %w", path, ErrWebPort)
	default:
		if extra := md.Undecoded(); len(extra) > 0 {
			keys := make([]string, len(extra))
			for i, k := range extra {
				keys[i] = k.String()
			}
			return Config{}, fmt.Errorf("%s: unknown setting %s", path, strings.Join(keys, ", "))
		}
	}
	if v := env("MEKO_PORT"); v != "" {
		p, err := strconv.Atoi(v)
		if err != nil {
			return Config{}, fmt.Errorf("MEKO_PORT=%q isn't a port number", v)
		}
		c.Server.Port = p
	}
	if env("MEKO_WEB_PORT") != "" {
		return Config{}, fmt.Errorf("MEKO_WEB_PORT: %w", ErrWebPort)
	}
	if v := env("MEKO_WEB_ACCESS"); v != "" {
		c.Web.Access = v
	}
	if v := env("MEKO_AUTH"); v != "" {
		c.Server.Auth = v
	}
	if v := env("MEKO_LOG_LEVEL"); v != "" {
		c.Log.Level = v
	}
	if v := env("MEKO_NODE"); v != "" {
		c.ContextEngine.Node = v
	}
	if v := env("MEKO_UPDATE_CHANNEL"); v != "" {
		c.Update.Channel = v
	}
	if v := env("MEKO_UPDATE_AUTO"); v != "" {
		b, err := strconv.ParseBool(v)
		if err != nil {
			return Config{}, fmt.Errorf("MEKO_UPDATE_AUTO=%q isn't true or false", v)
		}
		c.Update.Auto = b
	}
	if v := env("MEKO_UPDATE_REPO"); v != "" {
		c.Update.Repo = v
	}
	if v := env("MEKO_UPDATE_REQUIRE_SIGNATURE"); v != "" {
		b, err := strconv.ParseBool(v)
		if err != nil {
			return Config{}, fmt.Errorf("MEKO_UPDATE_REQUIRE_SIGNATURE=%q isn't true or false", v)
		}
		c.Update.RequireSignature = b
	}
	if v := env("MEKO_SECRETS"); v != "" {
		c.Secrets.Store = v
	}
	if v := env("MEKO_CONTEXT_ENGINE_DIR"); v != "" {
		c.ContextEngine.Dir = v
	}
	if v := env("MEKO_WEB_UI_DIR"); v != "" {
		c.WebUI.Dir = v
	}
	if !c.CallHome.Enabled {
		c.CallHome.OffBy = OffByFile
	}
	if v := env("MEKO_CALL_HOME"); v != "" {
		b, err := strconv.ParseBool(v)
		if err != nil {
			return Config{}, fmt.Errorf("MEKO_CALL_HOME=%q isn't true or false", v)
		}
		c.CallHome.Enabled, c.CallHome.OffBy = b, ""
		if !b {
			c.CallHome.OffBy = OffByEnv
		}
	}
	if YugabyteDBCallHomeOff(env("YB_DISABLE_CALLHOME")) {
		c.CallHome.Enabled, c.CallHome.OffBy = false, OffByYugabyteDB
	}
	if v := env("MEKO_CALL_HOME_URL"); v != "" {
		c.CallHome.URL = v
	}
	if o.Port != 0 {
		c.Server.Port = o.Port
	}
	if o.LogLevel != "" {
		c.Log.Level = o.LogLevel
	}
	return c, c.validate()
}

// pickedPortMark is the comment meko install writes above a port it picked (SetPickedPort), which
// tells that port from one the user chose (PortChosen).
const pickedPortMark = "# Picked by meko install: the port before it was in use by another program"

// ErrPortChosen is SetPickedPort's refusal to move a port the user chose.
var ErrPortChosen = errors.New("config.toml sets [server] port")

// PortChosen reports whether the user chose Meko's port: MEKO_PORT, or a [server] port in
// config.toml at path that meko install didn't pick. Install moves only a port nobody chose.
func PortChosen(path string, env func(string) string) (bool, error) {
	if env("MEKO_PORT") != "" {
		return true, nil
	}
	var c Config
	md, err := toml.DecodeFile(path, &c)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("%s: %w", path, err)
	}
	if !md.IsDefined("server", "port") {
		return false, nil
	}
	b, err := os.ReadFile(path) //nolint:gosec // G304: the data folder's config.toml
	if err != nil {
		return false, err
	}
	return pickedPortLine(strings.Split(string(b), "\n")) < 0, nil
}

// pickedPortLine is the index of the port line under pickedPortMark in [server], or -1.
func pickedPortLine(lines []string) int {
	section := ""
	for i, l := range lines {
		t := strings.TrimSpace(l)
		if strings.HasPrefix(t, "[") {
			section = tableName(t)
			continue
		}
		if section == "server" && t == pickedPortMark && i+1 < len(lines) && isPortLine(lines[i+1]) {
			return i + 1
		}
	}
	return -1
}

// tableName is the name of the table a [header] line opens: "server" for "[server]  # comment".
func tableName(header string) string {
	h, _, _ := strings.Cut(header, "#")
	return strings.TrimSpace(strings.Trim(strings.TrimSpace(h), "[]"))
}

func isPortLine(l string) bool {
	k, _, ok := strings.Cut(l, "=")
	return ok && strings.TrimSpace(k) == "port"
}

// SetPickedPort sets [server] port in config.toml at path to the port meko install picked, under
// pickedPortMark, and keeps the rest of the file as it is: it replaces an earlier pick, adds the
// line at the top of a [server] table, or adds the table. A port the user chose is refused
// (ErrPortChosen). [server] port is a setting every release reads, so a rollback keeps it.
func SetPickedPort(path string, port int) error {
	b, err := os.ReadFile(path) //nolint:gosec // G304: the data folder's config.toml
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	if chosen, err := PortChosen(path, func(string) string { return "" }); err != nil {
		return err
	} else if chosen {
		return ErrPortChosen
	}
	text := string(b)
	lines := strings.Split(text, "\n")
	line := fmt.Sprintf("port = %d", port)
	switch i, header := pickedPortLine(lines), serverHeader(lines); {
	case i >= 0:
		lines[i] = line
		text = strings.Join(lines, "\n")
	case header >= 0:
		lines = append(lines[:header+1], append([]string{pickedPortMark, line}, lines[header+1:]...)...)
		text = strings.Join(lines, "\n")
	default:
		if text != "" && !strings.HasSuffix(text, "\n") {
			text += "\n"
		}
		if text != "" {
			text += "\n"
		}
		text += "[server]\n" + pickedPortMark + "\n" + line + "\n"
	}
	var c Config
	if _, err := toml.Decode(text, &c); err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	return paths.WriteFile(path, []byte(text))
}

// serverHeader is the index of the [server] header line, or -1.
func serverHeader(lines []string) int {
	for i, l := range lines {
		if t := strings.TrimSpace(l); strings.HasPrefix(t, "[") && !strings.HasPrefix(t, "[[") && tableName(t) == "server" {
			return i
		}
	}
	return -1
}

// ErrWebPort refuses the browsers' old port setting ([web] port, MEKO_WEB_PORT): Meko has one
// port now. It's refused, not ignored, so a port someone chose for browsers doesn't quietly change.
var ErrWebPort = errors.New("[web] port is gone: Meko serves agents and browsers on one port, " +
	"[server] port (default 7777). Set it there (or MEKO_PORT), or remove it")

func (c Config) validate() error {
	if c.Server.Port < 1 || c.Server.Port > 65535 {
		return fmt.Errorf("port %d is out of range: use 1 to 65535", c.Server.Port)
	}
	if c.Storage.MaxContentMB < 1 {
		return fmt.Errorf("storage.max_content_mb %d is out of range: use a whole number of MB, 1 or more",
			c.Storage.MaxContentMB)
	}
	// An unknown access is refused, so a typo never opens the port to the network.
	if c.Web.Access != AccessLocal && c.Web.Access != AccessRemote {
		return fmt.Errorf("web.access %q isn't one of %q, %q", c.Web.Access, AccessLocal, AccessRemote)
	}
	if (c.TLS.Cert == "") != (c.TLS.Key == "") {
		return errors.New("tls.cert and tls.key go together: set both, or neither for a self-signed pair")
	}
	// An unknown mode is refused, so a typo never turns auth off.
	if c.Server.Auth != AuthToken && c.Server.Auth != AuthNone {
		return fmt.Errorf("server.auth %q isn't one of %q, %q", c.Server.Auth, AuthToken, AuthNone)
	}
	if s := c.Secrets.Store; s != "" && s != "keychain" && s != "file" {
		return fmt.Errorf("secrets.store %q isn't one of \"keychain\", \"file\"", s)
	}
	if ch := c.Update.Channel; ch != "stable" && ch != "off" {
		return fmt.Errorf("update.channel %q isn't one of \"stable\", \"off\"", ch)
	}
	if h := c.Update.IntervalHours; h < 0.1 || h > 24*30 {
		return fmt.Errorf("update.interval_hours %g is out of range: use 0.1 to 720", h)
	}
	if _, err := c.Log.SlogLevel(); err != nil {
		return err
	}
	if d := c.ConversationSearch.MaxDistance; d < 0 || d > 2 {
		return fmt.Errorf("conversation_search.max_distance %g is out of range: use 0 to 2", d)
	}
	if d := c.KnowledgeBase.MaxDistance; d < 0 || d > 2 {
		return fmt.Errorf("knowledge_base.max_distance %g is out of range: use 0 to 2", d)
	}
	if u := c.CallHome.URL; u != "" {
		if p, err := url.Parse(u); err != nil || (p.Scheme != "https" && p.Scheme != "http") || p.Host == "" {
			return fmt.Errorf("call_home.url %q isn't an http or https URL", u)
		}
	}
	return nil
}

// YugabyteDBCallHomeOff reports whether YB_DISABLE_CALLHOME's value turns call home off: one of
// yugabyted's true words.
func YugabyteDBCallHomeOff(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "true", "t", "yes", "y", "1":
		return true
	}
	return false
}

// SlogLevel turns the level name into a slog level.
func (l Log) SlogLevel() (slog.Level, error) {
	switch strings.ToLower(l.Level) {
	case "debug":
		return slog.LevelDebug, nil
	case "info":
		return slog.LevelInfo, nil
	case "warn":
		return slog.LevelWarn, nil
	case "error":
		return slog.LevelError, nil
	}
	return 0, fmt.Errorf("log level %q isn't one of debug, info, warn, error", l.Level)
}

// WebFile is web.toml beside config.toml at path: the [web] (and [tls]) settings Meko chose
// itself, from meko install's access, the handoff from AgentK (AddWeb) and meko ui start and stop
// (SetWebEnabled). It's a file of its own, not part of config.toml, because a release from before
// the web UI refuses config.toml's unknown keys: a rollback after an update that failed its
// probation, or a downgrade, would then leave a Meko that can't start. Older releases don't read
// web.toml at all, and ones that do ignore a key they don't know there (enabled).
func WebFile(configPath string) string {
	return filepath.Join(filepath.Dir(configPath), "web.toml")
}

// readWebFile reads web.toml; a missing one is empty.
func readWebFile(path string) (webFile, error) {
	var f webFile
	if _, err := toml.DecodeFile(path, &f); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return webFile{}, fmt.Errorf("%s: %w", path, err)
	}
	return f, nil
}

func mergeWeb(base Web, over webTable) Web {
	if over.Access != "" {
		base.Access = over.Access
	}
	if over.Name != "" {
		base.Name = over.Name
	}
	if over.Enabled != nil {
		base.Enabled = *over.Enabled
	}
	return base
}

// AddWeb writes web, and tls when it names files, to web.toml (WebFile), unless config.toml at path
// or web.toml already sets [web]'s access or name: config.toml's are the user's, and web.toml's an
// earlier choice. web.toml's enabled is kept. It reports whether it wrote. Meko writes these
// settings only this way: meko install, with the access it picked, and the handoff from AgentK,
// with the access AgentK had (never its port: Meko's is [server] port).
func AddWeb(path string, web Web, tls TLS) (bool, error) {
	webPath := WebFile(path)
	for _, p := range []string{path, webPath} {
		var c Config
		md, err := toml.DecodeFile(p, &c)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return false, fmt.Errorf("%s: %w", p, err)
		}
		if md.IsDefined("web", "access") || md.IsDefined("web", "name") {
			return false, nil
		}
	}
	f, err := readWebFile(webPath)
	if err != nil {
		return false, err
	}
	t := webTable{Access: web.Access, Name: web.Name}
	if f.Web != nil {
		t.Enabled = f.Web.Enabled
	}
	f.Web = &t
	if tls.Cert != "" && tls.Key != "" {
		f.TLS = &tls
	}
	return true, writeWebFile(webPath, f)
}

// ErrWebEnabledSet is SetWebEnabled's refusal when config.toml sets [web] enabled itself.
var ErrWebEnabledSet = errors.New("config.toml sets [web] enabled")

// SetWebEnabled turns the web UI on or off in web.toml (WebFile), keeping its other
// settings, unless config.toml at path sets [web] enabled, which wins over it (ErrWebEnabledSet).
func SetWebEnabled(path string, on bool) error {
	var c Config
	md, err := toml.DecodeFile(path, &c)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("%s: %w", path, err)
	}
	if err == nil && md.IsDefined("web", "enabled") {
		return ErrWebEnabledSet
	}
	webPath := WebFile(path)
	f, err := readWebFile(webPath)
	if err != nil {
		return err
	}
	if f.Web == nil {
		f.Web = &webTable{}
	}
	f.Web.Enabled = &on
	return writeWebFile(webPath, f)
}

// writeWebFile writes web.toml with what f sets.
func writeWebFile(path string, f webFile) error {
	var b strings.Builder
	b.WriteString("# How browsers reach Meko, as Meko chose it (meko install, the handoff from AgentK, meko ui\n" +
		"# start and stop). A [web] or [tls] key in config.toml overrides the one here. access = \"local\"\n" +
		"# takes requests from this computer only; \"remote\" listens on every address, browsers over HTTPS.\n[web]\n")
	if w := f.Web; w != nil {
		if w.Access != "" {
			fmt.Fprintf(&b, "access = %q\n", w.Access)
		}
		if w.Name != "" {
			fmt.Fprintf(&b, "name = %q\n", w.Name)
		}
		if w.Enabled != nil {
			fmt.Fprintf(&b, "enabled = %t\n", *w.Enabled)
		}
	}
	if t := f.TLS; t != nil && t.Cert != "" && t.Key != "" {
		fmt.Fprintf(&b, "\n[tls]\ncert = %q\nkey = %q\n", t.Cert, t.Key)
	}
	return paths.WriteFile(path, []byte(b.String()))
}
