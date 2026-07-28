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
	ID              string      `json:"id"`
	AccountID       string      `json:"account_id,omitempty"`
	Placement       string      `json:"placement"` // cloud | onprem
	Region          string      `json:"region"`
	State           EnvState    `json:"state"`
	K8sVersion      string      `json:"k8s_version,omitempty"`
	NodeCount       int         `json:"node_count,omitempty"`
	Provider        string      `json:"provider,omitempty"`
	Datapacks       int         `json:"datapacks"`
	MaxDatapacks    int         `json:"max_datapacks,omitempty"`
	AgentVersion    string      `json:"agent_version,omitempty"`
	LastHeartbeatAt *time.Time  `json:"last_heartbeat_at,omitempty"`
	MCPBaseURL      string      `json:"mcp_base_url,omitempty"`
	CertFingerprint string      `json:"cert_fingerprint,omitempty"`
	Components      []Component `json:"components,omitempty"`
	Live            bool        `json:"live"` // true = driven by a real agent
	CreatedAt       time.Time   `json:"created_at"`

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
	ClusterFacts    ClusterFacts `json:"cluster_facts"`
}

// EnrollResponse hands the agent its (mock) identity + endpoints.
type EnrollResponse struct {
	EnvironmentID string `json:"environment_id"`
	StatusToken   string `json:"status_token"`
	MCPBaseURL    string `json:"mcp_base_url"`
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
type DesiredResource struct {
	Kind string          `json:"kind"`
	Name string          `json:"name"`
	Spec json.RawMessage `json:"spec,omitempty"`
}

// DatapackPhase is a per-datapack status line in a StatusReport: what the agent
// actually provisioned for a declared datapack.
type DatapackPhase struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Phase    string `json:"phase"`              // Provisioning | Active | Error
	DB       string `json:"db,omitempty"`       // provisioned database name
	Endpoint string `json:"endpoint,omitempty"` // MCP endpoint (set once the server is up)
	Error    string `json:"error,omitempty"`
}

// StatusReport is the agent's heartbeat + phases (drives the fleet registry).
type StatusReport struct {
	EnvironmentState EnvState        `json:"environment_state"`
	AgentVersion     string          `json:"agent_version"`
	AppliedRevision  int             `json:"applied_revision"`
	ClusterFacts     ClusterFacts    `json:"cluster_facts"`
	Components       []Component     `json:"components"`
	DatapackPhases   []DatapackPhase `json:"datapack_phases"`
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
