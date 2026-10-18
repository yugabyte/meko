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
			env:      map[string]string{"MEKO_LOCAL_PORT": "9002", "MEKO_LOCAL_LOG_LEVEL": "warn"},
			wantPort: 9002, wantLevel: "warn",
		},
		{
			name: "flags over env", env: map[string]string{"MEKO_LOCAL_PORT": "9002"},
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
			name: "env port not a number", env: map[string]string{"MEKO_LOCAL_PORT": "abc"},
			want: []string{`MEKO_LOCAL_PORT="abc"`},
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
