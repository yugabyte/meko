package config

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/BurntSushi/toml"

	"github.com/yugabyte/meko/src/meko/internal/update"
)

func TestLoadPrecedence(t *testing.T) {
	cases := []struct {
		name      string
		file      string
		env       map[string]string
		o         Overrides
		wantPort  int
		wantLevel string
	}{
		{name: "defaults", wantPort: 7777, wantLevel: "info"},
		{name: "file", file: "[server]\nport = 9001\n[log]\nlevel = \"debug\"\n", wantPort: 9001, wantLevel: "debug"},
		{
			name: "env over file", file: "[server]\nport = 9001\n",
			env:      map[string]string{"MEKO_PORT": "9002", "MEKO_LOG_LEVEL": "warn"},
			wantPort: 9002, wantLevel: "warn",
		},
		{
			name: "flags over env", env: map[string]string{"MEKO_PORT": "9002"},
			o:        Overrides{Port: 9003, LogLevel: "error"},
			wantPort: 9003, wantLevel: "error",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.toml")
			if tc.file != "" {
				if err := os.WriteFile(path, []byte(tc.file), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			c, err := Load(path, func(k string) string { return tc.env[k] }, tc.o)
			if err != nil {
				t.Fatal(err)
			}
			if c.Server.Port != tc.wantPort || c.Log.Level != tc.wantLevel {
				t.Fatalf("got port %d level %q, want %d %q", c.Server.Port, c.Log.Level, tc.wantPort, tc.wantLevel)
			}
		})
	}
}

func TestLoadErrors(t *testing.T) {
	cases := []struct {
		name string
		file string
		env  map[string]string
		want []string // substrings of the error
	}{
		{name: "bad toml names file and line", file: "[server]\nport = \n", want: []string{"config.toml", "line 2"}},
		{name: "unknown key", file: "[server]\nprot = 1\n", want: []string{"unknown setting server.prot"}},
		{
			name: "env port not a number", env: map[string]string{"MEKO_PORT": "abc"},
			want: []string{`MEKO_PORT="abc"`},
		},
		{name: "port out of range", file: "[server]\nport = 70000\n", want: []string{"port 70000 is out of range"}},
		{name: "bad level", file: "[log]\nlevel = \"loud\"\n", want: []string{`"loud"`}},
		{name: "unknown auth mode", file: "[server]\nauth = \"nnone\"\n", want: []string{`server.auth "nnone"`}},
		{name: "unknown auth mode in env", env: map[string]string{"MEKO_AUTH": "off"}, want: []string{`"off"`}},
		{name: "unknown web access", file: "[web]\naccess = \"public\"\n", want: []string{`web.access "public"`}},
		// Meko has one port: the web listener's old setting is refused, saying where it went.
		{name: "web port in the file", file: "[web]\nport = 8443\n",
			want: []string{"config.toml", "[web] port is gone", "[server] port"}},
		{name: "web port in env", env: map[string]string{"MEKO_WEB_PORT": "9443"},
			want: []string{"MEKO_WEB_PORT", "[server] port"}},
		{name: "a cert without its key", file: "[tls]\ncert = \"/c.pem\"\n", want: []string{"tls.cert and tls.key"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.toml")
			if tc.file != "" {
				if err := os.WriteFile(path, []byte(tc.file), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			_, err := Load(path, func(k string) string { return tc.env[k] }, Overrides{})
			if err == nil {
				t.Fatal("no error")
			}
			for _, w := range tc.want {
				if !strings.Contains(err.Error(), w) {
					t.Errorf("error %q doesn't mention %q", err, w)
				}
			}
		})
	}
}

func TestContextEngineSettings(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	toml := "[context_engine]\nnode = \"/file/node\"\ndir = \"/file/context_engine\"\n"
	if err := os.WriteFile(path, []byte(toml), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := Load(path, func(string) string { return "" }, Overrides{})
	if err != nil || c.ContextEngine.Node != "/file/node" || c.ContextEngine.Dir != "/file/context_engine" {
		t.Fatalf("from the file: %+v %v", c.ContextEngine, err)
	}
	env := map[string]string{"MEKO_NODE": "/env/node", "MEKO_CONTEXT_ENGINE_DIR": "/env/context_engine"}
	c, err = Load(path, func(k string) string { return env[k] }, Overrides{})
	if err != nil || c.ContextEngine.Node != "/env/node" || c.ContextEngine.Dir != "/env/context_engine" {
		t.Fatalf("env over the file: %+v %v", c.ContextEngine, err)
	}
	// The table's name from before the rename is an unknown setting like any other.
	if err = os.WriteFile(path, []byte("[memory_engine]\nnode = \"/file/node\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err = Load(path, func(string) string { return "" }, Overrides{}); err == nil ||
		!strings.Contains(err.Error(), "unknown setting memory_engine") {
		t.Fatalf("[memory_engine]: %v", err)
	}
}

func TestConversationSearchDistance(t *testing.T) {
	dir := t.TempDir()
	for _, c := range []struct {
		file string
		want float64
		ok   bool
	}{
		{"", 0, true},
		{"[conversation_search]\nmax_distance = 0.4\n", 0.4, true},
		{"[conversation_search]\nmax_distance = 3\n", 0, false},
	} {
		path := filepath.Join(dir, "config.toml")
		if err := os.WriteFile(path, []byte(c.file), 0o600); err != nil {
			t.Fatal(err)
		}
		got, err := Load(path, func(string) string { return "" }, Overrides{})
		if c.ok != (err == nil) || c.ok && got.ConversationSearch.MaxDistance != c.want {
			t.Errorf("%q: %v %v", c.file, got.ConversationSearch, err)
		}
	}
}

func TestStorageLimit(t *testing.T) {
	dir := t.TempDir()
	for _, c := range []struct {
		file    string
		want    int
		wantErr string
	}{
		{file: "", want: DefaultMaxContentMB},
		{file: "[storage]\nmax_content_mb = 50\n", want: 50},
		{file: "[storage]\nmax_content_mb = 1000\n", want: 1000},
		{file: "[storage]\nmax_content_mb = 0\n", wantErr: "storage.max_content_mb 0 is out of range"},
		{file: "[storage]\nmax_content_mb = -5\n", wantErr: "storage.max_content_mb -5 is out of range"},
		{file: "[storage]\nmax_content_mb = \"lots\"\n", wantErr: "config.toml"},
		{file: "[storage]\nmax_content_mb = 1.5\n", wantErr: "config.toml"},
	} {
		path := filepath.Join(dir, "config.toml")
		if err := os.WriteFile(path, []byte(c.file), 0o600); err != nil {
			t.Fatal(err)
		}
		got, err := Load(path, func(string) string { return "" }, Overrides{})
		switch {
		case c.wantErr != "" && (err == nil || !strings.Contains(err.Error(), c.wantErr)):
			t.Errorf("%q: error %v, want one with %q", c.file, err, c.wantErr)
		case c.wantErr == "" && (err != nil || got.Storage.MaxContentMB != c.want):
			t.Errorf("%q: %+v %v, want %d", c.file, got.Storage, err, c.want)
		case c.wantErr == "" && got.Storage.MaxContentBytes() != int64(c.want)<<20:
			t.Errorf("%q: %d bytes", c.file, got.Storage.MaxContentBytes())
		}
	}
}

func TestAuthMode(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	c, err := Load(path, func(string) string { return "" }, Overrides{})
	if err != nil || c.Server.Auth != AuthToken {
		t.Fatalf("default %q, %v", c.Server.Auth, err)
	}
	if err = os.WriteFile(path, []byte("[server]\nauth = \"none\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err = Load(path, func(string) string { return "" }, Overrides{})
	if err != nil || c.Server.Auth != AuthNone {
		t.Fatalf("file %q, %v", c.Server.Auth, err)
	}
	c, err = Load(path, func(k string) string { return map[string]string{"MEKO_AUTH": "token"}[k] }, Overrides{})
	if err != nil || c.Server.Auth != AuthToken {
		t.Fatalf("env over file %q, %v", c.Server.Auth, err)
	}
}

func TestUpdateAndSecretsSettings(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	c, err := Load(path, func(string) string { return "" }, Overrides{})
	if err != nil || c.Update.Channel != "stable" || !c.Update.Auto || c.Update.IntervalHours != 6 ||
		c.Update.Repo != update.DefaultRepo || !c.Update.RequireSignature || c.Secrets.Store != "" {
		t.Fatalf("defaults %+v %+v, %v", c.Update, c.Secrets, err)
	}
	toml := "[update]\nauto = false\ninterval_hours = 1\nrequire_signature = false\n[secrets]\nstore = \"file\"\n"
	if err = os.WriteFile(path, []byte(toml), 0o600); err != nil {
		t.Fatal(err)
	}
	env := map[string]string{"MEKO_UPDATE_CHANNEL": "off", "MEKO_UPDATE_REPO": "me/fork"}
	c, err = Load(path, func(k string) string { return env[k] }, Overrides{})
	if err != nil || c.Update.Auto || c.Update.IntervalHours != 1 || c.Update.Channel != "off" ||
		c.Update.Repo != "me/fork" || c.Update.RequireSignature || c.Secrets.Store != "file" {
		t.Fatalf("got %+v %+v, %v", c.Update, c.Secrets, err)
	}
	env = map[string]string{"MEKO_UPDATE_REQUIRE_SIGNATURE": "true"}
	c, err = Load(path, func(k string) string { return env[k] }, Overrides{})
	if err != nil || !c.Update.RequireSignature {
		t.Fatalf("env over file: %+v, %v", c.Update, err)
	}
	for _, bad := range []map[string]string{
		{"MEKO_UPDATE_CHANNEL": "beta"}, {"MEKO_UPDATE_AUTO": "maybe"}, {"MEKO_UPDATE_REQUIRE_SIGNATURE": "maybe"},
		{"MEKO_SECRETS": "vault"},
	} {
		if _, err := Load(path, func(k string) string { return bad[k] }, Overrides{}); err == nil {
			t.Errorf("%v was taken", bad)
		}
	}
}

func TestWebUISettings(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte("[web_ui]\ndir = \"/file/web_ui\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := Load(path, func(string) string { return "" }, Overrides{})
	if err != nil || c.WebUI.Dir != "/file/web_ui" {
		t.Fatalf("from the file: %+v %v", c.WebUI, err)
	}
	env := map[string]string{"MEKO_WEB_UI_DIR": "/env/web_ui"}
	c, err = Load(path, func(k string) string { return env[k] }, Overrides{})
	if err != nil || c.WebUI.Dir != "/env/web_ui" {
		t.Fatalf("env over the file: %+v %v", c.WebUI, err)
	}
}

func TestCallHomeSettings(t *testing.T) {
	cases := []struct {
		name      string
		file      string
		env       map[string]string
		wantOn    bool
		wantOffBy string
		wantURL   string
	}{
		{name: "on by default", wantOn: true},
		{name: "off in the file", file: "[call_home]\nenabled = false\n", wantOffBy: OffByFile},
		{name: "env off", env: map[string]string{"MEKO_CALL_HOME": "false"}, wantOffBy: OffByEnv},
		{name: "env on over the file", file: "[call_home]\nenabled = false\n", env: map[string]string{"MEKO_CALL_HOME": "1"},
			wantOn: true},
		{name: "yugabyted's switch", env: map[string]string{"MEKO_CALL_HOME": "true", "YB_DISABLE_CALLHOME": "Yes"},
			wantOffBy: OffByYugabyteDB},
		{name: "yugabyted's switch set false", env: map[string]string{"YB_DISABLE_CALLHOME": "false"}, wantOn: true},
		{name: "url from the file", file: "[call_home]\nurl = \"http://127.0.0.1:9/\"\n", wantOn: true,
			wantURL: "http://127.0.0.1:9/"},
		{name: "url from env", file: "[call_home]\nurl = \"http://127.0.0.1:9/\"\n",
			env:    map[string]string{"MEKO_CALL_HOME_URL": "https://collector.test/"},
			wantOn: true, wantURL: "https://collector.test/"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.toml")
			if err := os.WriteFile(path, []byte(tc.file), 0o600); err != nil {
				t.Fatal(err)
			}
			c, err := Load(path, func(k string) string { return tc.env[k] }, Overrides{})
			if err != nil {
				t.Fatal(err)
			}
			if c.CallHome.Enabled != tc.wantOn || c.CallHome.OffBy != tc.wantOffBy || c.CallHome.URL != tc.wantURL {
				t.Fatalf("got %+v, want on %v, off by %q, url %q", c.CallHome, tc.wantOn, tc.wantOffBy, tc.wantURL)
			}
		})
	}
	for _, bad := range []map[string]string{{"MEKO_CALL_HOME": "maybe"}, {"MEKO_CALL_HOME_URL": "ftp://x"},
		{"MEKO_CALL_HOME_URL": "not a url"}} {
		none := filepath.Join(t.TempDir(), "none.toml")
		if _, err := Load(none, func(k string) string { return bad[k] }, Overrides{}); err == nil {
			t.Errorf("%v was taken", bad)
		}
	}
}

func TestWebSettings(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	c, err := Load(path, func(string) string { return "" }, Overrides{})
	if err != nil || c.Web != (Web{Access: AccessLocal, Enabled: true}) || c.Server.Port != 7777 {
		t.Fatalf("defaults: %+v %d %v", c.Web, c.Server.Port, err)
	}
	file := "[web]\naccess = \"remote\"\nname = \"dev.example.com\"\n"
	if err = os.WriteFile(path, []byte(file), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err = Load(path, func(string) string { return "" }, Overrides{})
	if err != nil || c.Web != (Web{Access: AccessRemote, Name: "dev.example.com", Enabled: true}) {
		t.Fatalf("from the file: %+v %v", c.Web, err)
	}
	env := map[string]string{"MEKO_WEB_ACCESS": "local"}
	c, err = Load(path, func(k string) string { return env[k] }, Overrides{})
	if err != nil || c.Web.Access != AccessLocal {
		t.Fatalf("env over the file: %+v %v", c.Web, err)
	}
}

// A port an older Meko wrote to web.toml (from install or the handoff) is ignored, not refused:
// the user never chose it there, and Meko's port is [server] port. The next write drops it.
func TestWebFileOldPort(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(WebFile(path), []byte("[web]\nport = 8443\naccess = \"remote\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := Load(path, func(string) string { return "" }, Overrides{})
	if err != nil || c.Server.Port != 7777 || c.Web.Access != AccessRemote {
		t.Fatalf("with web.toml's old port: %d %+v %v", c.Server.Port, c.Web, err)
	}
	if err = SetWebEnabled(path, false); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(WebFile(path))
	if strings.Contains(string(b), "port") || !strings.Contains(string(b), `access = "remote"`) {
		t.Fatalf("web.toml after a write: %q", b)
	}
}

func TestAddWeb(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	mine := "# mine\n[log]\nlevel = \"debug\"\n"
	if err := os.WriteFile(path, []byte(mine), 0o600); err != nil {
		t.Fatal(err)
	}
	wrote, err := AddWeb(path, Web{Access: AccessRemote}, TLS{Cert: "/a/c.pem", Key: "/a/k.pem"})
	if err != nil || !wrote {
		t.Fatalf("first add: %v %v", wrote, err)
	}
	c, err := Load(path, func(string) string { return "" }, Overrides{})
	if err != nil || c.Web.Access != AccessRemote || c.TLS.Cert != "/a/c.pem" || c.Log.Level != "debug" {
		t.Fatalf("after the add: %+v %+v %v", c.Web, c.TLS, err)
	}
	// config.toml is left as it is, so a release from before the web listener, which refuses keys it
	// doesn't know, still reads it after a rollback; web.toml is owner-only.
	if b, _ := os.ReadFile(path); string(b) != mine {
		t.Errorf("config.toml changed: %q", b)
	}
	if fi, serr := os.Stat(WebFile(path)); serr != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("web.toml: %v %v", fi, serr)
	}
	// Once there's a choice, nothing is added again.
	if wrote, err = AddWeb(path, Web{Access: AccessLocal}, TLS{}); err != nil || wrote {
		t.Fatalf("second add: %v %v", wrote, err)
	}
	// config.toml's own [web] keys win over web.toml's, and stop AddWeb.
	if err = os.WriteFile(path, []byte(mine+"[web]\naccess = \"local\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if c, err = Load(path, func(string) string { return "" }, Overrides{}); err != nil || c.Web.Access != AccessLocal {
		t.Fatalf("config.toml over web.toml: %+v %v", c.Web, err)
	}
	fresh := filepath.Join(t.TempDir(), "config.toml")
	if err = os.WriteFile(fresh, []byte("[web]\nname = \"dev.example.com\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if wrote, err = AddWeb(fresh, Web{Access: AccessRemote}, TLS{}); err != nil || wrote {
		t.Fatalf("add with the user's [web]: %v %v", wrote, err)
	}
}

// meko ui stop and start set [web] enabled in web.toml, keeping what else it holds; config.toml's
// own enabled wins and stops them; and meko install's access still goes in after an earlier stop.
func TestSetWebEnabled(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	load := func() Config {
		t.Helper()
		c, err := Load(path, func(string) string { return "" }, Overrides{})
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	if err := SetWebEnabled(path, false); err != nil {
		t.Fatal(err)
	}
	if c := load(); c.Web.Enabled || c.Web.Access != AccessLocal {
		t.Fatalf("off: %+v", c.Web)
	}
	// An earlier stop isn't a choice of access: install's still goes in, and the switch stays off.
	wrote, err := AddWeb(path, Web{Access: AccessRemote}, TLS{Cert: "/a/c.pem", Key: "/a/k.pem"})
	if err != nil || !wrote {
		t.Fatalf("add after a stop: %v %v", wrote, err)
	}
	if c := load(); c.Web.Enabled || c.Web.Access != AccessRemote || c.TLS.Cert != "/a/c.pem" {
		t.Fatalf("after the add: %+v %+v", c.Web, c.TLS)
	}
	if err := SetWebEnabled(path, true); err != nil {
		t.Fatal(err)
	}
	if c := load(); !c.Web.Enabled || c.Web.Access != AccessRemote || c.TLS.Key != "/a/k.pem" {
		t.Fatalf("on again: %+v %+v", c.Web, c.TLS)
	}
	// A release before the switch reads web.toml's [web] without it, and must still start.
	var old struct {
		Web struct {
			Port   int    `toml:"port"`
			Access string `toml:"access"`
			Name   string `toml:"name"`
		} `toml:"web"`
	}
	if _, err := toml.DecodeFile(WebFile(path), &old); err != nil || old.Web.Access != AccessRemote {
		t.Fatalf("as an older release reads it: %+v %v", old, err)
	}
	if err := os.WriteFile(path, []byte("[web]\nenabled = true\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := SetWebEnabled(path, false); !errors.Is(err, ErrWebEnabledSet) {
		t.Fatalf("with config.toml's own enabled: %v", err)
	}
}
