package api

import (
	_ "embed"
	"net/http"

	"github.com/labstack/echo/v4"
)

// uiURL is where the embedded admin console is served (the server root).
const uiURL = "/"

// uiIndexHTML is the single-page admin console: a static HTML+CSS+vanilla-JS
// page that drives the REST API (under /api/v1) from the browser to list and
// create users, storage pools, and timelines, and to view their endpoints.
// Like the spec and the explorer, it ships inside the binary, so it works with
// no network access and no build step.
//
//go:embed ui/index.html
var uiIndexHTML []byte

// registerUI serves the embedded admin console at the server root. It is a
// pure client of the documented API: the page talks to /api/v1, so it stays in
// sync with the spec automatically and needs no server-side rendering.
func registerUI(e *echo.Echo) {
	e.GET(uiURL, func(c echo.Context) error {
		return c.Blob(http.StatusOK, "text/html; charset=utf-8", uiIndexHTML)
	})
}
