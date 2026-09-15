package handlers

import (
	"fmt"
	"net/http"
	"regexp"
	"strings"

	"github.com/labstack/echo/v4"

	"meko/api_server/internal/models"
	"meko/api_server/internal/service"
)

// envRoutePath is the collection route every hint is generated under — register
// posts to it, get-one reads a path beneath it. Trimming it off the request path is
// what yields the base path the control plane is mounted under, so a YCP served at
// https://devcloud.yugabyte.com/ycp/ prints its own real URL rather than a
// placeholder the operator has to know to replace.
const envRoutePath = "/api/v1/environments"

// defaultAgentNamespace is where the install puts the agent when the operator does
// not choose otherwise. Its own namespace, not the control plane's: the agent holds
// a long-lived credential and reconciles the Meko stack beside itself.
const defaultAgentNamespace = "ycp-agent"

// apiKeyPlaceholder is what the hint prints where the plaintext API key goes. The
// control plane never sees the key — YBM mints it and shows it once — so the YBM UI
// substitutes the real value; an operator reading the hint raw pastes their own.
const apiKeyPlaceholder = "<YCP_AGENT_API_KEY>" //nolint:gosec // a placeholder, not a credential

// installHint renders the commands that attach an agent to an environment. It is the
// only place an operator is told how to do this, so it has to be runnable as printed,
// not a sketch.
//
// One shape per kind, because each kind has one agent: an amp environment installs
// the amp controller and its reconcile-agent, a meko environment installs the meko
// stack and the same reconcile-agent. Which components an environment runs is the
// chart's business either way; desired state declares only what lives inside them.
func installHint(c echo.Context, e *models.Environment) string {
	url := agentURL(c, e.AccountID, e.AgentBaseURL)
	if e.Kind == "amp" {
		return ampInstallHint(url, e.ID, e.Placement, e.AccountID)
	}
	return mekoReconcileInstallHint(url, e.ID, service.DefaultDataPlaneRelease(""))
}

// mekoStackNamespace is where a chart-installed meko data plane goes when the
// environment id will not serve as a namespace.
const mekoStackNamespace = "meko"

// mekoPullSecretName is the docker-registry Secret every pod in a BYOC release pulls
// with. Named here and in values-byoc.yaml's global.imagePullSecrets +
// yugabyte.Image.pullSecretName — three places that must agree, because the subchart
// does not read the chart's list and would otherwise be the one pod that cannot pull.
const mekoPullSecretName = "quay-registry-creds"

// mekoTokenSecretName is the Secret the reconcile-agent reads its api-server token
// from. Passed to the chart explicitly so the created Secret and the Deployment's
// secretKeyRef cannot drift, even though it is the chart's default.
const mekoTokenSecretName = "meko-reconcile-agent-token" //nolint:gosec // a Secret NAME, not a credential

