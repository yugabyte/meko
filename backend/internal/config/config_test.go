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

func TestEngineSettings(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte("[engine]\nnode = \"/file/node\"\ndir = \"/file/engine\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := Load(path, func(string) string { return "" }, Overrides{})
	if err != nil || c.Engine.Node != "/file/node" || c.Engine.Dir != "/file/engine" {
		t.Fatalf("from the file: %+v %v", c.Engine, err)
	}
	env := map[string]string{"MEKO_NODE": "/env/node", "MEKO_ENGINE_DIR": "/env/engine"}
	c, err = Load(path, func(k string) string { return env[k] }, Overrides{})
	if err != nil || c.Engine.Node != "/env/node" || c.Engine.Dir != "/env/engine" {
		t.Fatalf("env over the file: %+v %v", c.Engine, err)
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
