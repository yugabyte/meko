package models

import (
	"encoding/json"
	"time"
)

// Environment is one registered customer data-plane environment (YCP fleet
// registry, control-plane mode). Metadata only -- never customer data.
type Environment struct {
	CreatedAt       time.Time  `json:"created_at"`
	LastHeartbeatAt *time.Time `json:"last_heartbeat_at,omitempty"`
	// UUID is the row's identity and the key everything internal joins on. ID is
	// what an operator and an agent use, so it stays in the API unchanged; this is
	// what survives when two accounts pick the same one.
	UUID string `json:"uuid"`
	// ID is slug(name): the id in the API path and in the install hint's
	// --set agent.envName=. Unique per account, not globally.
	ID        string `json:"id"`
	AccountID string `json:"account_id,omitempty"`
	// APIKeyID is the id of the YBM API key this environment's agent authenticates
	// with. An id, not the key itself -- YBM holds the credential -- so serializing
	// it leaks nothing.
	APIKeyID string `json:"api_key_id"`
	// Kind distinguishes an amp environment (the amp controller) from a meko one
	// (the datapack/stack agent). It is fixed at registration, and it is the only
	// thing that says what an environment serves: the id is a bare slug(name) and
	// encodes nothing. Ids minted before that carry an "amp-" prefix and still
	// resolve. Legacy rows registered before this field read as "meko".
	Kind string `json:"kind"`
	// Type distinguishes a BYOC environment (the customer runs the data plane in
	// their own cloud) from a MANAGED one. Fixed at registration, like Kind, and
	// stored without behaviour attached yet. Legacy rows read as "BYOC" — YCP is
	// the BYOC control plane, so every environment predating this is one.
	Type            string          `json:"type"`
	Placement       string          `json:"placement"`
	Region          string          `json:"region"`
	State           string          `json:"state"`
	K8sVersion      string          `json:"k8s_version,omitempty"`
	Provider        string          `json:"provider,omitempty"`
	AgentVersion    string          `json:"agent_version,omitempty"`
	MCPBaseURL      string          `json:"mcp_base_url,omitempty"`
	CertFingerprint string          `json:"cert_fingerprint,omitempty"`
	Components      json.RawMessage `json:"components,omitempty"`
	// Resources is the amp controller's reported inventory (storage pools,
	// databases, branches, compute endpoints) — a JSON array of ResourceStatus,
	// stored verbatim from the status heartbeat for the console to render. Empty
	// ("[]") for a meko environment, which reports no inventory.
	Resources    json.RawMessage `json:"resources,omitempty"`
	NodeCount    int             `json:"node_count,omitempty"`
	MaxDatapacks int             `json:"max_datapacks,omitempty"`
	Live         bool            `json:"live"`

	// AgentBaseURL is the YBM pass-through base this environment's agent dials, as
	// the registration gave it. Stored only to render the install hint again later;
	// not serialized, being YBM's address for itself.
	AgentBaseURL string `json:"-"`
	// InstallHint is the rendered attach-the-agent commands, for the operator who did
	// not run them at registration. Computed by the handler — never stored, never
	// scanned — and set on the get-one response only: a fleet list would carry a
	// screenful of shell per row.
	InstallHint string `json:"install_hint,omitempty"`

	// Datapacks is no longer reported. Nothing can compute it: YCP stopped modelling
	// datapacks, so the agent does not count them and ApplyStatus has nothing to set it
	// from — it would be 0 for every environment registered from here on, and whatever
	// the last pre-change status report left for the rest. A field that is always 0 is
	// worse than an absent one, so it stops being serialized.
	//
	// The column stays for now, defaulted to 0 by 000001. Dropping it means renumbering
	// a shipped twenty-parameter upsert, where an off-by-one is silent data corruption,
	// and that is worth its own change rather than riding along here.
	Datapacks int `json:"-"`
}

// DesiredResource is one typed item of an environment's desired state. Kind-agnostic
// so new resource types need no schema change; the agent dispatches by Kind.
type DesiredResource struct {
	Kind string          `json:"kind"`
	Name string          `json:"name"`
	Spec json.RawMessage `json:"spec,omitempty"`
	// Generation is bumped by the control plane whenever Spec changes, so the
	// reconcile-agent can tell an unchanged resource (skip) from a mutated one
	// (re-apply). Starts at 0 (see ReplaceDesired: the amp backend's cache-cold
	// NoOp depends on it); maintained by the repository's desired-state writes.
	Generation int64 `json:"generation,omitempty"`
}

