package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
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
		{name: "defaults", wantPort: 7778, wantLevel: "info"},
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
		{name: "web port out of range", env: map[string]string{"MEKO_WEB_PORT": "0"}, want: []string{"web.port 0"}},
		{name: "both listeners on one port", file: "[web]\nport = 7778\n", want: []string{"both 7778"}},
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

func TestMemoryEngineSettings(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	toml := "[memory_engine]\nnode = \"/file/node\"\ndir = \"/file/memory_engine\"\n"
	if err := os.WriteFile(path, []byte(toml), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := Load(path, func(string) string { return "" }, Overrides{})
	if err != nil || c.MemoryEngine.Node != "/file/node" || c.MemoryEngine.Dir != "/file/memory_engine" {
		t.Fatalf("from the file: %+v %v", c.MemoryEngine, err)
	}
	env := map[string]string{"MEKO_NODE": "/env/node", "MEKO_MEMORY_ENGINE_DIR": "/env/memory_engine"}
	c, err = Load(path, func(k string) string { return env[k] }, Overrides{})
	if err != nil || c.MemoryEngine.Node != "/env/node" || c.MemoryEngine.Dir != "/env/memory_engine" {
		t.Fatalf("env over the file: %+v %v", c.MemoryEngine, err)
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
		c.Update.Repo != "yugabyte/agent-k-local" || c.Secrets.Store != "" {
		t.Fatalf("defaults %+v %+v, %v", c.Update, c.Secrets, err)
	}
	toml := "[update]\nauto = false\ninterval_hours = 1\n[secrets]\nstore = \"file\"\n"
	if err = os.WriteFile(path, []byte(toml), 0o600); err != nil {
		t.Fatal(err)
	}
	env := map[string]string{"MEKO_UPDATE_CHANNEL": "off", "MEKO_UPDATE_REPO": "me/fork"}
	c, err = Load(path, func(k string) string { return env[k] }, Overrides{})
	if err != nil || c.Update.Auto || c.Update.IntervalHours != 1 || c.Update.Channel != "off" ||
		c.Update.Repo != "me/fork" || c.Secrets.Store != "file" {
		t.Fatalf("got %+v %+v, %v", c.Update, c.Secrets, err)
	}
	for _, bad := range []map[string]string{
		{"MEKO_UPDATE_CHANNEL": "beta"}, {"MEKO_UPDATE_AUTO": "maybe"}, {"MEKO_SECRETS": "vault"},
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
	if err != nil || c.Web != (Web{Port: 7777, Access: AccessLocal}) {
		t.Fatalf("defaults: %+v %v", c.Web, err)
	}
	file := "[web]\nport = 8443\naccess = \"remote\"\nname = \"dev.example.com\"\n"
	if err = os.WriteFile(path, []byte(file), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err = Load(path, func(string) string { return "" }, Overrides{})
	if err != nil || c.Web != (Web{Port: 8443, Access: AccessRemote, Name: "dev.example.com"}) {
		t.Fatalf("from the file: %+v %v", c.Web, err)
	}
	env := map[string]string{"MEKO_WEB_PORT": "9443", "MEKO_WEB_ACCESS": "local"}
	c, err = Load(path, func(k string) string { return env[k] }, Overrides{})
	if err != nil || c.Web.Port != 9443 || c.Web.Access != AccessLocal {
		t.Fatalf("env over the file: %+v %v", c.Web, err)
	}
}

func TestAddWeb(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte("# mine\n[log]\nlevel = \"debug\""), 0o600); err != nil {
		t.Fatal(err)
	}
	wrote, err := AddWeb(path, Web{Port: 7777, Access: AccessRemote}, TLS{Cert: "/a/c.pem", Key: "/a/k.pem"})
	if err != nil || !wrote {
		t.Fatalf("first add: %v %v", wrote, err)
	}
	c, err := Load(path, func(string) string { return "" }, Overrides{})
	if err != nil || c.Web.Access != AccessRemote || c.TLS.Cert != "/a/c.pem" || c.Log.Level != "debug" {
		t.Fatalf("after the add: %+v %+v %v", c.Web, c.TLS, err)
	}
	b, _ := os.ReadFile(path)
	if !strings.HasPrefix(string(b), "# mine\n") {
		t.Errorf("the user's lines changed: %q", b)
	}
	// A [web] table that's there is the user's: nothing is added again.
	if wrote, err = AddWeb(path, Web{Access: AccessLocal}, TLS{}); err != nil || wrote {
		t.Fatalf("second add: %v %v", wrote, err)
	}
	if fi, serr := os.Stat(path); serr != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("mode: %v %v", fi.Mode(), serr)
	}
	// A missing file is made.
	fresh := filepath.Join(t.TempDir(), "config.toml")
	if wrote, err = AddWeb(fresh, Web{Access: AccessLocal}, TLS{}); err != nil || !wrote {
		t.Fatalf("fresh: %v %v", wrote, err)
	}
	if c, err = Load(fresh, func(string) string { return "" }, Overrides{}); err != nil || c.Web.Access != AccessLocal {
		t.Fatalf("fresh load: %+v %v", c.Web, err)
	}
}
