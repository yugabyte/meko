// Package api holds the wire types shared by the YCP server and meko-agent.
// Field names/JSON tags mirror design/ycp-openapi.yaml.
package api

import (
	"encoding/json"
	"time"
)

// EnvState is a value from the §7 bootstrapping / lifecycle state machine.
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
	// fixed at registration. A meko id is a bare slug; an amp id is prefixed
	// "amp-". Empty legacy rows read as meko.
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

	// EnrollmentToken is set only on the create response (returned once, never persisted).
	EnrollmentToken string `json:"enrollment_token,omitempty"`
	// Persisted auth material (never serialized to clients). The enrollment token is
	// TTL-based and reusable until it expires (kubeadm-style bootstrap token), so an
	// agent restart can re-enroll within the window; the status token is the
	// long-lived env-scoped credential.
	EnrollTokenHash string     `json:"-"`
	EnrollExpiresAt *time.Time `json:"-"`
	StatusTokenHash string     `json:"-"`
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
}

// EnrollRequest is what the agent posts to /agent/enroll.
type EnrollRequest struct {
	Name            string       `json:"name"`
	EnrollmentToken string       `json:"enrollment_token,omitempty"`
	Placement       string       `json:"placement,omitempty"`
	Kind            string       `json:"kind,omitempty"` // amp | meko; checked against the environment's kind
	ClusterFacts    ClusterFacts `json:"cluster_facts"`
}

// EnrollResponse hands the agent its identity, its endpoints, and the stack to run.
type EnrollResponse struct {
	EnvironmentID string `json:"environment_id"`
	StatusToken   string `json:"status_token"`
	MCPBaseURL    string `json:"mcp_base_url"`
	// Stack is the meko-stack spec this environment runs, resolved by the control plane
	// when the environment was registered.
	//
	// It comes back with enrollment rather than being fetched, so bringing Meko up needs
	// no second call and no command: an agent that enrolls has, in that one response,
	// everything it needs to deploy the whole data plane. The agent persists it beside
	// its status token, which means a restart does not need the control plane either.
	//
	// json.RawMessage on purpose — the agent owns the shape of the spec (see
	// cmd/agent/mekostack.go), so a newer control plane can send richer fields to an
	// older agent, and this package does not have to know about either.
	Stack json.RawMessage `json:"stack,omitempty"`
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
	State  string `json:"state,omitempty"`
	// Error is why this resource is not converged, when the agent can say: the
	// controller's rejection of a create/update/delete, a resource stuck
	// non-Ready past the reconcile deadline, or drift the agent cannot fix
	// (a kind with no update/delete path). Empty when the resource is fine.
	// Without it a stuck resource reads as a bare state in the console and the
	// reason lives only in the agent's and controller's logs.
	Error string `json:"error,omitempty"`
	// Attrs are kind-specific details the backend observed, reported verbatim
	// for the console/API to render: every resource's backend id (an amp UUID,
	// distinct from the path in ID), a cluster's/branch's pg_version, a branch's
	// fork_lsn, extensions, parent_branch_id (pairs with a branch's id to rebuild
	// the fork tree), is_default, and an endpoint's role, address,
	// tailnet_address, cpu, memory. Empty when the backend has none.
	Attrs map[string]string `json:"attrs,omitempty"`
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
