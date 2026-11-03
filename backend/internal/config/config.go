// Package config loads Meko's settings. Precedence: flags, then MEKO_* environment
// variables, then config.toml in the data folder, then defaults.
package config

import (
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/BurntSushi/toml"

	"github.com/yugabyte/meko/backend/internal/paths"
)

// DefaultPort is the API listener's: next to the web listener's DefaultWebPort.
const DefaultPort = 7778

// DefaultWebPort is the web listener's, the port AgentK listened on before Meko served it.
const DefaultWebPort = 7777

// Config is every setting.
type Config struct {
	Server             Server             `toml:"server"`
	Log                Log                `toml:"log"`
	MemoryEngine       MemoryEngine       `toml:"memory_engine"`
	ConversationSearch ConversationSearch `toml:"conversation_search"`
	Secrets            Secrets            `toml:"secrets"`
	Update             Update             `toml:"update"`
	KnowledgeBase      KnowledgeBase      `toml:"knowledge_base"`
	WebUI              WebUI              `toml:"web_ui"`
	Web                Web                `toml:"web"`
	TLS                TLS                `toml:"tls"`
	CallHome           CallHome           `toml:"call_home"`
}

// Web is the [web] table: the web listener browsers use, for the web UI and the extensions' at
// /ext/<name>/ (the API listener, [server], stays on 127.0.0.1 for agents and the CLI).
type Web struct {
	// Port is the web listener's (default 7777). Also MEKO_WEB_PORT.
	Port int `toml:"port"`
	// Access is "local" (the default: 127.0.0.1 over HTTP, for requests from this computer) or
	// "remote" (every address over HTTPS). Neither has a login. Also MEKO_WEB_ACCESS.
	Access string `toml:"access"`
	// Name is the host name links to the web listener use; empty is this host's FQDN (remote) or
	// 127.0.0.1 (local).
	Name string `toml:"name"`
	// Enabled is whether serve runs the web listener at all (true by default): meko ui stop and
	// meko ui start set it in web.toml (SetWebEnabled), and meko install --no-ui sets it off.
	Enabled bool `toml:"enabled"`
}

// webTable is web.toml's [web]: Web, with Enabled unset when the file doesn't name it.
type webTable struct {
	Port    int    `toml:"port"`
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

// TLS is the [tls] table: the web listener's certificate with remote access. Empty uses a
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

// MemoryEngine is the [memory_engine] table: where the memory engine and its Node are. Empty means the usual
// places (internal/memoryengine).
type MemoryEngine struct {
	Node string `toml:"node"` // also MEKO_NODE
	Dir  string `toml:"dir"`  // the built memory engine; also MEKO_MEMORY_ENGINE_DIR
}

// Log is the [log] table.
type Log struct {
	Level string `toml:"level"`
}

// Default is the configuration with nothing set.
func Default() Config {
	return Config{Server: Server{Port: DefaultPort, Auth: AuthToken}, Log: Log{Level: "info"},
		Web:      Web{Port: DefaultWebPort, Access: AccessLocal, Enabled: true},
		Update:   Update{Channel: "stable", Auto: true, IntervalHours: 6, Repo: "yugabyte/meko"},
		CallHome: CallHome{Enabled: true}}
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
	if v := env("MEKO_WEB_PORT"); v != "" {
		p, err := strconv.Atoi(v)
		if err != nil {
			return Config{}, fmt.Errorf("MEKO_WEB_PORT=%q isn't a port number", v)
		}
		c.Web.Port = p
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
		c.MemoryEngine.Node = v
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
	if v := env("MEKO_SECRETS"); v != "" {
		c.Secrets.Store = v
	}
	if v := env("MEKO_MEMORY_ENGINE_DIR"); v != "" {
		c.MemoryEngine.Dir = v
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

func (c Config) validate() error {
	if c.Server.Port < 1 || c.Server.Port > 65535 {
		return fmt.Errorf("port %d is out of range: use 1 to 65535", c.Server.Port)
	}
	if c.Web.Port < 1 || c.Web.Port > 65535 {
		return fmt.Errorf("web.port %d is out of range: use 1 to 65535", c.Web.Port)
	}
	if c.Web.Port == c.Server.Port {
		return fmt.Errorf("web.port and server.port are both %d: give the web listener a port of its own", c.Web.Port)
	}
	// An unknown access is refused, so a typo never opens the web listener to the network.
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
// the web listener refuses config.toml's unknown keys: a rollback after an update that failed its
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
	if over.Port != 0 {
		base.Port = over.Port
	}
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
// or web.toml already sets [web]'s port, access or name: config.toml's are the user's, and
// web.toml's an earlier choice. web.toml's enabled is kept. It reports whether it wrote. Meko
// writes these settings only this way: meko install, with the access it picked, and the handoff
// from AgentK, with the access and port AgentK had.
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
		if md.IsDefined("web", "port") || md.IsDefined("web", "access") || md.IsDefined("web", "name") {
			return false, nil
		}
	}
	f, err := readWebFile(webPath)
	if err != nil {
		return false, err
	}
	t := webTable{Port: web.Port, Access: web.Access, Name: web.Name}
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

// SetWebEnabled turns the web listener on or off in web.toml (WebFile), keeping its other
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
	b.WriteString("# The web listener's settings Meko chose (meko install, the handoff from AgentK, meko ui\n" +
		"# start and stop). A [web] or [tls] key in config.toml overrides the one here. access = \"local\"\n" +
		"# takes requests from this computer only; \"remote\" listens on every address, over HTTPS.\n[web]\n")
	if w := f.Web; w != nil {
		if w.Port != 0 {
			fmt.Fprintf(&b, "port = %d\n", w.Port)
		}
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
