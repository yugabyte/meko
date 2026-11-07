package rest

import (
	"net/http/httptest"
	"testing"

	"github.com/labstack/echo/v4"
)

// provision-status gives the client token only for a caller that holds a credential: never for the
// web listener's browsers (local access, which every user of the computer has, or remote, which
// anyone who reaches its port has), nor auth = "none".
func TestKeyFor(t *testing.T) {
	for cred, want := range map[string]string{"admin": "tok", "client": "tok", "remote": "", "local": "",
		"none": "", "": ""} {
		a := &api{Options{ClientToken: "tok", Credential: func(echo.Context) string { return cred }}}
		c := echo.New().NewContext(httptest.NewRequest("GET", "/", nil), httptest.NewRecorder())
		if got := a.keyFor(c); got != want {
			t.Errorf("%q: got %q, want %q", cred, got, want)
		}
	}
}
