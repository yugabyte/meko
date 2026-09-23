package handlers

import (
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/labstack/echo/v4"

	"meko/api_server/internal/models"
)

func hintFor(t *testing.T, kind, path, host, accountID, agentBaseURL string, headers map[string]string) string {
	t.Helper()
	return hintForEnv(t, &models.Environment{
		ID: "acme-prod", Kind: kind, AccountID: accountID, APIKeyID: keyA,
	}, path, host, agentBaseURL, headers)
}

// testBYOCMetricsURL stands in for the control plane's --byoc-metrics-url. The
// hint prints it and the enroll check accepts only it, so the tests below assert
// the printed value against the same constant rather than a literal.
const testBYOCMetricsURL = "https://byoc.example.com/api/v1/otlp/metrics"

func hintForEnv(
	t *testing.T, e *models.Environment, path, host, agentBaseURL string, headers map[string]string,
) string {
	t.Helper()
	req := httptest.NewRequest("POST", path, nil)
	req.Host = host
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	c := echo.New().NewContext(req, httptest.NewRecorder())
	e.AgentBaseURL = agentBaseURL
	return installHint(c, e, testBYOCMetricsURL)
}

// An amp environment gets a different hint: it installs the amp controller chart
// with its reconcile-agent turned on (the agent is the sole YCP client), not the
// It must be runnable as printed and must not leak the meko chart into
// an amp install.
//
// "Runnable as printed" is load-bearing here: the chart's own defaults are not
// installable (images resolve to docker.io, otel remote-writes to a store that is
// not there), so a hint that omits a value is not neutral — it is broken.
func TestInstallHint_Amp(t *testing.T) {
	hint := hintForEnv(t, &models.Environment{
		ID: "amp-acme-prod", Kind: "amp", APIKeyID: keyA, Placement: "onprem",
	}, "/api/v1/environments", "ycp.example.com", "", nil)

	for _, want := range []string{
		// The key, in a Secret the agent reads from — the chart's default name.
		"create secret generic amp-agent-api-key",
		"--from-literal=api-key=<YCP_AGENT_API_KEY>",
		// The release name every deploy script and the chart README use, so a
		// hint-installed release answers to `helm uninstall yb-amp-controller`.
		"helm upgrade --install yb-amp-controller amp/deploy/helm/yb-amp-controller",
		"--set agent.enabled=true",
		"--set agent.ycpUrl=http://ycp.example.com/api/v1",
		"--set agent.envName=amp-acme-prod",
		"--set agent.apiKeySecret.name=amp-agent-api-key",
		// The registered placement, or the agent silently reports "cloud".
		"--set agent.placement=onprem",
		// One value carries every image; without it they resolve to docker.io/amp/*.
		`--set-string imageTag="${TAG}"`,
		// --set-string, not --set: ? and = in the DSN are mis-tokenized by --set.
		`--set-string controller.databaseUrl="postgres://yugabyte@${YB}/yb_storage?sslmode=disable"`,
		// Without these three the controller comes up unable to pull, unable to
		// reach storage, and shipping metrics to a store that is not there.
		`--set-string controller.endpoint.ybHostPorts="${YB}"`,
		`--set "imagePullSecrets[0].name=${PULL_SECRET}"`,
		// Telemetry is the whole point of a YCP-managed install: without these the
		// console has no metrics for the environment, and the chart's preflight
		// refuses to render.
		"--set otel.enabled=true",
		"--set otel.byoc.enabled=true",
		"--set-string otel.byoc.metricsEndpoint=" + testBYOCMetricsURL,
		// Empty, not absent — the chart reads "" as "drop the in-cluster exporter".
		// Leaving them at their defaults points both collectors at a VictoriaMetrics
		// and a VictoriaLogs no BYOC cluster runs.
		"--set-string otel.remoteWriteEndpoint=",
		"--set-string otel.logsEndpoint=",
		// Both credentials, created before the install: a missing one fails the
		// collector that reads it to start.
		"create secret generic byoc-metrics-auth",
		"create secret generic datadog-api",
		// The operator is a hard prerequisite — the collectors are CRs, and without
		// it they are silently skipped.
		"opentelemetry-operator",
		"otel/opentelemetry-collector-contrib",
		// Tenancy label for every series the collectors scrape (ac-metrics.md).
		"--set otel.environmentId=amp-acme-prod",
		// A YCP-driven environment has no UI of its own, and no cloud LB.
		"--set ui.enabled=false",
		"--set controller.service.type=none",
		// The YugabyteDB prerequisite is called out.
		"YugabyteDB",
	} {
		if !strings.Contains(hint, want) {
			t.Errorf("amp hint is missing %q:\n%s", want, hint)
		}
	}

	// The retired token flow and controller-side YCP loop must not reappear, the
	// meko install must not bleed into an amp hint, and the placeholders this
	// rewrite removed must not come back.
	for _, unwanted := range []string{
		"enrollment-token", "ycp_enr_",
		"controller.ycp", // the controller no longer talks to YCP
		"values-byoc.yaml",
		"<your YugabyteDB DSN>", "<reconcile-agent tag>",
		"--set controller.databaseUrl=", // the mis-tokenizing form
		// The reason the hint existed to print this is gone: a YCP-managed install
		// that ships no telemetry is refused by the chart and by enroll.
		"--set otel.enabled=false",
	} {
		if strings.Contains(hint, unwanted) {
			t.Errorf("amp hint contains retired/foreign string %q:\n%s", unwanted, hint)
		}
	}
}