// ResourcePatch is one targeted change to an environment's desired state, the unit
// of a batch PATCH. It names one resource by Kind+Name and carries at most one intent
// flag; the four intents are mutually exclusive:
//
//   - none — a partial Spec merge: only the top-level keys present are changed, an
//     absent resource is created with the patch as its whole spec. (The region PATCH
//     narrows this: see the create-only default below.)
//   - Create — assert the resource does NOT exist yet; a collision is rejected.
//   - Update — assert the resource DOES exist; an absent one is rejected.
//   - Delete — a cascade removal of the resource and every descendant. Delete and a
//     non-empty Spec are mutually exclusive.
//
// The region storage-pool PATCH normalises a flagless non-delete item to Create (so a
// write cannot silently overwrite an existing resource on a shared MANAGED pool) and
// enforces cluster short-name uniqueness per pool across regions; that policy lives in
// the service/repository, not on this type. Lives here beside DesiredResource so
// handler, service and repository can all name it without a layering cycle.
type ResourcePatch struct {
	Kind   string          `json:"kind"`
	Name   string          `json:"name"`
	Spec   json.RawMessage `json:"spec,omitempty"`
	Delete bool            `json:"delete,omitempty"`
	Create bool            `json:"create,omitempty"`
	Update bool            `json:"update,omitempty"`
}

// RegionResources is one region's slice of a storage-pool region PATCH: the region
// whose MANAGED environment the patches target, and the batch of desired-state
// changes to apply there. A single PATCH carries one per region and applies them all
// in one atomic transaction, so a bad item in any region rolls the whole call back.
type RegionResources struct {
	Region    string          `json:"region"`
	Resources []ResourcePatch `json:"resources"`
}

// AeonAccountMapping is one sticky assignment pinning a storage pool, in one region,
// to the MANAGED environment that serves it. It is what makes the region-addressed
// resource APIs deterministic: the first PATCH for a (storage_pool, region) picks a
// READY MANAGED environment and records it here, and every later call for that pair
// reuses the same environment instead of re-choosing. Unique on
// (storage_pool_name, region); environment_id references environments(uuid).
type AeonAccountMapping struct {
	ID              string    `json:"id"`
	EnvironmentUUID string    `json:"environment_id"`
	Region          string    `json:"region"`
	StoragePoolName string    `json:"storage_pool_name"`
	CreatedOn       time.Time `json:"created_on"`
	UpdatedOn       time.Time `json:"updated_on"`
}

// EnvironmentTask is one amp controller task as the control plane stores it: the
// history behind an environment's inventory, so the console can say what is running
// and what failed. Kind-agnostic -- Kind and Result are stored and served without
// interpretation, so a controller that adds a task kind needs no change here.
type EnvironmentTask struct {
	TaskID    string `json:"task_id"`
	Kind      string `json:"kind"`
	Status    string `json:"status"` // pending | running | succeeded | failed
	ClusterID string `json:"cluster_id,omitempty"`
	TargetRef string `json:"target_ref,omitempty"`
	// Error is the controller's own failure reason, set when Status is failed. It is
	// what the console shows instead of an operator reading agent logs in a cluster
	// they may not have access to.
	Error  string          `json:"error,omitempty"`
	Result json.RawMessage `json:"result,omitempty"`
	// YCPGeneration is nil for a task an operator or the amp UI planned directly
	// rather than YCP driving it.
	YCPGeneration *int64 `json:"ycp_generation,omitempty"`
	// CreatedAt/UpdatedAt are the CONTROLLER's timestamps; ReportedAt is when the
	// agent last relayed this row. The pair is what lets a console distinguish a task
	// that has not moved from an agent that has stopped reporting -- see also the
	// environment's last_heartbeat_at.
	CreatedAt  time.Time `json:"created_at"`
	UpdatedAt  time.Time `json:"updated_at"`
	ReportedAt time.Time `json:"reported_at"`
}

// EnvironmentTaskStatuses is every status a task may hold, which is the amp
// controller's own enum. The handler whitelists a caller's status filter against it
// rather than passing the value through to SQL.
var EnvironmentTaskStatuses = []string{"pending", "running", "succeeded", "failed"}

// TerminalTaskStatuses are the statuses a task cannot leave. Retention prunes only
// these: a pending or running task is live state, however old its timestamp looks.
var TerminalTaskStatuses = []string{"succeeded", "failed"}

// Entitlement is the plan/limits envelope the console reads to decide what it may
// offer. Sourced from the billing system upstream; static for now.
type Entitlement struct {
	Plan            string `json:"plan"`
	Source          string `json:"source"`
	MaxEnvironments *int   `json:"max_environments"`
	MaxDatapacks    *int   `json:"max_datapacks"`
}

// EnvironmentExtension is one entry of an amp environment's extension catalog, as
// its reconcile agent relays it from the controller's GET /extensions.
type EnvironmentExtension struct {
	Name        string `json:"name"`
	Version     string `json:"version,omitempty"`
	Description string `json:"description,omitempty"`
	Builtin     bool   `json:"builtin"`
	// Preload means enabling it needs a compute restart (shared_preload_libraries).
	Preload bool `json:"preload"`
}
