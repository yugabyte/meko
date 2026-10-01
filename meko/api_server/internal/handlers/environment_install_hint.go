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
// control plane never sees the key — YBM mints it and shows it once, in its own
// box beside this hint, which it prints verbatim — so the operator pastes it into
// the fill-in header. Never inside a command: pasted unedited there, the
// placeholder becomes the key and the agent 401s.
const apiKeyPlaceholder = "<YCP_AGENT_API_KEY>" //nolint:gosec // a placeholder, not a credential

// The two telemetry Secrets the collectors read by secretKeyRef. Named here
// because the hint creates them, and left at the chart's defaults so the install
// passes no --set for either: one name, in one place, for the create and the read.
const (
	byocMetricsSecretName = "byoc-metrics-auth" //nolint:gosec // a Secret NAME, not a credential
	datadogSecretName     = "datadog-api"       //nolint:gosec // a Secret NAME, not a credential
	// ybPasswordSecretName holds the YSQL password the controller and its endpoints
	// both read (controller.ybPasswordSecret). A Secret rather than a DSN field: the
	// DSN goes through --set-string, which splits on "," and eats "\\".
	ybPasswordSecretName = "amp-yb-password" //nolint:gosec // a Secret NAME, not a credential
	// byocMetricsUsername is the BasicAuth user the door's nginx checks against
	// its htpasswd, and the chart's otel.byoc.username default. Repeated here
	// because the YugabyteDB telemetry block below is not the chart's and cannot
	// inherit it.
	byocMetricsUsername = "byoc"
	// ybTelemetryProviderName is the TelemetryProvider CR the universe's exporter
	// references. Only a name — the credential is the Secret above.
	ybTelemetryProviderName = "byoc-metrics"
)

