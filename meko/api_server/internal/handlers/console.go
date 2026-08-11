package handlers

import (
	"embed"
	"encoding/json"
	"net/http"
	"strings"

	"github.com/labstack/echo/v4"
)

// The fleet console, compiled in so control-plane mode has no static-asset
// dependency at runtime. It started as a copy of ycp/web/console.html, which
// could not be shared because api_server and ycp are separate Go modules that
// embedding cannot reach across; that copy is gone with the reference control
// plane, so this is now the only one.
//
// It registers environments and observes them. It does not edit desired state —
// see console_test.go for why that surface was removed rather than guarded.
//
//go:embed web/console.html
var consoleFS embed.FS

// adminSecretPlaceholder is substituted with the embedded admin secret, or with an
// empty string when none is embedded.
const adminSecretPlaceholder = "__EMBEDDED_ADMIN_SECRET__"

// ConsoleHandler serves the fleet console at /. Registered only in
// control-plane mode, so a data-plane api-server still 404s at the root.
//
// embedSecret, when non-empty, is written into the page so the console can call the
// fleet API without asking anyone for the admin secret. That is a deliberate trade and
// it is off by default (--console-embed-admin-secret):
//
//   - It hands the fleet-admin credential to anyone who can load this page. The console
//     could already *do* everything that credential allows, so it grants no new
//     capability against this control plane — but it does turn "can reach the console"
//     into "has the secret", which is reusable anywhere else the header is accepted.
//   - So it belongs on a control plane whose console is already the trust boundary — a
//     dev one behind a VPC-internal LoadBalancer or a tailnet — and not on one fronted
//     at a shared public domain, where any browser that wanders in would be handed it.
//
// Unset, the console asks for the secret in-page and keeps it in sessionStorage, which
// is the behaviour every deployment had before this.
func ConsoleHandler(embedSecret string) echo.HandlerFunc {
	return func(c echo.Context) error {
		b, err := consoleFS.ReadFile("web/console.html")
		if err != nil {
			return apiError(c, http.StatusInternalServerError, "INTERNAL_ERROR", "console asset missing")
		}
		// JSON-encoded rather than pasted in raw: the secret lands inside a JS string
		// literal, and a value containing a quote or a backslash would otherwise break
		// the script — silently, since the page would still serve.
		quoted, err := json.Marshal(embedSecret)
		if err != nil {
			return apiError(c, http.StatusInternalServerError, "INTERNAL_ERROR", "console secret encode failed")
		}
		page := strings.Replace(string(b), `"`+adminSecretPlaceholder+`"`, string(quoted), 1)
		// The page is generated per request (it embeds the admin secret) and ships
		// inside the api-server binary, so a new image is the only way it changes —
		// and a browser must never keep serving the previous build's copy. Without
		// this a rolled-forward console (e.g. amp support added to the register form)
		// silently shows the old UI until a hard refresh.
		c.Response().Header().Set("Cache-Control", "no-store")
		return c.HTMLBlob(http.StatusOK, []byte(page))
	}
}
