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
func registerDocs(e *echo.Echo) {
	e.GET(specURL, func(c echo.Context) error {
		return c.Blob(http.StatusOK, "application/yaml; charset=utf-8", specYAML)
	})

	// Swagger UI: the index lives at docsURL; its static assets hang off it.
	ui := echo.WrapHandler(swgui.New("amp controller API", specURL, docsURL+"/"))
	e.GET(docsURL, ui)
	e.GET(docsURL+"/*", ui)
}