// mekoReconcileInstallHint attaches a meko environment: the whole data plane from
// the chart, plus the reconcile-agent, which converges the environment's declared
// resources over the api-server's REST API.
//
// The only values it cannot know are the image tags, so they are lifted into a
// header the operator edits once rather than left mid-command. There is no
// api-server credential step: this preset's api-server runs without Cognito and
// authenticates nobody. Nothing keeps the reconciler off a person's datapacks —
// it is the api-server's only caller and is authoritative over it, which the hint
// says outright, because an operator who points one at a shared api-server needs
// to know before running it.
func mekoReconcileInstallHint(ycpURL, envID string, rel service.DataPlaneRelease) string {
	ns := namespaceFor(envID, mekoStackNamespace)
	chart := "meko/deploy/helm/meko"

	var b strings.Builder
	fmt.Fprintf(&b, "# Prerequisites\n")
	fmt.Fprintf(&b, "#   - a meko checkout — the chart is not published to a Helm repo\n")
	fmt.Fprintf(&b, "#   - a cluster with a default StorageClass (the bundled YugabyteDB claims it)\n")
	fmt.Fprintf(&b, "#   - a Quay robot credential that can read quay.io/yugabyte/meko-*\n\n")

	// The tags are one release across the stack, but printed separately anyway: the
	// pin can name different tags per component, and a hint that collapsed them to
	// one variable would be wrong the moment it did.
	fmt.Fprintf(&b, "# Fill these in, then paste the rest as-is.\n")
	fmt.Fprintf(&b, "NS=%s\n", ns)
	fmt.Fprintf(&b, "API_TAG=%s\n", tagOf(rel.APIServer))
	fmt.Fprintf(&b, "MCP_TAG=%s\n", tagOf(rel.MCPServer))
	fmt.Fprintf(&b, "UI_TAG=%s\n", tagOf(rel.UI))
	fmt.Fprintf(&b, "AGENT_TAG=<agent image tag>     # quay.io/yugabyte/ycp-reconciler-agent\n")
	fmt.Fprintf(&b, "QUAY_USER=<robot name>         # e.g. yugabyte+something\n")
	fmt.Fprintf(&b, "QUAY_TOKEN=<robot token>\n\n")

	fmt.Fprintf(&b, "# 1 — two credentials. The agent's API key (shown once by YBM; revocable\n")
	fmt.Fprintf(&b, "#      there), and a pull secret: every image in this release is on private\n")
	fmt.Fprintf(&b, "#      Quay, and %s is the name values-byoc.yaml expects.\n", mekoPullSecretName)
	fmt.Fprintf(&b, "kubectl create namespace \"${NS}\" --dry-run=client -o yaml | kubectl apply -f -\n")
	fmt.Fprintf(&b, "kubectl -n \"${NS}\" create secret generic meko-agent-api-key \\\n")
	fmt.Fprintf(&b, "  --from-literal=api-key=%s \\\n", apiKeyPlaceholder)
	fmt.Fprintf(&b, "  --dry-run=client -o yaml | kubectl apply -f -\n")
	fmt.Fprintf(&b, "kubectl -n \"${NS}\" create secret docker-registry %s \\\n", mekoPullSecretName)
	fmt.Fprintf(&b, "  --docker-server=quay.io --docker-username=\"${QUAY_USER}\" --docker-password=\"${QUAY_TOKEN}\" \\\n")
	fmt.Fprintf(&b, "  --dry-run=client -o yaml | kubectl apply -f -\n\n")

	fmt.Fprintf(&b, "# 2 — install the stack and the reconcile-agent, from a meko checkout. Swap\n")
	fmt.Fprintf(&b, "#      values-aws.yaml for values-gcp.yaml, and values-dev.yaml for\n")
	fmt.Fprintf(&b, "#      values-prod.yaml, to match the cluster. values-byoc.yaml goes LAST:\n")
	fmt.Fprintf(&b, "#      it is what makes this one self-contained namespace.\n")
	fmt.Fprintf(&b, "helm upgrade --install meko %s -n \"${NS}\" \\\n", chart)
	fmt.Fprintf(&b, "  -f %s/values-aws.yaml -f %s/values-dev.yaml \\\n", chart, chart)
	fmt.Fprintf(&b, "  -f %s/values-byoc.yaml \\\n", chart)
	fmt.Fprintf(&b, "  --set-string apiServer.image.tag=\"${API_TAG}\" \\\n")
	fmt.Fprintf(&b, "  --set-string mekoMcpServer.image.tag=\"${MCP_TAG}\" \\\n")
	fmt.Fprintf(&b, "  --set-string mekoUi.image.tag=\"${UI_TAG}\" \\\n")
	fmt.Fprintf(&b, "  --set reconcileAgent.enabled=true \\\n")
	fmt.Fprintf(&b, "  --set reconcileAgent.ycpUrl=%s \\\n", ycpURL)
	fmt.Fprintf(&b, "  --set reconcileAgent.envName=%s \\\n", envID)
	fmt.Fprintf(&b, "  --set-string reconcileAgent.image.tag=\"${AGENT_TAG}\"\n\n")

	fmt.Fprintf(&b, "# The agent enrolls, then converges the agents and datapacks this environment\n")
	fmt.Fprintf(&b, "# declares.\n")
	fmt.Fprintf(&b, "# Declare them with POST %s/environments/%s/desired.\n", ycpURL, envID)
	fmt.Fprintf(&b, "#   kubectl -n \"${NS}\" rollout status deploy/meko-api-server\n")
	fmt.Fprintf(&b, "#   kubectl -n \"${NS}\" logs -f deploy/meko-reconcile-agent\n")
	fmt.Fprintf(&b, "#\n")
	fmt.Fprintf(&b, "# The agent is this api-server's only caller and is authoritative over it:\n")
	fmt.Fprintf(&b, "# anything desired state stops declaring is collected. On an api-server with\n")
	fmt.Fprintf(&b, "# Cognito ON, add the account's MCP token:\n")
	fmt.Fprintf(&b, "#   kubectl -n \"${NS}\" create secret generic %s \\\n", mekoTokenSecretName)
	fmt.Fprintf(&b, "#     --from-literal=meko-api-token=<token>\n")
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

// sentinelAccountID is the all-zeros account the fleet registry defaults to when
// YCP runs without YBM to attribute an environment to.
const sentinelAccountID = service.SentinelAccountID

// metricsIdentityArgs are the --set arguments that stamp this environment's
// identity on every series the OTel collectors scrape (ac-metrics.md). The
// account is dropped when it is the standalone sentinel: the same zeros on every
// series is worse than no label at all.
//
// service.MetricsAccountID decides that, rather than a comparison here, because
// the metrics read path applies the same rule to build its query — the label a
// hint stamps and the label a query matches on have to be the one decision.
func metricsIdentityArgs(envID, accountID string) []string {
	args := []string{"otel.environmentId=" + envID}
	if account := service.MetricsAccountID(accountID); account != "" {
		args = append(args, "otel.accountId="+account)
	}
	return args
}

// tagOf is the tag half of an image reference, for a hint that prints tags an
// operator can paste. A reference with no tag (or a digest) yields a placeholder
// rather than something that looks pasteable and is not.
func tagOf(image string) string {
	i := strings.LastIndex(image, ":")
	if i < 0 || strings.Contains(image[i+1:], "/") || strings.Contains(image, "@") {
		return "<tag>"
	}
	return image[i+1:]
}

var dns1123Label = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)

