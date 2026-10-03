// Package api holds the wire types shared by the YCP server and meko-agent.
// Field names/JSON tags mirror design/ycp-openapi.yaml.
package api

import (
	"encoding/json"
	"time"
)

// EnvState is a value from the §7 bootstrapping / lifecycle state machine.
//
// The control plane requires a state to be present but TOLERATES one it does not
// know (REVIEW.md: "a new value is safe only if the receiver tolerates unknown
// states") — an agent on the customer's upgrade schedule can be ahead of it. The
// list is still copied, deliberately, ycp and meko being separate Go modules that
// must not cross-import: into envStates in meko's api_server and the EnvState enum
// in meko/design/ycp-openapi.yaml. Adding a state means all three, and
// TestEnvStateListsAgreeAcrossTheRepo fails until it is.
type EnvState string

const (
	StatePending             EnvState = "PENDING"
	StateClusterProvisioning EnvState = "CLUSTER_PROVISIONING"
	StateEnrolling           EnvState = "ENROLLING"
	StateAttesting           EnvState = "ATTESTING"
	StateProvisioning        EnvState = "PROVISIONING"
	StateValidating          EnvState = "VALIDATING"
	StateReady               EnvState = "READY"
	StateDegraded            EnvState = "DEGRADED"
	StateSuspended           EnvState = "SUSPENDED"
	StateDraining            EnvState = "DRAINING"
	StateDeregistered        EnvState = "DEREGISTERED"
)

// Component is one Meko service's health in a data-plane cluster.
type Component struct {
	Name    string `json:"name"`
	Healthy int    `json:"healthy"`
	Total   int    `json:"total"`
	Version string `json:"version,omitempty"`
	// Error is why a declared component is not running, when the agent can say —
	// a spec it refused, or an apply the API server rejected. Without it a bad
	// command reads as 0/1 in the console and the reason lives only in agent logs.
	Error string `json:"error,omitempty"`
}

// ClusterFacts are the read-only facts the agent reports about its cluster.
type ClusterFacts struct {
	Provider   string `json:"provider,omitempty"`
	Region     string `json:"region,omitempty"`
	K8sVersion string `json:"k8s_version,omitempty"`
	NodeCount  int    `json:"node_count,omitempty"`
}

// Environment is one registered customer data plane.
type Environment struct {
	ID string `json:"id"`
	// Kind is "amp" (the amp controller) or "meko" (the datapack/stack agent),
	// fixed at registration. The id is a bare slug and does not encode the kind;
	// ids minted before that carry an "amp-" prefix. Empty legacy rows read as meko.
	Kind            string           `json:"kind,omitempty"`
	AccountID       string           `json:"account_id,omitempty"`
	Placement       string           `json:"placement"` // cloud | onprem
	Region          string           `json:"region"`
	State           EnvState         `json:"state"`
	K8sVersion      string           `json:"k8s_version,omitempty"`
	NodeCount       int              `json:"node_count,omitempty"`
	Provider        string           `json:"provider,omitempty"`
	Datapacks       int              `json:"datapacks"`
	MaxDatapacks    int              `json:"max_datapacks,omitempty"`
	AgentVersion    string           `json:"agent_version,omitempty"`
	LastHeartbeatAt *time.Time       `json:"last_heartbeat_at,omitempty"`
	MCPBaseURL      string           `json:"mcp_base_url,omitempty"`
	CertFingerprint string           `json:"cert_fingerprint,omitempty"`
	Components      []Component      `json:"components,omitempty"`
	Resources       []ResourceStatus `json:"resources,omitempty"` // amp controller's reported inventory
	Live            bool             `json:"live"`                // true = driven by a real agent
	CreatedAt       time.Time        `json:"created_at"`

	// APIKeyID is the id of the YBM API key this environment's agent authenticates
	// with -- an id, not the key itself, so it is not secret. Unique fleet-wide:
	// one key drives one environment.
	APIKeyID string `json:"api_key_id,omitempty"`

	// InstallHint is the rendered commands that attach an agent to this environment,
	// for a console showing them until the operator has run them. Present on get-one
	// only; the fleet list omits it.
	InstallHint string `json:"install_hint,omitempty"`
}