// The failure mode this rewrite existed to kill was a `<placeholder>` sitting inside
// a command that looks pasteable. Values the server cannot know belong in the
// editable header, so no other line may carry one — checking the shape rather than
// the three specific placeholders that happened to be there.
func TestInstallHint_AmpPlaceholdersOnlyInTheHeader(t *testing.T) {
	hint := hintForEnv(t, &models.Environment{
		ID: "amp-acme-prod", Kind: "amp", APIKeyID: keyA, Placement: "cloud",
	}, "/api/v1/environments", "ycp.example.com", "", nil)

	for _, line := range strings.Split(hint, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		// The header's NAME=<value> lines are the point: they are what the operator
		// edits. A header line is a bare shell assignment — no spaces before the
		// "=" — which is what separates it from a `--set foo=<bar>` flag.
		if name, _, ok := strings.Cut(trimmed, "="); ok && name != "" && !strings.ContainsAny(name, " \t") {
			continue
		}
		if strings.Contains(strings.ReplaceAll(line, apiKeyPlaceholder, ""), "<") {
			t.Errorf("placeholder outside the editable header:\n%s\nin:\n%s", line, hint)
		}
	}
}

// An environment registered without a placement takes the chart default rather than
// printing an empty --set.
func TestInstallHint_AmpOmitsEmptyPlacement(t *testing.T) {
	hint := hintForEnv(t, &models.Environment{
		ID: "amp-acme-prod", Kind: "amp", APIKeyID: keyA,
	}, "/api/v1/environments", "ycp.example.com", "", nil)

	if strings.Contains(hint, "agent.placement") {
		t.Errorf("hint set a placement it was never given:\n%s", hint)
	}
}

// The collectors stamp these on every series they scrape, so one shared metrics
// store can tell one environment's — and one account's — series from another's.
// Both the install command and the --reuse-values snippet have to carry them:
// --reuse-values replays the previous release's values and ignores chart defaults,
// so a value it does not name stays unset.
func TestInstallHint_AmpStampsMetricsIdentity(t *testing.T) {
	hint := hintForEnv(t, &models.Environment{
		ID: "amp-acme-prod", Kind: "amp", APIKeyID: keyA,
		AccountID: "3f2b1c00-0000-0000-0000-000000000009",
	}, "/api/v1/environments", "ycp.example.com", "", nil)

	for _, want := range []string{
		"--set otel.environmentId=amp-acme-prod",
		"--set otel.accountId=3f2b1c00-0000-0000-0000-000000000009",
	} {
		if got := strings.Count(hint, want); got != 2 {
			t.Errorf("want %q in both the install command and the --reuse-values snippet, found %d:\n%s",
				want, got, hint)
		}
	}
}

// A YCP standing up without YBM attributes every environment to the all-zeros
// sentinel, so stamping it would put one meaningless constant on every series in
// the store. The environment id is still stamped.
func TestInstallHint_AmpOmitsSentinelAccount(t *testing.T) {
	hint := hintForEnv(t, &models.Environment{
		ID: "amp-acme-prod", Kind: "amp", APIKeyID: keyA, AccountID: sentinelAccountID,
	}, "/api/v1/environments", "ycp.example.com", "", nil)

	if strings.Contains(hint, "otel.accountId") {
		t.Errorf("hint stamped the standalone sentinel account:\n%s", hint)
	}
	if !strings.Contains(hint, "--set otel.environmentId=amp-acme-prod") {
		t.Errorf("hint dropped the environment id along with the account:\n%s", hint)
	}
}

