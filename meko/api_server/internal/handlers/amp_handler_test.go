package handlers

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/labstack/echo/v4"

	"meko/api_server/internal/service"
)

// ampError must map the not-registered sentinel to its own status/code —
// MCP tools and the inference gateway branch on AMP_NOT_REGISTERED to tell
// users "an admin must register AMP first" instead of a generic AMP failure.
func TestAMPErrorMapping(t *testing.T) {
	cases := []struct {
		name       string
		err        error
		wantStatus int
		wantCode   string
	}{
		{"not registered", service.ErrAMPNotRegistered,
			http.StatusPreconditionFailed, "AMP_NOT_REGISTERED"},
		{"wrapped not registered", errors.Join(errors.New("ctx"), service.ErrAMPNotRegistered),
			http.StatusPreconditionFailed, "AMP_NOT_REGISTERED"},
		{"controller failure", errors.New("connection refused"),
			http.StatusBadGateway, "AMP_ERROR"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := echo.New()
			rec := httptest.NewRecorder()
			c := e.NewContext(httptest.NewRequest(http.MethodGet, "/", nil), rec)

			if err := ampError(c, tc.err); err != nil {
				t.Fatalf("ampError returned error: %v", err)
			}
			if rec.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d", rec.Code, tc.wantStatus)
			}
			var resp APIResponse
			if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			if resp.Error == nil || resp.Error.Code != tc.wantCode {
				t.Fatalf("error code = %+v, want %s", resp.Error, tc.wantCode)
			}
		})
	}
}
