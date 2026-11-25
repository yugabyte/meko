package rest_test

import (
	"path/filepath"
	"testing"

	"github.com/labstack/echo/v4"

	"github.com/yugabyte/meko/src/meko/internal/server"
	"github.com/yugabyte/meko/src/meko/internal/testenv"
)

// Setting up extraction or answers makes the server run a program, so only the owner may: the
// admin token, remote or local access. Only the admin token names the program; local access is
// every user of the computer's, so the others get the claude the server finds, or a 400 without one.
func TestHarnessSetup(t *testing.T) {
	claude := fakeClaude(t)
	as := ""
	e := testenv.Start(t, testenv.Options{ExtractorHarness: &fakeHarness{}, Answerer: &fakeAnswerer{},
		Credential: func(c echo.Context) string {
			if as != "" {
				return as
			}
			return server.Credential(c)
		}})
	elsewhere := fakeClaude(t)
	for _, settings := range []string{"/settings/memory-extractor", "/settings/answers"} {
		t.Setenv("PATH", "/usr/bin:/bin")           // the context engine is running: it needs no Node now
		for _, cred := range []string{"", "none"} { // "" keeps the client token's
			as = cred
			wantErr(t, e, "PUT", settings, map[string]any{"harness": "claude"}, 403, "FORBIDDEN")
		}
		for _, cred := range []string{"remote", "local"} {
			as = cred
			wantErr(t, e, "PUT", settings, map[string]any{"harness": "claude", "command": elsewhere}, 403,
				"FORBIDDEN")
			wantErr(t, e, "PUT", settings, map[string]any{"harness": "claude"}, 400, "VALIDATION_ERROR")
		}
		t.Setenv("PATH", filepath.Dir(claude)+":/usr/bin:/bin")
		for _, cred := range []string{"remote", "local"} {
			as = cred
			if st := obj(okData(t, e, "PUT", settings, map[string]any{"harness": "claude"}, 200)); st["command"] != claude {
				t.Errorf("%s %s: %v", cred, settings, st)
			}
		}
		as = "admin"
		if st := obj(okData(t, e, "PUT", settings, map[string]any{"harness": "claude", "command": elsewhere},
			200)); st["command"] != elsewhere {
			t.Errorf("admin %s: %v", settings, st)
		}
		okData(t, e, "PUT", settings, map[string]any{"harness": ""}, 200)
	}
}
