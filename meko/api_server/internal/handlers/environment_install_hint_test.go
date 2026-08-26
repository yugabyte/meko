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
	return installHint(c, e, agentBaseURL)
}

// The hint is the only place an operator is told how to attach an agent, so it has to
// be runnable as printed. These are the specific ways the old one was not.
func TestInstallHint_IsRunnableAsPrinted(t *testing.T) {
	hint := hintFor(t, "meko", "/api/v1/environments", "ycp.example.com", "", "", nil)

	for _, want := range []string{
		// The key placeholder, in a Secret rather than on a command line. The YBM UI
		// substitutes the real key; the control plane never sees it.
		"create secret generic meko-agent-api-key",
		"--from-literal=api-key=<YCP_AGENT_API_KEY>",
		// The chart that actually exists: a component of `meko` in meko-devops.
		"helm upgrade --install meko-agent charts/meko",
		// values-agent.yaml is what suppresses the other seven components. Without it
		// a data-plane install puts an api-server and a Langfuse in someone's cluster.
		"-f charts/meko/values-agent.yaml",
		// No agent_base_url: the direct-to-meko shape, with the API base appended.
		"--set agent.ycpUrl=http://ycp.example.com/api/v1",
		"--set agent.envName=acme-prod",
		// The one thing the server genuinely cannot know.
		"--set-string agent.image.tag=",
	} {
		if !strings.Contains(hint, want) {
			t.Errorf("hint is missing %q:\n%s", want, hint)
		}
	}

	// The retired token flow must not resurface, and the old hint's placeholders
	// must stay gone.
	for _, unwanted := range []string{
		"enrollment-token", "30-minute", "ycp_enr_",
		"<ns>", "<this control plane>", "--set agent.enabled=true",
		// The meko chart has no otel block; the amp hint's metrics values are not
		// hers to set.
		"otel.",
	} {
		if strings.Contains(hint, unwanted) {
			t.Errorf("hint still contains %q:\n%s", unwanted, hint)
		}
	}
}

// An amp environment gets a different hint: it installs the amp controller chart
// with its reconcile-agent turned on (the agent is the sole YCP client), not the
// meko-agent. It must be runnable as printed and must not leak the meko chart into
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
		"--set otel.enabled=false",
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
	// meko-agent install must not bleed into an amp hint, and the placeholders this
	// rewrite removed must not come back.
	for _, unwanted := range []string{
		"enrollment-token", "ycp_enr_",
		"controller.ycp", // the controller no longer talks to YCP
		"meko-agent", "values-agent.yaml",
		"<your YugabyteDB DSN>", "<reconcile-agent tag>",
		"--set controller.databaseUrl=", // the mis-tokenizing form
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
		if got := namespaceFor(tc.id); got != tc.want {
			t.Errorf("namespaceFor(%q) = %q, want %q", tc.id, got, tc.want)
		}
	}
}

// Registered through YBM (agent_base_url + an account), the hint points the agent
// at YBM's per-account pass-through, not at this control plane directly.
func TestInstallHint_UsesAgentBaseURL(t *testing.T) {
	hint := hintFor(t, "meko", "/api/v1/environments", "meko.internal",
		acctA, "https://devcloud.yugabyte.com/api/public/v1/", nil)
	want := "--set agent.ycpUrl=https://devcloud.yugabyte.com/api/public/v1/accounts/" + acctA + "/ycp"
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
	if !strings.Contains(hint, "--set agent.ycpUrl=http://ycp.internal/api/v1") {
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
	if !strings.Contains(hint, "--set agent.ycpUrl=https://devcloud.yugabyte.com/ycp/api/v1") {
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
	if !strings.Contains(hint, "--set agent.ycpUrl=https://ycp.example.com") {
		t.Errorf("forwarded scheme/host not used:\n%s", hint)
	}
	if strings.Contains(hint, "10.0.0.5") {
		t.Errorf("hint leaked the internal address:\n%s", hint)
	}
}
