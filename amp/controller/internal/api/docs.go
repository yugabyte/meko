package api

import (
	"net/http"

	"github.com/labstack/echo/v4"
	swgui "github.com/swaggest/swgui/v5emb"
)

// Paths for the served OpenAPI document and the interactive explorer.
const (
	specURL = apiBaseURL + "/openapi.yaml"
	docsURL = apiBaseURL + "/docs"
)

// registerDocs serves the OpenAPI document (the embedded source of truth) and
// an embedded Swagger UI that renders it. Both ship inside the binary, so the
// explorer works with no network access.
//
// The server root redirects to the explorer, so a browser pointed at a
// port-forwarded controller lands on the API reference. The console is
// amp_ui, not the controller. 302 rather than 301 because browsers cache a 301
// indefinitely.
func registerDocs(e *echo.Echo) {
	e.GET("/", func(c echo.Context) error {
		return c.Redirect(http.StatusFound, docsURL)
	})

	e.GET(specURL, func(c echo.Context) error {
		return c.Blob(http.StatusOK, "application/yaml; charset=utf-8", specYAML)
	})

	// Swagger UI: the index lives at docsURL; its static assets hang off it.
	ui := echo.WrapHandler(swgui.New("amp controller API", specURL, docsURL+"/"))
	e.GET(docsURL, ui)
	e.GET(docsURL+"/*", ui)
}