// ClusterSpec requests a brand-new cluster (provision mode, §7.1).
type ClusterSpec struct {
	Provider  string `json:"provider"`
	Region    string `json:"region"`
	NodeCount int    `json:"node_count,omitempty"`
	NodeSize  string `json:"node_size,omitempty"`
}

// EnvironmentCreate is the console's register-environment request.
type EnvironmentCreate struct {
	Name        string       `json:"name"`
	Kind        string       `json:"kind,omitempty"` // amp | meko (empty => meko)
	AccountID   string       `json:"account_id,omitempty"`
	Placement   string       `json:"placement"`
	Region      string       `json:"region"`
	ClusterSpec *ClusterSpec `json:"cluster_spec,omitempty"`
	// APIKeyID binds the environment to the YBM API key its agent will
	// authenticate with. Required.
	APIKeyID string `json:"api_key_id"`
	// AgentBaseURL, when set, is the YBM pass-through base the install hint points
	// the agent at; empty falls back to the address the register call arrived on.
	AgentBaseURL string `json:"agent_base_url,omitempty"`
}

// EnrollRequest is what the agent posts to /agent/enroll. Identity comes from the
// API key the call authenticated with, not the body.
type EnrollRequest struct {
	Name         string       `json:"name"`
	Placement    string       `json:"placement,omitempty"`
	Kind         string       `json:"kind,omitempty"` // amp | meko; checked against the environment's kind
	ClusterFacts ClusterFacts `json:"cluster_facts"`
	// Telemetry attests what this deployment is configured to ship. The control
	// plane checks it and degrades the environment on a mismatch — it does not
	// refuse the enrolment; see Telemetry.
	Telemetry *Telemetry `json:"telemetry,omitempty"`
}

// Telemetry is the agent's account of where this deployment's metrics and logs
// actually go, read from the env the chart set from the same values the collectors
// render from — so the agent cannot report something the collectors are not doing.
//
// It exists because the install runs from a checkout the customer has: values,
// templates and every rule inside the chart are theirs to edit, so nothing the
// chart enforces is actually enforced. The metrics endpoint in particular is not
// the customer's to choose — it is the door YCP minted credentials for. The agent
// is the one component in that cluster the control plane already trusts, so it is
// what attests, and the control plane is what decides.
//
// Sent on enroll AND on every status report: enroll alone would miss a later
// `helm upgrade` that turns telemetry off.
//
// A nil Telemetry means the agent is too old to report any. The control plane marks
// those DEGRADED, which is what it does with a mismatch too — nothing here refuses
// an enrolment, because an environment that cannot ship metrics is still one an
// operator needs to see.
type Telemetry struct {
	// OtelEnabled is otel.enabled: whether the collectors were rendered at all.
	OtelEnabled bool `json:"otel_enabled"`
	// ByocEnabled is otel.byoc.enabled: whether anything is shipped off-cluster.
	ByocEnabled bool `json:"byoc_enabled"`
	// MetricsEndpoint is otel.byoc.metricsEndpoint verbatim — the value compared
	// against the one the control plane printed in the install hint.
	MetricsEndpoint string `json:"metrics_endpoint,omitempty"`
	// LogsSink names where logs fan out ("datadog"), or is empty when they stay
	// in-cluster.
	LogsSink string `json:"logs_sink,omitempty"`

	// Everything below is RUNTIME, not configuration: what the collectors are
	// actually achieving, rather than what they were told to do. A deployment can
	// be configured perfectly and ship nothing — the door goes unreachable, the
	// password is rotated, PrivateLink breaks, the collector crashloops — and the
	// fields above cannot see any of it.
	//
	// The agent computes this itself, on purpose. Detecting "stopped shipping"
	// needs the PREVIOUS counter value, and the agent is the only party that
	// already polls on a loop; putting the history server-side would mean a column,
	// a migration, and state shared across api-server replicas to learn the same
	// thing.

	// MetricsSent is otelcol_exporter_sent_metric_points for the byoc exporter, as
	// last read from the controller collector's own telemetry. Reported for the
	// console, not for the staleness decision — that is StalledSeconds.
	MetricsSent float64 `json:"metrics_sent,omitempty"`
	// MetricsSendFailed is the matching send_failed counter. Diagnosis, not
	// detection: the exporter only counts a failure after its retry budget is
	// exhausted (300s by default), so this lags a broken door badly.
	MetricsSendFailed float64 `json:"metrics_send_failed,omitempty"`
	// MetricsQueued is the exporter's current queue depth. Also diagnosis: with
	// MetricsSent flat, an empty queue means nothing is being scraped (the fault is
	// upstream of the exporter) and a growing one means the door is not accepting
	// what we have (the fault is the door or the network).
	MetricsQueued float64 `json:"metrics_queued,omitempty"`
	// StalledSeconds is how long since MetricsSent last advanced, as the agent
	// observed it. THIS is the detection signal: the collectors scrape on a fixed
	// clock, so a healthy environment's counter climbs whether or not anyone is
	// using it, and flat means broken rather than quiet. Zero when it advanced on
	// this tick, or when the agent has no history yet (a fresh start).
	StalledSeconds int `json:"stalled_seconds,omitempty"`
	// ProbeError is why the agent could not read the collector's own telemetry at
	// all. Distinct from a stall: it says the agent cannot see, not that metrics
	// have stopped — an unreachable collector Service is itself worth surfacing,
	// but it must not be reported as a confirmed outage.
	ProbeError string `json:"probe_error,omitempty"`
}