// One cluster holds many amp environments, one namespace each — the chart is built
// for it. So the namespace is a variable defaulting to the environment id, and no
// command may hardcode one.
func TestInstallHint_AmpNamespaceIsAVariable(t *testing.T) {
	hint := hintForEnv(t, &models.Environment{
		ID: "amp-acme-prod", Kind: "amp", APIKeyID: keyA,
	}, "/api/v1/environments", "ycp.example.com", "", nil)

	if !strings.Contains(hint, "NS=amp-acme-prod") {
		t.Errorf("hint did not default NS to the environment id:\n%s", hint)
	}
	for _, want := range []string{
		`kubectl create namespace "${NS}"`,
		`kubectl -n "${NS}" create secret generic`,
		`amp/deploy/helm/yb-amp-controller -n "${NS}"`,
	} {
		if !strings.Contains(hint, want) {
			t.Errorf("hint is missing %q — a hardcoded namespace collides with the\n"+
				"other environments in the cluster:\n%s", want, hint)
		}
	}
}

// The environment id is only as constrained as the name it was slugged from, which
// is validated as 3-60 chars and nothing else. Anything kubectl would reject falls
// back rather than printing a command that cannot run.
func TestNamespaceFor(t *testing.T) {
	for _, tc := range []struct{ id, want string }{
		{"amp-acme-prod", "amp-acme-prod"},
		{"acme-prod", "acme-prod"},
		{"amp-acme.prod", "amp"},                  // slug() does not strip dots
		{"amp-acme@prod", "amp"},                  // nor anything else
		{"amp-Acme", "amp"},                       // nor uppercase, if it ever reached here
		{"-amp-acme", "amp"},                      // leading dash
		{"amp-acme-", "amp"},                      // trailing dash
		{"amp-" + strings.Repeat("x", 60), "amp"}, // 64 chars: over the 63 limit
		{"", "amp"},
	} {
		if got := namespaceFor(tc.id, "amp"); got != tc.want {
			t.Errorf("namespaceFor(%q) = %q, want %q", tc.id, got, tc.want)
		}
	}
}

// Registered through YBM (agent_base_url + an account), the hint points the agent
// at YBM's per-account pass-through, not at this control plane directly.
func TestInstallHint_UsesAgentBaseURL(t *testing.T) {
	hint := hintFor(t, "meko", "/api/v1/environments", "meko.internal",
		acctA, "https://devcloud.yugabyte.com/api/public/v1/", nil)
	want := "--set reconcileAgent.ycpUrl=https://devcloud.yugabyte.com/api/public/v1/accounts/" + acctA + "/ycp"
	if !strings.Contains(hint, want) {
		t.Errorf("hint did not compose the YBM pass-through URL %q:\n%s", want, hint)
	}
	if strings.Contains(hint, "meko.internal") {
		t.Errorf("hint leaked the direct control-plane address:\n%s", hint)
	}
}

// An agent_base_url with no account to compose it with falls back to the direct
// shape rather than printing a broken /accounts//ycp URL.
func TestInstallHint_AgentBaseURLNeedsAccount(t *testing.T) {
	hint := hintFor(t, "meko", "/api/v1/environments", "ycp.internal",
		"", "https://devcloud.yugabyte.com/api/public/v1", nil)
	if !strings.Contains(hint, "--set reconcileAgent.ycpUrl=http://ycp.internal/api/v1") {
		t.Errorf("hint should fall back to the direct shape without an account:\n%s", hint)
	}
}

// A YCP mounted under a subpath has to print its own URL including that subpath —
// production serves it at https://devcloud.yugabyte.com/ycp/, and an agent pointed at
// the bare domain gets 404s from every call.
func TestInstallHint_CarriesTheBasePath(t *testing.T) {
	hint := hintFor(t, "meko", "/ycp/api/v1/environments", "devcloud.yugabyte.com", "", "", map[string]string{
		"X-Forwarded-Proto": "https",
	})
	if !strings.Contains(hint, "--set reconcileAgent.ycpUrl=https://devcloud.yugabyte.com/ycp/api/v1") {
		t.Errorf("hint did not carry scheme + base path:\n%s", hint)
	}
}

// Behind a TLS-terminating proxy the request arrives as plain HTTP. Printing an
// http:// URL for an https:// control plane sends the agent somewhere that redirects
// or refuses, so the forwarded headers win.
func TestInstallHint_HonorsForwardedHeaders(t *testing.T) {
	hint := hintFor(t, "meko", "/api/v1/environments", "10.0.0.5:25433", "", "", map[string]string{
		"X-Forwarded-Proto": "https, http",
		"X-Forwarded-Host":  "ycp.example.com, internal",
	})
	if !strings.Contains(hint, "--set reconcileAgent.ycpUrl=https://ycp.example.com") {
		t.Errorf("forwarded scheme/host not used:\n%s", hint)
	}
	if strings.Contains(hint, "10.0.0.5") {
		t.Errorf("hint leaked the internal address:\n%s", hint)
	}
}

