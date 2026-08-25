package handlers

import (
	"fmt"
	"net/http"
	"strings"

	"github.com/labstack/echo/v4"

	"meko/api_server/internal/models"
)

// registerRoutePath is the route this hint is generated from. Trimming it off the
// request path is what yields the base path the control plane is mounted under, so
// a YCP served at https://devcloud.yugabyte.com/ycp/ prints its own real URL rather
// than a placeholder the operator has to know to replace.
const registerRoutePath = "/api/v1/environments"

// defaultAgentNamespace is where the install puts the agent when the operator does
// not choose otherwise. Its own namespace, not the control plane's: the agent holds
// a long-lived credential and reconciles the Meko stack beside itself.
const defaultAgentNamespace = "ycp-agent"

// apiKeyPlaceholder is what the hint prints where the plaintext API key goes. The
// control plane never sees the key — YBM mints it and shows it once — so the YBM UI
// substitutes the real value; an operator reading the hint raw pastes their own.
const apiKeyPlaceholder = "<YCP_AGENT_API_KEY>" //nolint:gosec // a placeholder, not a credential

// installHint renders the commands that attach an agent to a freshly registered
// environment. It is the only place an operator is told how to do this, so it has to
// be runnable as printed, not a sketch.
//
// The hint is kind-specific: a meko environment installs the meko-agent (a
// component of the `meko` chart), an amp environment installs the amp controller
// and its reconcile-agent (the yb-amp-controller chart's `agent` block) — the
// agent is the sole YCP client and drives the controller over its REST API.
func installHint(c echo.Context, e *models.Environment, agentBaseURL string) string {
	url := agentURL(c, e.AccountID, agentBaseURL)
	if e.Kind == "amp" {
		return ampInstallHint(url, e.ID)
	}
	return mekoInstallHint(url, e.ID)
}

// mekoInstallHint attaches the meko-agent to a meko environment.
//
// The previous version was a sketch, and each gap in it was a support question: it
// named a `meko/meko-agent` chart in a `meko` Helm repo (neither exists — the agent
// is a component of the `meko` chart in meko-devops), left `<ns>` and
// `<this control plane>` for the reader to fill in, and used
// `--set agent.enabled=true`, which switches the agent on without switching the
// other seven components off. That last one is how a data-plane install ends up
// putting an api-server and a Langfuse in a customer's cluster; `values-agent.yaml`
// is the preset that exists to prevent it.
func mekoInstallHint(ycpURL, envID string) string {
	ns := defaultAgentNamespace

	var b strings.Builder
	fmt.Fprintf(&b, "# 1 — hand the cluster the agent's API key (shown once by YBM; revocable there)\n")
	fmt.Fprintf(&b, "kubectl create namespace %s\n", ns)
	fmt.Fprintf(&b, "kubectl -n %s create secret generic meko-agent-api-key \\\n", ns)
	fmt.Fprintf(&b, "  --from-literal=api-key=%s\n\n", apiKeyPlaceholder)

	fmt.Fprintf(&b, "# 2 — install the agent, from a meko-devops checkout\n")
	fmt.Fprintf(&b, "#      swap values-aws.yaml for values-gcp.yaml, and values-dev.yaml for\n")
	fmt.Fprintf(&b, "#      values-prod.yaml, to match the cluster. values-agent.yaml goes LAST:\n")
	fmt.Fprintf(&b, "#      it is what suppresses every component except the agent.\n")
	fmt.Fprintf(&b, "helm upgrade --install meko-agent charts/meko -n %s \\\n", ns)
	fmt.Fprintf(&b, "  -f charts/meko/values-aws.yaml -f charts/meko/values-dev.yaml \\\n")
	fmt.Fprintf(&b, "  -f charts/meko/values-agent.yaml \\\n")
	fmt.Fprintf(&b, "  --set agent.ycpUrl=%s \\\n", ycpURL)
	fmt.Fprintf(&b, "  --set agent.envName=%s \\\n", envID)
	fmt.Fprintf(&b, "  --set-string agent.image.tag=<meko-agent tag>\n\n")

	fmt.Fprintf(&b, "# The agent enrolls, then brings up what this environment was registered with.\n")
	fmt.Fprintf(&b, "# There is no third step: registration already declared its desired state.\n")
	fmt.Fprintf(&b, "#   kubectl -n %s logs -f deploy/meko-agent\n", ns)
	return b.String()
}

// ampInstallNamespace is where the amp controller install puts the controller
// when the operator does not choose otherwise.
const ampInstallNamespace = "amp"

