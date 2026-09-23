package handlers

import (
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/labstack/echo/v4"

	"meko/api_server/internal/models"
)

// mekoHint renders a meko environment's install hint.
func mekoHint(t *testing.T) string {
	t.Helper()
	req := httptest.NewRequest("POST", "/api/v1/environments", nil)
	req.Host = "ycp.example.com"
	c := echo.New().NewContext(req, httptest.NewRecorder())
	return installHint(c, &models.Environment{ID: "acme-prod", Kind: "meko"}, testBYOCMetricsURL)
}

// The meko hint has to be runnable as printed: the values the operator must supply
// belong in the header, and the only placeholder left inside the commands is the
// API key the control plane never sees.
func TestInstallHint_MekoIsRunnableAsPrinted(t *testing.T) {
	hint := mekoHint(t)

	for _, want := range []string{
		// The chart path as it exists in a meko checkout, and the preset that makes
		// the install one self-contained namespace.
		"meko/deploy/helm/meko",
		"values-byoc.yaml",
		// Both credentials, in Secrets rather than on a command line: the agent's
		// API key, and the pull secret every image in the release needs now that
		// they are all on private Quay.
		"create secret generic meko-agent-api-key",
		"create secret docker-registry quay-registry-creds",
		"--docker-server=quay.io",
		// The chart wiring that makes the agent render at all.
		"--set reconcileAgent.enabled=true",
		"--set reconcileAgent.ycpUrl=http://ycp.example.com/api/v1",
		"--set reconcileAgent.envName=acme-prod",
		// values-byoc.yaml enables the inference gateway, so the hint has to carry
		// its tag (else the env preset's ECR tag resolves under quay.io and the pull
		// fails) and its Bedrock credentials (mounted with envFrom and no
		// optional=true, so absent is CreateContainerConfigError, not a missing
		// feature).
		"--set-string inferenceGateway.image.tag=",
		"--set dataPlaneSecrets.awsAccessKeyId=",
		"--set dataPlaneSecrets.awsSecretAccessKey=",
		// The footnote for the configuration that does need a token.
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
			strings.HasPrefix(line, "QUAY_USER=") || strings.HasPrefix(line, "QUAY_TOKEN=") ||
			strings.HasPrefix(line, "BEDROCK_KEY=") || strings.HasPrefix(line, "BEDROCK_SECRET=") ||
			strings.Contains(line, "--from-literal=") {
			continue
		}
		t.Errorf("placeholder outside the header and the Secret literals: %q", line)
	}
}
