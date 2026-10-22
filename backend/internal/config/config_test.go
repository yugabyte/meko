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