// ampAPIKeySecretName is the Secret the reconcile-agent reads the API key from. It
// is passed to the chart's agent.apiKeySecret.name so the created Secret and the
// agent Deployment's secretKeyRef agree; it is the chart's default name, but we set
// it explicitly so the two can't drift.
const ampAPIKeySecretName = "amp-agent-api-key" //nolint:gosec // a Secret NAME, not a credential

// ampInstallHint attaches an amp environment to this control plane. It installs
// the amp controller plus its reconcile-agent: the agent enrolls, binds the
// controller, and converges it to the environment's desired state, driving it
// over the REST API. The controller itself no longer talks to YCP. A YugabyteDB
// foundation cluster must already be running (the controller's state store).
func ampInstallHint(ycpURL, envID string) string {
	ns := ampInstallNamespace

	var b strings.Builder
	fmt.Fprintf(&b, "# Prerequisite: a YugabyteDB foundation cluster already running in this\n")
	fmt.Fprintf(&b, "# cluster — it is the amp controller's state store.\n\n")

	fmt.Fprintf(&b, "# 1 — hand the cluster the agent's API key (shown once by YBM; revocable\n")
	fmt.Fprintf(&b, "#      there). The reconcile-agent reads it, not the controller.\n")
	fmt.Fprintf(&b, "kubectl create namespace %s\n", ns)
	fmt.Fprintf(&b, "kubectl -n %s create secret generic %s \\\n", ns, ampAPIKeySecretName)
	fmt.Fprintf(&b, "  --from-literal=api-key=%s\n\n", apiKeyPlaceholder)

	fmt.Fprintf(&b, "# 2 — install the amp controller and its reconcile-agent (from an amp checkout).\n")
	fmt.Fprintf(&b, "#      The agent is the sole YCP client; the controller only serves its REST API.\n")
	fmt.Fprintf(&b, "helm upgrade --install amp-controller amp/deploy/helm/yb-amp-controller -n %s \\\n", ns)
	fmt.Fprintf(&b, "  --set controller.databaseUrl=<your YugabyteDB DSN> \\\n")
	fmt.Fprintf(&b, "  --set agent.enabled=true \\\n")
	fmt.Fprintf(&b, "  --set agent.ycpUrl=%s \\\n", ycpURL)
	fmt.Fprintf(&b, "  --set agent.envName=%s \\\n", envID)
	fmt.Fprintf(&b, "  --set agent.apiKeySecret.name=%s \\\n", ampAPIKeySecretName)
	fmt.Fprintf(&b, "  --set-string agent.image.tag=<reconcile-agent tag>\n\n")

	fmt.Fprintf(&b, "# The agent enrolls, binds this controller, then converges it to the environment's\n")
	fmt.Fprintf(&b, "# desired state and reports the result to this console.\n")
	fmt.Fprintf(&b, "#   kubectl -n %s logs -f deploy/amp-reconcile-agent\n", ns)
	return b.String()
}

// agentURL is the base the agent dials. Registered through YBM (agent_base_url +
// an account), that is YBM's agent pass-through for the account; otherwise it
// falls back to the address this register call arrived on plus /api/v1 — the
// direct-to-meko shape a dev-mode agent uses.
func agentURL(c echo.Context, accountID, agentBaseURL string) string {
	if agentBaseURL != "" && accountID != "" {
		return strings.TrimRight(agentBaseURL, "/") + "/accounts/" + accountID + "/ycp"
	}
	return controlPlaneURL(c) + "/api/v1"
}

// controlPlaneURL reconstructs the URL the caller reached this control plane on, for
// the agent to dial back. It is a default, not a discovery: an agent inside another
// cluster may need a different address (a private LB, a service DNS name) than the
// browser or CLI that registered the environment.
func controlPlaneURL(c echo.Context) string {
	r := c.Request()
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	// Behind a proxy the request arrives as plain HTTP even when the client used TLS,
	// so a hint that ignored this would print an http:// URL for an https:// YCP.
	if proto := r.Header.Get("X-Forwarded-Proto"); proto != "" {
		scheme = strings.TrimSpace(strings.Split(proto, ",")[0])
	}
	host := r.Host
	if fwd := r.Header.Get("X-Forwarded-Host"); fwd != "" {
		host = strings.TrimSpace(strings.Split(fwd, ",")[0])
	}
	if host == "" {
		return "<this control plane's base URL>"
	}
	return scheme + "://" + host + basePathOf(r)
}

// basePathOf recovers the prefix the control plane is mounted under from the request
// path, since the handler layer is not told the --base-path value.
func basePathOf(r *http.Request) string {
	p := r.URL.Path
	if i := strings.LastIndex(p, registerRoutePath); i > 0 {
		return p[:i]
	}
	return ""
}
