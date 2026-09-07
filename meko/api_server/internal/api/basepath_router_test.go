package api_test

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"meko/api_server/docs"
	"meko/api_server/internal/api"
)

// registeredPaths collapses the router's route table into a set, which is what
// the base-path assertions below are really about: where routes are mounted,
// not what the handlers do. Needs no database — NewRouter only constructs
// repositories, it does not query through them.
func registeredPaths(t *testing.T, cfg api.ServerConfig) map[string]bool {
	t.Helper()
	router, _, err := api.NewRouter(cfg)
	if err != nil {
		t.Fatalf("NewRouter: %v", err)
	}
	paths := map[string]bool{}
	for _, r := range router.Routes() {
		paths[r.Path] = true
	}
	return paths
}

// TestRouter_BasePath_MountsWholeSurface covers the YCP-at-/ycp deployment:
// with --base-path set, every route moves under the prefix and nothing but the
// probe alias answers at the domain root.
func TestRouter_BasePath_MountsWholeSurface(t *testing.T) {
	paths := registeredPaths(t, api.ServerConfig{ControlPlaneMode: true, BasePath: "/ycp"})

	for _, want := range []string{
		"/ycp/api/v1/health",
		"/ycp/api/v1/environments",
		"/ycp/api/v1/agent/enroll",
		"/ycp/api/v1/agent/desired-state",
		// The operator's half of the same state: the agent GET resolves by a key id
		// bound to the environment, which nobody at a console holds.
		"/ycp/api/v1/environments/:id/desired",
		// The task history rides the same fleet group, so it inherits the
		// admin-secret + account middleware rather than being wired its own.
		"/ycp/api/v1/environments/:id/tasks",
		"/ycp/api/v1/environments/:id/tasks/:task_id",
		"/ycp/api/v1/admin/metrics",
		"/ycp/success",
		"/ycp/swagger/*",
		"/ycp/", // the console
		"/ycp",  // …and the same page without the trailing slash
		// Probes hit the pod directly, bypassing the proxy that supplies the
		// prefix, so health stays reachable unprefixed as well.
		"/api/v1/health",
	} {
		if !paths[want] {
			t.Errorf("route %q not registered under base path", want)
		}
	}

	// Everything else must be gone from the root: a request the proxy did not
	// prefix should 404 rather than reach the fleet API.
	for _, unwanted := range []string{
		"/",
		"/success",
		"/api/v1/environments",
		"/api/v1/agent/enroll",
	} {
		if paths[unwanted] {
			t.Errorf("route %q is still registered at the domain root", unwanted)
		}
	}
}

// TestRouter_NoBasePath_UnchangedMount is the regression guard for every
// deployment that does not set the flag: the mount table must look exactly as
// it did before base paths existed, including no duplicate health route.
func TestRouter_NoBasePath_UnchangedMount(t *testing.T) {
	paths := registeredPaths(t, api.ServerConfig{ControlPlaneMode: true})

	for _, want := range []string{
		"/",
		"/success",
		"/api/v1/health",
		"/api/v1/environments",
		"/swagger/*",
	} {
		if !paths[want] {
			t.Errorf("route %q missing with no base path configured", want)
		}
	}
	for _, unwanted := range []string{"/ycp", "/ycp/", "/ycp/api/v1/health"} {
		if paths[unwanted] {
			t.Errorf("unexpected prefixed route %q with no base path configured", unwanted)
		}
	}
}

// TestRouter_BasePath_RejectsNonPath is the important half of the validation:
// NewRouter must refuse a base path that is not a path prefix instead of
// mounting the surface somewhere unreachable and letting the health probe
// report the pod healthy.
func TestRouter_BasePath_RejectsNonPath(t *testing.T) {
	for _, bad := range []string{
		"https://devcloud.yugabyte.com/ycp",
		"/ycp/*",
		"/../ycp",
	} {
		router, _, err := api.NewRouter(api.ServerConfig{ControlPlaneMode: true, BasePath: bad})
		if err == nil {
			t.Errorf("NewRouter(BasePath=%q) succeeded, want an error", bad)
			continue
		}
		if router != nil {
			t.Errorf("NewRouter(BasePath=%q) returned a router alongside its error", bad)
		}
	}
}

