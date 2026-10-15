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
	return mekoHintFor(t, "", testBYOCMetricsURL)
}

func mekoHintFor(t *testing.T, accountID, byocMetricsURL string) string {
	t.Helper()
	req := httptest.NewRequest("POST", "/api/v1/environments", nil)
	req.Host = "ycp.example.com"
	c := echo.New().NewContext(req, httptest.NewRecorder())
	return installHint(c, &models.Environment{ID: "acme-prod", Kind: "meko", AccountID: accountID}, byocMetricsURL)
}

// The meko hint has to be runnable as printed: the values the operator must supply
// belong in the header — the API key the control plane never sees included.
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
		`--from-literal=api-key="${AGENT_API_KEY}"`,
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
		// The MCP server embeds KB/conversation queries and (under the BYOC preset's
		// EMBEDDER_PROVIDER=aws_bedrock) memories with Bedrock, so the hint must carry
		// its credential too or memory_add/search and those searches cannot embed.
		"--set dataPlaneSecrets.mcpAwsAccessKeyId=",
		"--set dataPlaneSecrets.mcpAwsSecretAccessKey=",
		// One Bedrock region for memory and KB/conversation query embedding;
		// unset, the two fall back to different regions.
		"BEDROCK_REGION=us-east-1",
		`--set-string mekoMcpServer.staticEnv.awsRegion="${BEDROCK_REGION}"`,
		// values-byoc.yaml also turns langfuse on with migrate.enabled, so a
		// fresh install needs its seven dataPlaneSecrets.langfuse* values or it
		// fails at dataplane-secrets.yaml's own fail-fast guard.
		"LANGFUSE_OWNER_EMAIL=",
		`--set-string dataPlaneSecrets.langfuseInitUserEmail="${LANGFUSE_OWNER_EMAIL}"`,
		`--set-string dataPlaneSecrets.langfuseEncryptionKey="$(openssl rand -hex 32)"`,
		// The langfuse S3 event store is now required (MEKO-832): a header var the
		// operator fills, wired into all three bucket overrides so media/export
		// never hit the hosted bucket.
		"LANGFUSE_S3_BUCKET=",
		`--set-string langfuse.config.LANGFUSE_S3_EVENT_UPLOAD_BUCKET="${LANGFUSE_S3_BUCKET}"`,
		`--set-string langfuse.config.LANGFUSE_S3_BATCH_EXPORT_BUCKET="${LANGFUSE_S3_BUCKET}"`,
		`--set-string dataPlaneSecrets.langfuseS3AccessKey="${LANGFUSE_S3_KEY}"`,
		// The footnote for the configurations that do need a token — either
		// identity provider, not just Cognito.
		"identity provider ON (Cognito or OIDC)",
	} {
		if !strings.Contains(hint, want) {
			t.Errorf("hint is missing %q:\n%s", want, hint)
		}
	}

	// Placeholders belong in the header block, where the operator edits them once,
	// not mid-command where the paste silently produces something broken.
	for _, line := range strings.Split(hint, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "#") || !strings.Contains(line, "<") {
			continue
		}
		if strings.HasPrefix(line, "TAG=") || strings.HasPrefix(line, "AGENT_TAG=") ||
			strings.HasPrefix(line, "QUAY_USER=") || strings.HasPrefix(line, "QUAY_TOKEN=") ||
			strings.HasPrefix(line, "BEDROCK_KEY=") || strings.HasPrefix(line, "BEDROCK_SECRET=") ||
			strings.HasPrefix(line, "LANGFUSE_OWNER_EMAIL=") ||
			strings.HasPrefix(line, "LANGFUSE_S3_BUCKET=") || strings.HasPrefix(line, "LANGFUSE_S3_REGION=") ||
			strings.HasPrefix(line, "LANGFUSE_S3_KEY=") || strings.HasPrefix(line, "LANGFUSE_S3_SECRET=") ||
			strings.HasPrefix(line, "BYOC_PASSWORD=") || strings.HasPrefix(line, "DD_API_KEY=") ||
			strings.HasPrefix(line, "AGENT_API_KEY=") {
			continue
		}
		t.Errorf("placeholder outside the header: %q", line)
	}
}

// values-byoc.yaml leaves the rag workers and the provisioner off, and an
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

// The data plane ships telemetry the way an amp one does: metrics to the door this
// control plane issued, logs to Datadog, nowhere else, labeled with the
// environment's identity, and with both credentials in place before the install
// that reads them.
func TestInstallHint_MekoShipsTelemetryOffCluster(t *testing.T) {
	hint := mekoHintFor(t, "3f2b1c00-0000-0000-0000-000000000009", testBYOCMetricsURL)

	for _, want := range []string{
		"opentelemetry-operator",
		"otel/opentelemetry-collector-contrib",
		"create secret generic byoc-metrics-auth",
		"create secret generic datadog-api",
		`--from-literal=api-key="${DD_API_KEY}"`,
		"--set otel.enabled=true",
		"--set otel.byoc.enabled=true",
		"--set-string otel.byoc.metricsEndpoint=" + testBYOCMetricsURL,
		// Empty, not absent: the chart reads "" as no in-cluster store.
		"--set-string otel.remoteWriteEndpoint=",
		"--set-string otel.logsEndpoint=",
		`--set otel.podMetrics="${POD_METRICS}"`,
		"--set otel.environmentId=acme-prod",
		"--set otel.accountId=3f2b1c00-0000-0000-0000-000000000009",
		// Quoted, so a pasted credential is not expanded by the shell.
		"BYOC_PASSWORD='<byoc password>'",
		"DD_API_KEY='<datadog api key>'",
	} {
		if !strings.Contains(hint, want) {
			t.Errorf("hint is missing %q:\n%s", want, hint)
		}
	}
	install := strings.Index(hint, "helm upgrade --install meko")
	for _, secret := range []string{"byoc-metrics-auth", "datadog-api"} {
		if strings.Index(hint, secret) > install {
			t.Errorf("%s is created after the install that reads it:\n%s", secret, hint)
		}
	}
}

// With no door configured the endpoint is left to the chart, and a standalone
// YCP's sentinel account is not stamped on every series.
func TestInstallHint_MekoWithoutADoorOrAnAccount(t *testing.T) {
	hint := mekoHintFor(t, "00000000-0000-0000-0000-000000000000", "")
	for _, unwanted := range []string{"otel.byoc.metricsEndpoint", "otel.accountId"} {
		if strings.Contains(hint, unwanted) {
			t.Errorf("hint carries %q:\n%s", unwanted, hint)
		}
	}
}