// EnrollResponse hands the agent its identity, its endpoints, and the stack to run.
type EnrollResponse struct {
	EnvironmentID string `json:"environment_id"`
	// EnvironmentUUID is the environment's row key. The id above is a slug, unique
	// only within an account, so this is the identity to prefer where one is needed.
	EnvironmentUUID string `json:"environment_uuid"`
	MCPBaseURL      string `json:"mcp_base_url"`
}

// DesiredState is the generic, resource-typed set the agent reconciles toward.
// The protocol is transport-agnostic (currently HTTP/JSON long-poll); adding a
// new resource type needs only a new agent-side reconciler for that Kind, not a
// protocol change.
type DesiredState struct {
	Revision       int               `json:"revision"`
	TargetVersions map[string]string `json:"target_versions"`
	Resources      []DesiredResource `json:"resources"`
}

// DesiredResource is one declarative item of desired state.
//   - Kind: the resource type (e.g. "datapack"); dispatched to a reconciler.
//   - Name: the resource's stable name within the environment.
//   - Spec: kind-specific desired config (references secret NAMES only, never values).
//   - Generation: bumped by the control plane whenever Spec changes, so a
//     reconciler can tell an unchanged resource (skip) from a mutated one
//     (re-apply) by comparing it to the last generation it observed. Zero for a
//     control plane that does not stamp it (meko today), which a generation-diff
//     engine treats as "always newer than an unset observed generation" only on
//     first sight — see meko/ycp/reconcile.
type DesiredResource struct {
	Kind       string          `json:"kind"`
	Name       string          `json:"name"`
	Generation int64           `json:"generation,omitempty"`
	Spec       json.RawMessage `json:"spec,omitempty"`
}

// StatusReport is the agent's heartbeat + phases (drives the fleet registry).
type StatusReport struct {
	EnvironmentState EnvState         `json:"environment_state"`
	AgentVersion     string           `json:"agent_version"`
	AppliedRevision  int              `json:"applied_revision"`
	ClusterFacts     ClusterFacts     `json:"cluster_facts"`
	Components       []Component      `json:"components"`
	Resources        []ResourceStatus `json:"resources,omitempty"` // amp controller's inventory; meko omits it
	// Tasks is a DELTA of the amp controller's task log -- every task whose status
	// moved since the previous report -- not the whole log and not a snapshot. An
	// amp agent sends it; a meko agent omits it.
	//
	// Unlike Resources, an empty slice therefore does NOT mean "this environment
	// has no tasks": it means nothing changed this tick, which is the common case.
	// The control plane accumulates these deltas, so it is the side that holds the
	// history; see TaskStatus.
	Tasks []TaskStatus `json:"tasks,omitempty"`
	// Telemetry re-attests on every tick what enroll attested once, so a later
	// `helm upgrade` that turns telemetry off or repoints it shows up in the
	// console rather than as a gradually emptier dashboard.
	Telemetry *Telemetry `json:"telemetry,omitempty"`
}