// namespaceFor is the namespace an install defaults to: the environment id, so a
// cluster can hold several environments without the operator having to think about
// it (each chart installs once per namespace — amp's cluster-scoped RBAC names are
// namespace-prefixed for exactly this). The id is not guaranteed to be a legal
// namespace: slug() only lowercases and maps space/underscore/slash, over a name
// validated as 3-60 chars, so an id can exceed 63 or carry a dot. Fall back rather
// than print something kubectl will reject.
func namespaceFor(envID, fallback string) string {
	if len(envID) == 0 || len(envID) > 63 || !dns1123Label.MatchString(envID) {
		return fallback
	}
	return envID
}

// ampInstallHint attaches an amp environment to this control plane: the amp
// controller plus its reconcile-agent, which binds the controller and converges it
// to the environment's desired state over the REST API. The controller itself never
// talks to YCP.
//
// Everything the control plane can know is filled in. What is left are four values
// it cannot — the YugabyteDB endpoint, the image tag, the pull secret, and the
// namespace if the default does not suit — and those are lifted into a header the
// operator edits once, rather than left as placeholders inside a command that looks
// pasteable and is not. The chart's own defaults are not usable as-is: the images
// resolve to docker.io, otel remote-writes to a store that is not there, and the UI
// and an internal LoadBalancer would both be deployed for nobody.
func ampInstallHint(ycpURL, envID, placement, accountID string) string {
	ns := namespaceFor(envID, ampInstallNamespace)

	var b strings.Builder
	fmt.Fprintf(&b, "# Prerequisites\n")
	fmt.Fprintf(&b, "#   - YugabyteDB up and reachable from this cluster, with a yb_storage database:\n")
	fmt.Fprintf(&b, "#       CREATE DATABASE yb_storage;\n")
	fmt.Fprintf(&b, "#   - a meko checkout — the chart is not published to a Helm repo\n")
	fmt.Fprintf(&b, "#   - a docker-registry Secret for quay.io/yugabyte, in the namespace below\n\n")

	fmt.Fprintf(&b, "# Fill these in, then paste the rest as-is.\n")
	fmt.Fprintf(&b, "NS=%s\n", ns)
	fmt.Fprintf(&b, "YB=<yb-tserver-host>:5433      # YugabyteDB tserver endpoint (host:port)\n")
	fmt.Fprintf(&b, "TAG=<amp image tag>            # quay.io/yugabyte/amp-* tag, e.g. custom-7f195e43e51f\n")
	fmt.Fprintf(&b, "PULL_SECRET=<secret name>      # docker-registry Secret for quay.io/yugabyte\n\n")

	fmt.Fprintf(&b, "# 1 — hand the cluster the agent's API key (shown once by YBM; revocable\n")
	fmt.Fprintf(&b, "#      there). The reconcile-agent reads it, not the controller.\n")
	fmt.Fprintf(&b, "kubectl create namespace \"${NS}\" --dry-run=client -o yaml | kubectl apply -f -\n")
	fmt.Fprintf(&b, "kubectl -n \"${NS}\" create secret generic %s \\\n", ampAPIKeySecretName)
	fmt.Fprintf(&b, "  --from-literal=api-key=%s \\\n", apiKeyPlaceholder)
	fmt.Fprintf(&b, "  --dry-run=client -o yaml | kubectl apply -f -\n\n")

	fmt.Fprintf(&b, "# 2 — install the amp controller and its reconcile-agent, from a meko checkout.\n")
	fmt.Fprintf(&b, "#      The agent is the sole YCP client; the controller only serves its REST API.\n")
	fmt.Fprintf(&b, "helm upgrade --install yb-amp-controller amp/deploy/helm/yb-amp-controller -n \"${NS}\" \\\n")
	fmt.Fprintf(&b, "  --set-string imageTag=\"${TAG}\" \\\n")
	// --set-string, not --set: the DSN's ? and = are mis-tokenized by --set.
	fmt.Fprintf(&b, "  --set-string controller.databaseUrl=\"postgres://yugabyte@${YB}/yb_storage?sslmode=disable\" \\\n")
	fmt.Fprintf(&b, "  --set-string controller.endpoint.ybHostPorts=\"${YB}\" \\\n")
	fmt.Fprintf(&b, "  --set \"imagePullSecrets[0].name=${PULL_SECRET}\" \\\n")
	fmt.Fprintf(&b, "  --set controller.service.type=none \\\n")
	fmt.Fprintf(&b, "  --set ui.enabled=false \\\n")
	fmt.Fprintf(&b, "  --set otel.enabled=false \\\n")
	for _, a := range metricsIdentityArgs(envID, accountID) {
		fmt.Fprintf(&b, "  --set %s \\\n", a)
	}
	fmt.Fprintf(&b, "  --set agent.enabled=true \\\n")
	fmt.Fprintf(&b, "  --set agent.ycpUrl=%s \\\n", ycpURL)
	fmt.Fprintf(&b, "  --set agent.envName=%s \\\n", envID)
	if placement != "" {
		fmt.Fprintf(&b, "  --set agent.placement=%s \\\n", placement)
	}
	fmt.Fprintf(&b, "  --set agent.apiKeySecret.name=%s\n\n", ampAPIKeySecretName)

	fmt.Fprintf(&b, "# The agent binds this controller, then converges it to the environment's desired\n")
	fmt.Fprintf(&b, "# state and reports the result to this console.\n")
	fmt.Fprintf(&b, "#   kubectl -n \"${NS}\" rollout status deploy/amp-controller\n")
	fmt.Fprintf(&b, "#   kubectl -n \"${NS}\" logs -f deploy/amp-reconcile-agent\n")
	fmt.Fprintf(&b, "#\n")
	fmt.Fprintf(&b, "# otel is off because its default remote-write endpoint is a metrics store this\n")
	fmt.Fprintf(&b, "# cluster very likely does not run. Turn it back on with otel.remoteWriteEndpoint\n")
	fmt.Fprintf(&b, "# set, on a cluster running the OpenTelemetry Operator; the environmentId and\n")
	fmt.Fprintf(&b, "# accountId above then label every series it scrapes.\n")
	fmt.Fprintf(&b, "#\n")
	fmt.Fprintf(&b, "# Already running an amp controller in this namespace? Attach the agent to that\n")
	fmt.Fprintf(&b, "# release instead, leaving every other value untouched:\n")
	fmt.Fprintf(&b, "#   helm upgrade yb-amp-controller amp/deploy/helm/yb-amp-controller \\\n")
	fmt.Fprintf(&b, "#     -n \"${NS}\" --reuse-values \\\n")
	fmt.Fprintf(&b, "#     --set agent.enabled=true --set agent.ycpUrl=%s \\\n", ycpURL)
	fmt.Fprintf(&b, "#     --set agent.envName=%s ", envID)
	if placement != "" {
		fmt.Fprintf(&b, "--set agent.placement=%s ", placement)
	}
	fmt.Fprintf(&b, "\\\n")
	for _, a := range metricsIdentityArgs(envID, accountID) {
		fmt.Fprintf(&b, "#     --set %s \\\n", a)
	}
	fmt.Fprintf(&b, "#     --set agent.apiKeySecret.name=%s\n", ampAPIKeySecretName)
	return b.String()
}

// agentURL is the base the agent dials. Registered through YBM (agent_base_url +
// an account), that is YBM's agent pass-through for the account; otherwise it
// falls back to the address this call arrived on plus /api/v1 — the
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
	if i := strings.LastIndex(p, envRoutePath); i > 0 {
		return p[:i]
	}
	return ""
}
