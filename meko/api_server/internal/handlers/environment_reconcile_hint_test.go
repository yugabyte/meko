package handlers

import (
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/labstack/echo/v4"

	"meko/api_server/internal/models"
)

// hintForDesired renders a meko environment's hint against a given desired state.
func hintForDesired(t *testing.T, desired []models.DesiredResource) string {
	t.Helper()
	req := httptest.NewRequest("POST", "/api/v1/environments", nil)
	req.Host = "ycp.example.com"
	c := echo.New().NewContext(req, httptest.NewRecorder())
	return installHint(c, &models.Environment{ID: "acme-prod", Kind: "meko"}, desired)
}

// Which agent an environment runs follows from what it declares. A stack in
// desired state is the meko-agent's to reconcile; nothing there means the chart
// installed it and the reconcile-agent converges datapacks over the api-server.
func TestInstallHint_MekoModelFollowsDesiredState(t *testing.T) {
	stack := hintForDesired(t, []models.DesiredResource{{Kind: "meko-stack", Name: "meko"}})
	if !strings.Contains(stack, "deploy/meko-agent") {
		t.Errorf("an environment declaring a stack must get the meko-agent hint:\n%s", stack)
	}

	chart := hintForDesired(t, []models.DesiredResource{})
	if !strings.Contains(chart, "deploy/meko-reconcile-agent") {
		t.Errorf("an environment declaring no stack must get the reconcile-agent hint:\n%s", chart)
	}
	if strings.Contains(chart, "values-agent.yaml") {
		t.Errorf("the reconcile-agent hint must not install the agent-only preset:\n%s", chart)
	}
}

// A desired-state read that failed passes nil, which must not flip an existing
// environment onto the other install model — every environment registered before
// the reconcile-agent existed runs the meko-agent.
func TestInstallHint_UnreadableDesiredFallsBackToTheAgent(t *testing.T) {
	hint := hintForDesired(t, nil)
	if !strings.Contains(hint, "deploy/meko-agent") {
		t.Errorf("a nil desired state must render the meko-agent hint:\n%s", hint)
	}
}

// The individual stack kinds count too: an operator who declared a database and an
// mcp-server rather than one meko-stack still has an environment the meko-agent
// serves.
func TestInstallHint_IndividualStackKindsCount(t *testing.T) {
	for _, kind := range []string{"meko-stack", "database", "mcp-server", "rag-worker"} {
		hint := hintForDesired(t, []models.DesiredResource{{Kind: kind, Name: "x"}})
		if !strings.Contains(hint, "deploy/meko-agent") {
			t.Errorf("kind %q must render the meko-agent hint", kind)
		}
	}
	// A datapack is the reconcile-agent's, so it must not pull in the other hint.
	hint := hintForDesired(t, []models.DesiredResource{{Kind: "datapack", Name: "team-a"}})
	if !strings.Contains(hint, "deploy/meko-reconcile-agent") {
		t.Errorf("a datapack-only environment must render the reconcile-agent hint:\n%s", hint)
	}
}

// The reconcile-agent hint has to be runnable as printed: the values the operator
// must supply belong in the header, and the only placeholder left inside the
// commands is the API key the control plane never sees.
func TestInstallHint_ReconcileIsRunnableAsPrinted(t *testing.T) {
	hint := hintForDesired(t, []models.DesiredResource{})

	for _, want := range []string{
		// The chart path as it exists in a meko checkout, and the preset that makes
		// the install one self-contained namespace.
		"meko/deploy/helm/meko",
		"values-byoc.yaml",
		// The one credential the install needs, in a Secret rather than on a
		// command line.
		"create secret generic meko-agent-api-key",
		// The chart wiring that makes the agent render at all.
		"--set reconcileAgent.enabled=true",
		"--set reconcileAgent.ycpUrl=http://ycp.example.com/api/v1",
		"--set reconcileAgent.envName=acme-prod",
		// What keeps the reconciler off a person's datapacks, and the footnote for
		// the configuration that does need a token.
		"created_by_agent_id",
		"Cognito ON",
	} {
		if !strings.Contains(hint, want) {
			t.Errorf("hint is missing %q:\n%s", want, hint)
		}
	}

	// Placeholders belong in the header block, where the operator edits them once,
	// not mid-command where the paste silently produces something broken. The
	// exceptions are the two credentials, which are Secret values on their own line.
	for _, line := range strings.Split(hint, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "#") || !strings.Contains(line, "<") {
			continue
		}
		if strings.HasPrefix(line, "TAG=") || strings.HasPrefix(line, "AGENT_TAG=") ||
			strings.Contains(line, "--from-literal=") {
			continue
		}
		t.Errorf("placeholder outside the header and the Secret literals: %q", line)
	}
}
