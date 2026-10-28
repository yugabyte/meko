package rest

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/labstack/echo/v4"
)

// A tool's error reply maps to a status by its code: a missing datapack is a 404, the tool's own
// failure (a _failed code) a 500 here or a 502 from Meko Cloud, a memory engine that's down a 503, and
// the rest a 400 with the tool's code.
func TestToolReply(t *testing.T) {
	for _, c := range []struct {
		text   string
		cloud  bool
		status int
		code   string
	}{
		{`{"results":[]}`, false, 200, ""},
		{`{"error":"datapack_access_denied","detail":"no datapack x"}`, true, 404, "NOT_FOUND"},
		{`{"error":"datapack_not_found","detail":"x"}`, true, 404, "NOT_FOUND"},
		{`{"error":"search_failed","detail":"Meko couldn't complete the call."}`, false, 500, "INTERNAL_ERROR"},
		{`{"error":"search_failed","detail":"x"}`, true, 502, "CLOUD_CALL_FAILED"},
		{`{"error":"memory_engine_unavailable","detail":"x"}`, false, 503, "memory_engine_unavailable"},
		{`{"error":"invalid_force_source","detail":"x"}`, false, 400, "invalid_force_source"},
	} {
		rec := httptest.NewRecorder()
		ec := echo.New().NewContext(httptest.NewRequest(http.MethodPost, "/", nil), rec)
		if err := toolReply(ec, "context_search", c.text, c.cloud); err != nil {
			t.Fatal(err)
		}
		var env struct {
			Error *struct{ Code string } `json:"error"`
		}
		_ = json.Unmarshal(rec.Body.Bytes(), &env)
		code := ""
		if env.Error != nil {
			code = env.Error.Code
		}
		if rec.Code != c.status || code != c.code {
			t.Errorf("%s (cloud %v): %d %q, want %d %q", c.text, c.cloud, rec.Code, code, c.status, c.code)
		}
	}
}

// A browser that names no location is a web UI tab from before these routes: it gets this Meko's
// datapacks. Tokens get the routes' defaults.
func TestDefaultLocation(t *testing.T) {
	for cred, want := range map[string]string{"session": "local", "local": "local", "admin": "all", "client": "all",
		"none": "all"} {
		a := &api{Options{Credential: func(echo.Context) string { return cred }}}
		ec := echo.New().NewContext(httptest.NewRequest(http.MethodGet, "/", nil), httptest.NewRecorder())
		if got := a.defaultLocation(ec, "all"); got != want {
			t.Errorf("%s: %q, want %q", cred, got, want)
		}
	}
}