// ResourceStatus is one item of an amp environment's reported inventory: a
// storage pool, database (project), branch, or compute endpoint. The amp
// controller reports its whole inventory on each status heartbeat; a meko agent
// omits it. Kind-agnostic and flat — Parent links a resource to its owner by id
// (project→pool, branch→project, endpoint→branch).
type ResourceStatus struct {
	Kind   string `json:"kind"`
	ID     string `json:"id"`
	Name   string `json:"name,omitempty"`
	Parent string `json:"parent,omitempty"`
	// State is the backend's own status for the resource ("ready",
	// "provisioning"), overridden by the agent when it has something the backend
	// cannot say. The overrides, and what each tells a reader to DO — the console
	// renders a different sentence for each, so they must not be conflated:
	//
	//   updating  an async task for this resource is in flight; wait.
	//   drift     no verb exists for this resource as declared — a kind with no
	//             update/delete path, or a field only create carries. Nothing the
	//             operator writes changes it; this is the "cannot be changed in
	//             place" case.
	//   rejected  this declaration cannot be applied but another could — the
	//             remote refused it (a 400) or the agent refused it before
	//             sending. Fixed by re-declaring, and Error says how.
	//   blocked   the agent is not permitted: credentials, a scope it lacks, or a
	//             quota no rewrite clears. Neither the spec nor the resource is
	//             wrong.
	//   error     a create/delete that failed for a resource the backend does not
	//             list, so there is no backend state to override.
	//
	// A reader that does not know a value must fall back to showing Error, never
	// to the immutability sentence: reporting a rejected endpoint as immutable is
	// CLOUDGA-36992.
	State string `json:"state,omitempty"`
	// Error is why this resource is not converged, when the agent can say: the
	// controller's rejection of a create/update/delete, the error a completed-but-
	// failed async task reported, a resource stuck non-Ready past the reconcile
	// deadline, or drift the agent cannot fix (a kind with no update/delete path).
	// Empty when the resource is fine. Without it a stuck resource reads as a bare
	// state in the console and the reason lives only in the agent's and
	// controller's logs.
	//
	// It also carries the detail behind a non-error State the agent sets, notably
	// "updating" — what the in-flight task is, so a generation that has not landed
	// yet is distinguishable from one that never will.
	Error string `json:"error,omitempty"`
	// Attrs are kind-specific details the backend observed, reported verbatim
	// for the console/API to render: every resource's backend id (an amp UUID,
	// distinct from the path in ID), a cluster's/branch's pg_version, a branch's
	// extensions (opted in per branch — seeded at create, inherited on fork, added
	// later; the default branch's set is also mirrored on the cluster as a
	// convenience view),
	// a branch's fork_lsn, parent_branch_id
	// (pairs with a branch's id to rebuild the fork tree), is_default, and an
	// endpoint's role, address, tailnet_address, cpu, memory,
	// connectivity_mode and connectivity_allowed_cidrs. A branch also reports its
	// pg_settings and write_token_settings, an endpoint its pg_settings,
	// pageserver_settings, safekeeper_settings and extended_config. Empty when the
	// backend has none.
	//
	// Each settings surface is one JSON OBJECT, keys sorted, written whole rather
	// than flattened per setting. Whole because that is the unit desired state
	// replaces: a declared surface owns the map, so an editor has to see what it
	// is about to overwrite or its first write silently drops every key it never
	// rendered. An empty or unset surface is "{}" — the controller's GET omits
	// both, so nothing distinguishes them, and both mean nothing is stored.
	// extended_config is reported although desired state cannot converge it:
	// it is declarable at endpoint create, so a spec that disagrees is permanent
	// drift, and this is the only way to see what it disagrees with.
	//
	// The same skew rule as connectivity below: an ABSENT settings key means the
	// agent predates the release that added these, never that the surface is
	// empty. Reading absence as empty is the destructive save they exist to
	// prevent.
	//
	// connectivity_mode is reported resolved: an endpoint predating the field
	// behaves as cluster_ip. connectivity_allowed_cidrs is present only under an
	// nlb_* mode, comma-joined like extensions, and an EMPTY value there means
	// unrestricted, not unset — so a consumer splitting on "," must treat the
	// empty string as the empty list rather than as one blank entry.
	//
	// Both keys are absent altogether from an agent older than the release that
	// added them, and the two sides are always skewed (the customer upgrades on
	// their own schedule). So a MISSING connectivity_mode means unknown, never
	// cluster_ip; only when it is present does a missing cidr key mean "no load
	// balancer to restrict". Reading absence as the default would silently drop
	// the warning for the internet-exposed case this encoding exists to surface.
	//
	// The controller singleton reports its own: its running version, the image_tag
	// its pod template carries, and target_version. The tag is the one compared
	// against a declared version — the running version is the binary's own stamp,
	// which is never an image tag.
	Attrs map[string]string `json:"attrs,omitempty"`
}

