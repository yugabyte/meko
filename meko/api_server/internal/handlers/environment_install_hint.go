package handlers

import (
	"fmt"
	"net/http"
	"strings"

	"github.com/labstack/echo/v4"
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

// installHint renders the commands that attach an agent to a freshly registered
// environment. It is the only place an operator is told how to do this, so it has to
// be runnable as printed, not a sketch.
//
// The previous version was a sketch, and each gap in it was a support question: it
// named a `meko/meko-agent` chart in a `meko` Helm repo (neither exists — the agent
// is a component of the `meko` chart in meko-devops), left `<ns>` and
// `<this control plane>` for the reader to fill in, and used
// `--set agent.enabled=true`, which switches the agent on without switching the
// other seven components off. That last one is how a data-plane install ends up
// putting an api-server and a Langfuse in a customer's cluster; `values-agent.yaml`
// is the preset that exists to prevent it.
func installHint(c echo.Context, envID, token string) string {
	ycpURL := controlPlaneURL(c)
	ns := defaultAgentNamespace

	var b strings.Builder
	fmt.Fprintf(&b, "# 1 — hand the cluster this one-time enrollment token (30-minute TTL, so install promptly)\n")
	fmt.Fprintf(&b, "kubectl create namespace %s\n", ns)
	fmt.Fprintf(&b, "kubectl -n %s create secret generic meko-agent-enrollment \\\n", ns)
	fmt.Fprintf(&b, "  --from-literal=enrollment-token=%s\n\n", token)

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
