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
		// The footnote for the configurations that do need a token — either
		// identity provider, not just Cognito.
		"identity provider ON (Cognito or OIDC)",
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

// values-byoc.yaml leaves the rag workers, langfuse and the provisioner off, and an
// overlay passed AFTER it is the only thing that turns one back on — helm's last-wins
// ordering is the whole mechanism. The hint is where an operator is told that, so the slot has to be in
// the pasted command and not just in the prose.
func TestInstallHint_MekoCanEnableTheDisabledComponents(t *testing.T) {
	hint := mekoHint(t)

	for _, want := range []string{
		// An editable header slot, empty so the base stack pastes unchanged.
		"EXTRA_VALUES=",
		// Named so the operator does not have to derive the overlay's existence.
		"values-byoc-full.yaml",
		// And expanded where it can win: after values-byoc.yaml, not before.
		"-f meko/deploy/helm/meko/values-byoc.yaml ${EXTRA_VALUES}",
		// The key the whole stack degrades silently without.
		"OPENAI_KEY=",
		`--set-string dataPlaneSecrets.openaiApiKey="${OPENAI_KEY}"`,
	} {
		if !strings.Contains(hint, want) {
			t.Errorf("hint is missing %q:\n%s", want, hint)
		}
	}

	// The overlay must not be applied by default: enabling components an operator
	// did not ask for is how a BYOC install grows pods it has no credentials for.
	if strings.Contains(hint, "-f meko/deploy/helm/meko/values-byoc-full.yaml \\") {
		t.Errorf("the full-stack overlay is applied unconditionally:\n%s", hint)
	}
}
