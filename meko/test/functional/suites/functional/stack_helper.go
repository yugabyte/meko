// Package functional holds the tests the portal's Functional card runs (tag
// regression). They share the run's stack, and each creates and deletes only its
// own resources, named with t.ResourceSuffix(). The universe tests build their own
// universes on the run's db.provider and skip with the reason when it cannot do
// what they need. Tests that use nothing of the run's stack declare ClusterOnly,
// so a stack that never comes up does not block them.
package functional

import (
	"fmt"
	"net/http"

	"github.com/yugabyte/meko/meko/test/functional/framework/apps/mekoapi"
	"github.com/yugabyte/meko/meko/test/functional/suites/internal/stacktest"
)

var (
	stackEnv           = stacktest.Env
	clusterEnv         = stacktest.ClusterEnv
	cleanupCtx         = stacktest.CleanupCtx
	apiFor             = stacktest.API
	mcpFor             = stacktest.MCP
	hasLangfuseProject = stacktest.HasLangfuseProject
	accountsFor        = stacktest.Accounts
	anonymousAPI       = stacktest.Anonymous
	controlFor         = stacktest.Control
)

func tags(extra ...string) []string { return append([]string{"regression", "nightly"}, extra...) }

func wantStatus(what string, got mekoapi.Response, status int, code string) error {
	if got.Status != status || (code != "" && got.Code != code) {
		return fmt.Errorf("%s: HTTP %d %q, want %d %q: %s", what, got.Status, got.Code, status, code, truncateBody(got.Body))
	}
	return nil
}

func truncateBody(b []byte) string {
	if len(b) > 300 {
		return string(b[:300]) + "..."
	}
	return string(b)
}

var (
	statusOK        = http.StatusOK
	statusNotFound  = http.StatusNotFound
	statusForbidden = http.StatusForbidden
	statusUnauth    = http.StatusUnauthorized
)