// TaskStatus is one amp controller task, relayed from its write-ahead task log so
// the console can answer "what is running" and "what failed" without reaching into
// the data plane. Kind-agnostic: YCP stores and serves these without interpreting
// Kind or Result, so a controller that adds a task kind needs no change here.
type TaskStatus struct {
	ID     string `json:"id"`
	Kind   string `json:"kind"`   // e.g. create_cluster, resize_endpoint
	Status string `json:"status"` // pending | running | succeeded | failed
	// ClusterID and TargetRef are the task's target, as the controller reports it.
	// TargetRef pairs with Kind to say which resource moved.
	ClusterID string `json:"cluster_id,omitempty"`
	TargetRef string `json:"target_ref,omitempty"`
	// Error is the controller's own failure reason, set when Status is failed. This
	// is the string the console shows instead of an operator reading agent logs in a
	// cluster they may not have access to.
	Error string `json:"error,omitempty"`
	// Result is the task's final step result, opaque JSON. For resize_endpoint it
	// carries in_place_resize / in_place_message; absent until a step records one.
	Result json.RawMessage `json:"result,omitempty"`
	// YCPGeneration is the desired-state generation this task applies, or nil for a
	// task an operator or the amp UI planned directly rather than YCP driving it.
	// Worth keeping: it is what distinguishes "the console did this" from "someone
	// went around the control plane".
	YCPGeneration *int64    `json:"ycp_generation,omitempty"`
	CreatedAt     time.Time `json:"created_at"`
	UpdatedAt     time.Time `json:"updated_at"`
}

// Params is deliberately absent from TaskStatus: it is the whole originating
// request, it would ride every heartbeat, and Kind + TargetRef + Result already
// answer what the console asks.

// Extension is one entry of the amp controller's extension catalog (its
// listExtensions): what a cluster in this environment can be created with, builtins
// and operator-registered images alike. Image, library and layout are left behind —
// they describe how the data plane mounts the extension, not what a console picks.
type Extension struct {
	Name    string `json:"name"`
	Builtin bool   `json:"builtin"`
	// Preload means enabling it needs a compute restart (shared_preload_libraries).
	Preload     bool   `json:"preload"`
	Version     string `json:"version,omitempty"`
	Description string `json:"description,omitempty"`
}

// ExtensionCatalog is the body of POST /agent/extensions: the environment's whole
// catalog, replacing what the control plane held. Extensions is never omitempty —
// an empty catalog is a reported fact, distinct from one never reported.
type ExtensionCatalog struct {
	Extensions []Extension `json:"extensions"`
}

// BootstrapProgress is the pre-agent installer's outbound report.
type BootstrapProgress struct {
	Phase   EnvState `json:"phase"`
	Step    string   `json:"step"`
	Message string   `json:"message"`
	Error   string   `json:"error,omitempty"`
}

// Entitlement reflects the plan resolved from YBM Aeon (read-only in YCP).
type Entitlement struct {
	Plan            string `json:"plan"`
	Source          string `json:"source"`
	MaxEnvironments *int   `json:"max_environments"`
	MaxDatapacks    *int   `json:"max_datapacks"`
}

// APIError is the error envelope.
type APIError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}
