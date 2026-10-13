package api

import (
	"net/http"
	"testing"

	"github.com/labstack/echo/v4"
)

// /datapacks/count shares its prefix with /datapacks/:datapack_id. Echo ranks a
// static segment above a parameter, so the count route must win; if it did
// not, "count" would reach Describe as a datapack id.
func TestDatapackCountRoute_IsNotTheDescribeRoute(t *testing.T) {
	e := echo.New()
	g := e.Group("/api/v1")
	registerDatapackRoutes(g, g, &serviceRegistry{}, 0, false, false)

	c := e.NewContext(nil, nil)
	e.Router().Find(http.MethodGet, "/api/v1/datapacks/count", c)
	if got := c.Path(); got != "/api/v1/datapacks/count" {
		t.Fatalf("GET /datapacks/count routed to %q", got)
	}

	c = e.NewContext(nil, nil)
	e.Router().Find(http.MethodGet, "/api/v1/datapacks/3e5b4c20-ed05-4c69-ad5c-9a4da3b5b9eb", c)
	if got := c.Path(); got != "/api/v1/datapacks/:datapack_id" {
		t.Fatalf("GET /datapacks/<id> routed to %q", got)
	}
}