// The database under an amp environment is exported by YBA, not by the chart's
// collectors, so the hint has to hand the operator the CR blocks that turn it on
// (meko#1405). What matters is the three fields that fail quietly if wrong:
// MINIMAL is the smallest level that takes effect, the target list omits YCQL,
// and the exporter names the provider that carries the credential.
// TABLE_OFF must never appear: the export path drops its parameters silently.
func TestInstallHint_AmpTurnsOnYugabyteDBMetrics(t *testing.T) {
	hint := hintForEnv(t, &models.Environment{
		ID: "amp-acme-prod", Kind: "amp", APIKeyID: keyA,
		AccountID: "3f2b1c00-0000-0000-0000-000000000009",
	}, "/api/v1/environments", "ycp.example.com", "", nil)

	for _, want := range []string{
		// The gate defaults off and is read at yugaware startup, so an operator
		// who pastes only the two CR blocks gets no collector and no error —
		// the exact failure the hint exists to prevent.
		"yb.universe.otel_collector_enabled",
		// The CRs apply only where YBA runs, and the hint says whose they are.
		"OPERATOR-MANAGED FOUNDATION",
		"TelemetryProvider:",
		"provider: OTLP",
		"passwordSecret: {name: byoc-metrics-auth, key: password}",
		"collectionLevel: MINIMAL",
		"scrapeConfigTargets: [MASTER_EXPORT, TSERVER_EXPORT, YSQL_EXPORT]",
		"telemetryProvider: byoc-metrics",
		// The door 415s an uncompressed payload, and every send failing is
		// visible only in the collector's own counters.
		"compression: gzip",
		"environment_id: amp-acme-prod",
		"account_id: 3f2b1c00-0000-0000-0000-000000000009",
	} {
		if !strings.Contains(hint, want) {
			t.Errorf("hint does not carry %q:\n%s", want, hint)
		}
	}
	// A password in the block would be a credential printed into a file the
	// operator then commits. It reaches YBA by Secret reference or not at all.
	if strings.Contains(hint, "password: ") {
		t.Errorf("hint puts a literal password in the TelemetryProvider:\n%s", hint)
	}
}

// Ordering is the point of putting this in the hint at all: the exporter names a
// Secret that step 2 creates, and the identity tags exist nowhere earlier. Printed
// before the Secret it would not work; printed after the install the universe
// would have spent that window collecting nothing.
func TestInstallHint_AmpTurnsOnMetricsAfterTheSecretAndBeforeTheInstall(t *testing.T) {
	hint := hintForEnv(t, &models.Environment{
		ID: "amp-acme-prod", Kind: "amp", APIKeyID: keyA,
	}, "/api/v1/environments", "ycp.example.com", "", nil)

	secret := strings.Index(hint, "create secret generic byoc-metrics-auth")
	telemetry := strings.Index(hint, "collectionLevel: MINIMAL")
	install := strings.Index(hint, "helm upgrade --install yb-amp-controller")
	if secret < 0 || telemetry < 0 || install < 0 {
		t.Fatalf("hint is missing one of the three steps:\n%s", hint)
	}
	if !(secret < telemetry && telemetry < install) {
		t.Errorf("steps out of order (secret %d, telemetry %d, install %d):\n%s",
			secret, telemetry, install, hint)
	}
}

// A control plane with no door has nowhere to send the database's metrics, so the
// step is absent — and the sequence must still read 1, 2, 3 rather than skipping
// a number, which reads as a step the operator missed.
func TestInstallHint_AmpNumbersTheInstallWhenThereIsNoDoor(t *testing.T) {
	req := httptest.NewRequest("POST", "/api/v1/environments", nil)
	req.Host = "ycp.example.com"
	c := echo.New().NewContext(req, httptest.NewRecorder())
	hint := installHint(c, &models.Environment{
		ID: "amp-acme-prod", Kind: "amp", APIKeyID: keyA,
	}, "")

	if strings.Contains(hint, "TelemetryProvider:") {
		t.Errorf("hint declares a telemetry provider with no door to point it at:\n%s", hint)
	}
	if !strings.Contains(hint, "# 3 — install the amp controller") {
		t.Errorf("install step is not numbered 3 with the telemetry step absent:\n%s", hint)
	}
}