// TestRouter_BasePath_SwaggerSpecFollowsMount guards the "Try it out" path:
// the generated spec bakes in basePath /api/v1, which is a 404 under a prefix.
func TestRouter_BasePath_SwaggerSpecFollowsMount(t *testing.T) {
	if _, _, err := api.NewRouter(api.ServerConfig{BasePath: "/ycp"}); err != nil {
		t.Fatalf("NewRouter: %v", err)
	}
	if got := docs.SwaggerInfo.BasePath; got != "/ycp/api/v1" {
		t.Errorf("SwaggerInfo.BasePath = %q, want %q", got, "/ycp/api/v1")
	}
	// And back to the plain mount, so the default deployment is unaffected.
	if _, _, err := api.NewRouter(api.ServerConfig{}); err != nil {
		t.Fatalf("NewRouter: %v", err)
	}
	if got := docs.SwaggerInfo.BasePath; got != "/api/v1" {
		t.Errorf("SwaggerInfo.BasePath = %q, want %q", got, "/api/v1")
	}
}

// TestRouter_BasePath_HealthAliasIsRateLimited: the alias hangs off the bare
// Echo instance, outside the /api/v1 group, so it has to be handed the group's
// limiter explicitly. HealthHandler pings both databases per call.
func TestRouter_BasePath_HealthAliasIsRateLimited(t *testing.T) {
	router, _, err := api.NewRouter(api.ServerConfig{ControlPlaneMode: true, BasePath: "/ycp"})
	if err != nil {
		t.Fatalf("NewRouter: %v", err)
	}
	// ManagementRateLimiter allows 10 rps with a burst of 20; well past that
	// from one IP must start returning 429.
	var limited bool
	for i := 0; i < 60; i++ {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/health", nil)
		req.RemoteAddr = "203.0.113.9:1234"
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		if rec.Code == http.StatusTooManyRequests {
			limited = true
			break
		}
	}
	if !limited {
		t.Error("root /api/v1/health alias never rate-limited; it bypasses ManagementRateLimiter")
	}
}

// TestRouter_BasePath_ServesConsoleAndAPI drives the router over HTTP: the
// console must render at the mount root (with and without the trailing slash),
// the fleet API must be reachable under the prefix, and the domain root must
// 404 apart from the health probe.
func TestRouter_BasePath_ServesConsoleAndAPI(t *testing.T) {
	router, _, err := api.NewRouter(api.ServerConfig{ControlPlaneMode: true, BasePath: "ycp"})
	if err != nil {
		t.Fatalf("NewRouter: %v", err)
	}

	for _, path := range []string{"/ycp/", "/ycp"} {
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != http.StatusOK {
			t.Errorf("GET %s = %d, want 200", path, rec.Code)
			continue
		}
		body, readErr := io.ReadAll(rec.Body)
		if readErr != nil {
			t.Errorf("GET %s: reading body: %v", path, readErr)
			continue
		}
		if !strings.Contains(string(body), "Yugabyte Control Plane") {
			t.Errorf("GET %s did not return the console HTML", path)
		}
	}

	// 503 (admin secret unset), not 404: the route exists and the admin gate ran.
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/ycp/api/v1/environments", nil))
	if rec.Code == http.StatusNotFound {
		t.Errorf("GET /ycp/api/v1/environments = 404, want the admin gate to answer")
	}

	// The unprefixed fleet route is gone; the unprefixed probe is not.
	rec = httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/environments", nil))
	if rec.Code != http.StatusNotFound {
		t.Errorf("GET /api/v1/environments = %d, want 404 under a base path", rec.Code)
	}
	rec = httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/health", nil))
	if rec.Code != http.StatusOK {
		t.Errorf("GET /api/v1/health = %d, want 200 (probe alias)", rec.Code)
	}
	rec = httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if rec.Code != http.StatusNotFound {
		t.Errorf("GET / = %d, want 404 under a base path", rec.Code)
	}
}