// installHint renders the commands that attach an agent to an environment. It is the
// only place an operator is told how to do this, so it has to be runnable as printed,
// not a sketch.
//
// One shape per kind, because each kind has one agent: an amp environment installs
// the amp controller and its reconcile-agent, a meko environment installs the meko
// stack and the same reconcile-agent. Which components an environment runs is the
// chart's business either way; desired state declares only what lives inside them.
func installHint(c echo.Context, e *models.Environment, byocMetricsURL string) string {
	url := agentURL(c, e.AccountID, e.AgentBaseURL)
	if e.Kind == "amp" {
		return ampInstallHint(url, e.ID, e.Placement, e.AccountID, byocMetricsURL)
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

// ampControllerChart is the published controller chart, the same reference the
// reconcile-agent upgrades the release from. No --version: helm resolves the
// newest release, and the agent moves it to any version YCP declares.
const ampControllerChart = "oci://quay.io/yugabyte/charts/yb-amp-controller"

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
	// values-byoc.yaml enables the inference gateway, so its tag is no longer
	// optional here: unset, it falls through to the env preset's ECR tag under
	// quay.io/yugabyte and the pod never pulls.
	fmt.Fprintf(&b, "GW_TAG=%s\n", tagOf(rel.InferenceGateway))
	fmt.Fprintf(&b, "AGENT_TAG=<agent image tag>     # quay.io/yugabyte/ycp-reconciler-agent\n")
	fmt.Fprintf(&b, "AGENT_API_KEY='%s'   # shown once by the YCP console; revocable there\n", apiKeyPlaceholder)
	// The gateway's Secret is mounted with envFrom and no optional=true, so an
	// install that omits these gets CreateContainerConfigError, not a missing
	// feature. In the header rather than inline for the reason above: the control
	// plane cannot know them, and a command carrying a placeholder looks pasteable
	// and is not.
	fmt.Fprintf(&b, "BEDROCK_KEY=<aws access key id>        # inference gateway, bedrock provider\n")
	fmt.Fprintf(&b, "BEDROCK_SECRET=<aws secret access key>\n")
	fmt.Fprintf(&b, "QUAY_USER=<robot name>         # e.g. yugabyte+something\n")
	fmt.Fprintf(&b, "QUAY_TOKEN=<robot token>\n")
	// The one langfuse field a chart cannot invent for itself: everything else
	// required below is real cryptographic material this shell can generate on
	// the spot (openssl rand), but the seeded owner needs an actual mailbox.
	fmt.Fprintf(&b, "LANGFUSE_OWNER_EMAIL=<you@your-domain>        # bundled langfuse's seeded owner login\n")
	// Worth a line of its own rather than a footnote: without it the stack comes up
	// entirely green and remembers nothing, which reads as a bug rather than a
	// missing credential. The refs are optional, so empty installs fine.
	fmt.Fprintf(&b, "OPENAI_KEY=                    # no key, no memory extraction and no RAG\n")
	// The extension point. values-byoc.yaml leaves the rag workers, langfuse and
	// the provisioner off; an overlay passed after it is what turns any of them
	// back on, and helm's last-wins ordering is the whole mechanism. Empty by
	// default so the paste is unchanged for an operator who wants the base stack.
	fmt.Fprintf(&b, "EXTRA_VALUES=                  # overlays applied after values-byoc.yaml, e.g.\n")
	fmt.Fprintf(&b, "#   EXTRA_VALUES=\"-f %s/values-byoc-full.yaml\"\n", chart)
	fmt.Fprintf(&b, "#   moves the inference gateway onto OpenAI and adds the rag workers;\n")
	fmt.Fprintf(&b, "#   needs OPENAI_KEY\n\n")

	fmt.Fprintf(&b, "# 1 — two credentials. The agent's API key, and a pull secret: every image\n")
	fmt.Fprintf(&b, "#      in this release is on private Quay, and %s is the\n", mekoPullSecretName)
	fmt.Fprintf(&b, "#      name values-byoc.yaml expects.\n")
	fmt.Fprintf(&b, "kubectl create namespace \"${NS}\" --dry-run=client -o yaml | kubectl apply -f -\n")
	fmt.Fprintf(&b, "kubectl -n \"${NS}\" create secret generic meko-agent-api-key \\\n")
	fmt.Fprintf(&b, "  --from-literal=api-key=\"${AGENT_API_KEY}\" \\\n")
	fmt.Fprintf(&b, "  --dry-run=client -o yaml | kubectl apply -f -\n")
	fmt.Fprintf(&b, "kubectl -n \"${NS}\" create secret docker-registry %s \\\n", mekoPullSecretName)
	fmt.Fprintf(&b, "  --docker-server=quay.io --docker-username=\"${QUAY_USER}\" --docker-password=\"${QUAY_TOKEN}\" \\\n")
	fmt.Fprintf(&b, "  --dry-run=client -o yaml | kubectl apply -f -\n\n")

	fmt.Fprintf(&b, "# 2 — install the stack and the reconcile-agent, from a meko checkout. Swap\n")
	fmt.Fprintf(&b, "#      values-aws.yaml for values-gcp.yaml, and values-dev.yaml for\n")
	fmt.Fprintf(&b, "#      values-prod.yaml, to match the cluster. values-byoc.yaml goes LAST\n")
	fmt.Fprintf(&b, "#      except for EXTRA_VALUES: it is what makes this one self-contained\n")
	fmt.Fprintf(&b, "#      namespace, and anything after it is deliberately overriding that.\n")
	fmt.Fprintf(&b, "helm upgrade --install meko %s -n \"${NS}\" \\\n", chart)
	fmt.Fprintf(&b, "  -f %s/values-aws.yaml -f %s/values-dev.yaml \\\n", chart, chart)
	fmt.Fprintf(&b, "  -f %s/values-byoc.yaml ${EXTRA_VALUES} \\\n", chart)
	fmt.Fprintf(&b, "  --set-string dataPlaneSecrets.openaiApiKey=\"${OPENAI_KEY}\" \\\n")
	fmt.Fprintf(&b, "  --set-string apiServer.image.tag=\"${API_TAG}\" \\\n")
	fmt.Fprintf(&b, "  --set-string mekoMcpServer.image.tag=\"${MCP_TAG}\" \\\n")
	fmt.Fprintf(&b, "  --set-string mekoUi.image.tag=\"${UI_TAG}\" \\\n")
	fmt.Fprintf(&b, "  --set-string inferenceGateway.image.tag=\"${GW_TAG}\" \\\n")
	fmt.Fprintf(&b, "  --set dataPlaneSecrets.awsAccessKeyId=\"${BEDROCK_KEY}\" \\\n")
	fmt.Fprintf(&b, "  --set dataPlaneSecrets.awsSecretAccessKey=\"${BEDROCK_SECRET}\" \\\n")
	// values-byoc.yaml turns langfuse on with migrate.enabled — a fresh install
	// fails at dataplane-secrets.yaml's own fail-fast guard without these seven.
	// Six are generated inline (openssl rand, once, at paste time — see the
	// README for why these must NOT be regenerated on a later upgrade); the
	// seventh is the header var above, since a chart cannot invent an email.
	fmt.Fprintf(&b, "  --set-string dataPlaneSecrets.langfuseNextauthSecret=\"$(openssl rand -hex 16)\" \\\n")
	fmt.Fprintf(&b, "  --set-string dataPlaneSecrets.langfuseSalt=\"$(openssl rand -hex 16)\" \\\n")
	fmt.Fprintf(&b, "  --set-string dataPlaneSecrets.langfuseEncryptionKey=\"$(openssl rand -hex 32)\" \\\n")
	fmt.Fprintf(&b, "  --set-string dataPlaneSecrets.langfuseAdminApiKey=\"$(openssl rand -hex 16)\" \\\n")
	fmt.Fprintf(&b, "  --set-string dataPlaneSecrets.langfuseInitUserEmail=\"${LANGFUSE_OWNER_EMAIL}\" \\\n")
	fmt.Fprintf(&b, "  --set-string dataPlaneSecrets.langfuseInitUserPassword=\"$(openssl rand -hex 16)\" \\\n")
	fmt.Fprintf(&b, "  --set-string dataPlaneSecrets.langfuseInitProjectSecretKey=\"$(openssl rand -hex 16)\" \\\n")
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
	fmt.Fprintf(&b, "# anything desired state stops declaring is collected. On an api-server with an\n")
	fmt.Fprintf(&b, "# identity provider ON (Cognito or OIDC), add the account's MCP token, or every\n")
	fmt.Fprintf(&b, "# call the agent makes 401s:\n")
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
// Everything the control plane can know is filled in. What is left are the values
// it cannot — the YugabyteDB endpoint, the image tag, the pull secret, the two
// telemetry credentials, and the namespace if the default does not suit — and those
// are lifted into a header the operator edits once, rather than left as
// placeholders inside a command that looks pasteable and is not. The chart's own
// defaults are not usable as-is: the images resolve to docker.io, the telemetry
// sinks are in-cluster stores this cluster does not run, and the UI and an internal
// LoadBalancer would both be deployed for nobody.
//
// byocMetricsURL is deliberately NOT in that header. It is the door this control
// plane minted credentials for, so it is not an install-time choice: it is printed
// inline, and an agent reporting anything else has its environment marked DEGRADED.
// Empty (a control plane with no door configured) prints the telemetry lines it can
// and leaves the endpoint to the chart's default.
func ampInstallHint(ycpURL, envID, placement, accountID, byocMetricsURL string) string {
	ns := namespaceFor(envID, ampInstallNamespace)

	var b strings.Builder
	fmt.Fprintf(&b, "# Prerequisites\n")
	fmt.Fprintf(&b, "#   - YugabyteDB up and reachable from this cluster, with a yb_storage database:\n")
	fmt.Fprintf(&b, "#       CREATE DATABASE yb_storage;\n")
	fmt.Fprintf(&b, "#   - a docker-registry Secret for quay.io/yugabyte, in the namespace below\n")
	fmt.Fprintf(&b, "#   - helm logged in to quay.io with that Secret's credential — the chart is\n")
	fmt.Fprintf(&b, "#     private, like the images:\n")
	fmt.Fprintf(&b, "#       helm registry login quay.io\n")
	fmt.Fprintf(&b, "#   - the OpenTelemetry Operator, with the contrib collector distro. The\n")
	fmt.Fprintf(&b, "#     collectors are OpenTelemetryCollector CRs, so nothing ships without it:\n")
	fmt.Fprintf(&b, "#       helm repo add open-telemetry https://open-telemetry.github.io/opentelemetry-helm-charts\n")
	fmt.Fprintf(&b, "#       helm upgrade --install opentelemetry-operator open-telemetry/opentelemetry-operator \\\n")
	fmt.Fprintf(&b, "#         -n opentelemetry-operator-system --create-namespace \\\n")
	fmt.Fprintf(&b, "#         --set admissionWebhooks.certManager.enabled=false \\\n")
	fmt.Fprintf(&b, "#         --set admissionWebhooks.autoGenerateCert.enabled=true \\\n")
	fmt.Fprintf(&b, "#         --set manager.collectorImage.repository=otel/opentelemetry-collector-contrib\n\n")

	fmt.Fprintf(&b, "# Fill these in, then paste the rest as-is.\n")
	fmt.Fprintf(&b, "NS=%s\n", ns)
	fmt.Fprintf(&b, "YB=<yb-tserver-host>:5433      # YugabyteDB tserver endpoint (host:port)\n")
	// Empty by default: the foundation's universe runs with YSQL auth off.
	fmt.Fprintf(&b, "YB_PASSWORD=''                 # the yugabyte user's YSQL password, if YSQL auth is on\n")
	fmt.Fprintf(&b, "AGENT_API_KEY='%s'   # shown once by the YCP console; revocable there\n", apiKeyPlaceholder)
	fmt.Fprintf(&b, "TAG=<amp image tag>            # quay.io/yugabyte/amp-* release tag\n")
	fmt.Fprintf(&b, "PULL_SECRET=<secret name>      # docker-registry Secret for quay.io/yugabyte\n")
	fmt.Fprintf(&b, "BYOC_PASSWORD='<byoc password>'  # BasicAuth password for the metrics door\n")
	fmt.Fprintf(&b, "DD_API_KEY='<datadog api key>'   # logs fan-out to Datadog\n")
	fmt.Fprintf(&b, "POD_METRICS=true               # per-pod CPU/mem; needs a cluster-scoped ClusterRole\n")
	fmt.Fprintf(&b, "                               # for node metrics — false to stay namespace-scoped\n\n")

	fmt.Fprintf(&b, "# 1 — hand the cluster the agent's API key, which the reconcile-agent reads,\n")
	fmt.Fprintf(&b, "#      and the YSQL password, if there is one, which the controller and every\n")
	fmt.Fprintf(&b, "#      endpoint read.\n")
	fmt.Fprintf(&b, "kubectl create namespace \"${NS}\" --dry-run=client -o yaml | kubectl apply -f -\n")
	fmt.Fprintf(&b, "kubectl -n \"${NS}\" create secret generic %s \\\n", ampAPIKeySecretName)
	fmt.Fprintf(&b, "  --from-literal=api-key=\"${AGENT_API_KEY}\" \\\n")
	fmt.Fprintf(&b, "  --dry-run=client -o yaml | kubectl apply -f -\n")
	fmt.Fprintf(&b, "if [ -n \"${YB_PASSWORD}\" ]; then\n")
	fmt.Fprintf(&b, "  kubectl -n \"${NS}\" create secret generic %s \\\n", ybPasswordSecretName)
	fmt.Fprintf(&b, "    --from-literal=password=\"${YB_PASSWORD}\" \\\n")
	fmt.Fprintf(&b, "    --dry-run=client -o yaml | kubectl apply -f -\n")
	fmt.Fprintf(&b, "fi\n\n")

	fmt.Fprintf(&b, "# 2 — hand the cluster the two telemetry credentials. Both must exist BEFORE\n")
	fmt.Fprintf(&b, "#      the install: the collectors read them by secretKeyRef, and a missing one\n")
	fmt.Fprintf(&b, "#      fails that collector to start.\n")
	fmt.Fprintf(&b, "kubectl -n \"${NS}\" create secret generic %s \\\n", byocMetricsSecretName)
	fmt.Fprintf(&b, "  --from-literal=password=\"${BYOC_PASSWORD}\" \\\n")
	fmt.Fprintf(&b, "  --dry-run=client -o yaml | kubectl apply -f -\n")
	fmt.Fprintf(&b, "kubectl -n \"${NS}\" create secret generic %s \\\n", datadogSecretName)
	fmt.Fprintf(&b, "  --from-literal=api-key=\"${DD_API_KEY}\" \\\n")
	fmt.Fprintf(&b, "  --dry-run=client -o yaml | kubectl apply -f -\n\n")

	// Numbered, not fixed: with no door configured there is nowhere for the
	// database's metrics to go, so that step is absent and the install is 3.
	// A printed sequence that skips a number reads as a missing step.
	installStep := 3
	if byocMetricsURL != "" {
		ybMetricsExportHint(&b, envID, accountID, byocMetricsURL)
		installStep = 4
	}

	fmt.Fprintf(&b, "# %d — install the amp controller and its reconcile-agent.\n", installStep)
	fmt.Fprintf(&b, "#      The agent is the sole YCP client; the controller only serves its REST API.\n")
	fmt.Fprintf(&b, "helm upgrade --install yb-amp-controller %s -n \"${NS}\" \\\n", ampControllerChart)
	// Above the preflight hook's own budget. helm waits on a post-install hook for
	// --timeout, which defaults to exactly 5m — the same as the preflight's default
	// deadline — so without this helm abandons the release first and reports a
	// timeout instead of the probe's verdict, and an upgrade is left failed for a
	// check that was still running.
	fmt.Fprintf(&b, "  --timeout 10m \\\n")
	fmt.Fprintf(&b, "  --set-string imageTag=\"${TAG}\" \\\n")
	// --set-string, not --set: the DSN's ? and = are mis-tokenized by --set.
	fmt.Fprintf(&b, "  --set-string controller.databaseUrl=\"postgres://yugabyte@${YB}/yb_storage?sslmode=disable\" \\\n")
	// Names the Secret only when step 1 created one; empty leaves the controller
	// and its endpoints passwordless.
	fmt.Fprintf(&b, "  --set-string controller.ybPasswordSecret.name=\"${YB_PASSWORD:+%s}\" \\\n", ybPasswordSecretName)
	fmt.Fprintf(&b, "  --set-string controller.endpoint.ybHostPorts=\"${YB}\" \\\n")
	fmt.Fprintf(&b, "  --set \"imagePullSecrets[0].name=${PULL_SECRET}\" \\\n")
	fmt.Fprintf(&b, "  --set controller.service.type=none \\\n")
	fmt.Fprintf(&b, "  --set ui.enabled=false \\\n")
	fmt.Fprintf(&b, "  --set otel.enabled=true \\\n")
	fmt.Fprintf(&b, "  --set otel.byoc.enabled=true \\\n")
	if byocMetricsURL != "" {
		fmt.Fprintf(&b, "  --set-string otel.byoc.metricsEndpoint=%s \\\n", byocMetricsURL)
	}
	// Without this the collectors would carry permanently failing exporters aimed
	// at a VictoriaMetrics and a VictoriaLogs no BYOC cluster runs.
	fmt.Fprintf(&b, "  --set otel.inClusterStores.enabled=false \\\n")
	fmt.Fprintf(&b, "  --set otel.podMetrics=\"${POD_METRICS}\" \\\n")
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
	fmt.Fprintf(&b, "# Metrics ship to yugabyte-cloud over the door above; logs ship to Datadog\n")
	fmt.Fprintf(&b, "# (us3), with otel.byoc.enabled and the datadog-api Secret. inClusterStores\n")
	fmt.Fprintf(&b, "# is off because a BYOC cluster runs no in-cluster metrics or log store.\n")
	fmt.Fprintf(&b, "# A post-install job checks the metrics door and waits for the collectors to\n")
	fmt.Fprintf(&b, "# export through it. It does NOT fail the install, so a successful install does\n")
	fmt.Fprintf(&b, "# not mean metrics are flowing — its log is the verdict:\n")
	fmt.Fprintf(&b, "#   kubectl -n \"${NS}\" logs job/yb-amp-controller-telemetry-preflight\n")
	fmt.Fprintf(&b, "# The metrics endpoint is not an install-time choice: this console checks it at\n")
	fmt.Fprintf(&b, "# enroll, and shows an environment that ships elsewhere as DEGRADED.\n")
	fmt.Fprintf(&b, "#\n")
	fmt.Fprintf(&b, "# Already running an amp controller in this namespace? Attach the agent to that\n")
	fmt.Fprintf(&b, "# release instead, keeping the values it does not name here:\n")
	fmt.Fprintf(&b, "#   helm upgrade yb-amp-controller %s \\\n", ampControllerChart)
	fmt.Fprintf(&b, "#     -n \"${NS}\" --reuse-values \\\n")
	fmt.Fprintf(&b, "#     --set agent.enabled=true --set agent.ycpUrl=%s \\\n", ycpURL)
	fmt.Fprintf(&b, "#     --set agent.envName=%s ", envID)
	if placement != "" {
		fmt.Fprintf(&b, "--set agent.placement=%s ", placement)
	}
	fmt.Fprintf(&b, "\\\n")
	// --reuse-values replays the old release's values and ignores new chart
	// defaults, so every telemetry value has to be repeated here: without them the
	// re-attached release keeps shipping nowhere and the environment comes up
	// DEGRADED.
	fmt.Fprintf(&b, "#     --set otel.enabled=true --set otel.byoc.enabled=true \\\n")
	if byocMetricsURL != "" {
		fmt.Fprintf(&b, "#     --set-string otel.byoc.metricsEndpoint=%s \\\n", byocMetricsURL)
	}
	fmt.Fprintf(&b, "#     --set otel.inClusterStores.enabled=false \\\n")
	// The upgrade pulls the newest chart, so the release's old image tags would be
	// replayed against it. Per image, not just imageTag: that is only the fallback,
	// and an existing release (up-gke.sh, redeploy.sh) pins these three, which
	// --reuse-values keeps — the same keys the reconcile-agent's upgrade sets.
	fmt.Fprintf(&b, "#     --set-string imageTag=\"${TAG}\" --set-string controller.image.tag=\"${TAG}\" \\\n")
	fmt.Fprintf(&b, "#     --set-string agent.image.tag=\"${TAG}\" \\\n")
	fmt.Fprintf(&b, "#     --set-string controller.endpoint.image.tag=\"${TAG}\" \\\n")
	fmt.Fprintf(&b, "#     --set otel.podMetrics=\"${POD_METRICS}\" \\\n")
	// Step 1 created it; a release that predates the value has passwordless
	// endpoints, which a YSQL-auth-on universe refuses.
	fmt.Fprintf(&b, "#     --set-string controller.ybPasswordSecret.name=\"${YB_PASSWORD:+%s}\" \\\n",
		ybPasswordSecretName)
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

// ybMetricsExportHint prints the step that turns YugabyteDB metrics on.
//
// The amp chart's collectors cover amp's own components; the database underneath
// them is YBA's to export, declared on the CRs the foundation renders (meko#1405).
// So this step is YAML to add to a spec, not a command to run: the spec's path,
// kube context and chart source belong to the operator, and naming an invocation
// here would couple this control plane to a CLI surface it does not own.
//
// It is printed AFTER the Secret step and BEFORE the install, and that order is
// the whole reason this lives in the hint at all. The exporter references a
// Secret that only exists once step 2 has run, and the identity tags are not
// known anywhere earlier — so a foundation comes up with no telemetry, and this
// is the first moment it can be turned on correctly. An empty exporter list is
// not "collect but ship nowhere": YBA gates the collector sidecar on there being
// an exporter, so nothing is collected until this is applied, and no series can
// reach the door without the identity on it.
func ybMetricsExportHint(b *strings.Builder, envID, accountID, byocMetricsURL string) {
	fmt.Fprintf(b, "# 3 — turn on YugabyteDB metrics, so the database under this environment\n")
	fmt.Fprintf(b, "#      reports too. FOR AN OPERATOR-MANAGED FOUNDATION: these are CRs that\n")
	fmt.Fprintf(b, "#      YugabyteDB Anywhere reads, so they apply only where a foundation\n")
	fmt.Fprintf(b, "#      runs one. Until then the universe collects nothing.\n")
	fmt.Fprintf(b, "#\n")
	// The CRs live in the foundation's namespace and the provider reads its
	// Secret there — which need not be ${NS}, so step 2's copy does not count.
	fmt.Fprintf(b, "#      First, the provider's password, in the FOUNDATION's namespace (step 2\n")
	fmt.Fprintf(b, "#      created it only in ${NS}, which may be a different one):\n")
	fmt.Fprintf(b, "#        kubectl -n <foundation namespace> create secret generic %s \\\n", byocMetricsSecretName)
	fmt.Fprintf(b, "#          --from-literal=password=\"${BYOC_PASSWORD}\" \\\n")
	fmt.Fprintf(b, "#          --dry-run=client -o yaml | kubectl apply -f -\n")
	fmt.Fprintf(b, "#\n")
	fmt.Fprintf(b, "#      Then three edits to the foundation spec, then apply it. Its keys apply\n")
	fmt.Fprintf(b, "#      top to bottom, so (b) goes ABOVE the YBUniverse key.\n")
	fmt.Fprintf(b, "#\n")
	// The gate defaults OFF and YBA reads its runtime config at startup, so a
	// spec that adds it later gets no collector and no error at all. Printed
	// first because it is the one thing here that cannot be fixed afterwards
	// without reinstalling yugaware.
	fmt.Fprintf(b, "#      (a) YBA's collector gate, under the existing yba: key. It defaults off\n")
	fmt.Fprintf(b, "#      and is read at yugaware STARTUP — it must be in the spec that installs\n")
	fmt.Fprintf(b, "#      YBA, or nothing below produces a collector and nothing reports why:\n")
	fmt.Fprintf(b, "#\n")
	fmt.Fprintf(b, "#      yba:\n")
	fmt.Fprintf(b, "#        values:\n")
	fmt.Fprintf(b, "#          additionalAppConf:\n")
	fmt.Fprintf(b, "#            nonStringConf: {\"yb.universe.otel_collector_enabled\": true}\n")
	fmt.Fprintf(b, "#\n")
	fmt.Fprintf(b, "#      (b) a new top-level key, above YBUniverse:\n")
	fmt.Fprintf(b, "#\n")
	fmt.Fprintf(b, "#      TelemetryProvider:\n")
	fmt.Fprintf(b, "#        name: %s\n", ybTelemetryProviderName)
	fmt.Fprintf(b, "#        spec:\n")
	fmt.Fprintf(b, "#          provider: OTLP\n")
	fmt.Fprintf(b, "#          otlp:\n")
	fmt.Fprintf(b, "#            endpoint: %s\n", strings.TrimSuffix(byocMetricsURL, "/metrics"))
	fmt.Fprintf(b, "#            protocol: HTTP\n")
	fmt.Fprintf(b, "#            metricsEndpoint: %s\n", byocMetricsURL)
	// Explicit, though gzip is the CRD's default: the door returns 415 to an
	// uncompressed payload, so a changed default would fail every send with
	// nothing but the collector's own counters to say so.
	fmt.Fprintf(b, "#            compression: gzip\n")
	fmt.Fprintf(b, "#            authType: BasicAuth\n")
	fmt.Fprintf(b, "#            basicAuth:\n")
	fmt.Fprintf(b, "#              username: %s\n", byocMetricsUsername)
	fmt.Fprintf(b, "#              passwordSecret: {name: %s, key: password}\n", byocMetricsSecretName)
	fmt.Fprintf(b, "#\n")
	fmt.Fprintf(b, "#      (c) inside the YBUniverse entry's spec:\n")
	fmt.Fprintf(b, "#\n")
	fmt.Fprintf(b, "#      telemetry:\n")
	fmt.Fprintf(b, "#        metrics:\n")
	// MINIMAL, and deliberately not TABLE_OFF, whose name is the one that says
	// what is wanted: YBA's metrics export sends only priority_regex, metrics and
	// show_help, and TABLE_OFF's params file defines neither of the first two, so
	// it reaches the database as no filter at all and emits every per-table
	// series. MINIMAL defines both and takes effect.
	fmt.Fprintf(b, "#          collectionLevel: MINIMAL\n")
	fmt.Fprintf(b, "#          scrapeConfigTargets: [MASTER_EXPORT, TSERVER_EXPORT, YSQL_EXPORT]\n")
	fmt.Fprintf(b, "#          exporters:\n")
	fmt.Fprintf(b, "#            - telemetryProvider: %s\n", ybTelemetryProviderName)
	fmt.Fprintf(b, "#              additionalTags:\n")
	fmt.Fprintf(b, "#                source: amp\n")
	fmt.Fprintf(b, "#                environment_id: %s\n", envID)
	// Same rule as the chart's identity args: the standard YCP account is a
	// sentinel that would label every series identically, so it is omitted.
	if account := service.MetricsAccountID(accountID); account != "" {
		fmt.Fprintf(b, "#                account_id: %s\n", account)
	}
	fmt.Fprintf(b, "\n")
}
