package models

import (
	"encoding/json"
	"time"
)

// Environment is one registered customer data-plane environment (YCP fleet
// registry, control-plane mode). Metadata only -- never customer data.
type Environment struct {
	CreatedAt       time.Time       `json:"created_at"`
	LastHeartbeatAt *time.Time      `json:"last_heartbeat_at,omitempty"`
	ID              string          `json:"id"`
	AccountID       string          `json:"account_id,omitempty"`
	Placement       string          `json:"placement"`
	Region          string          `json:"region"`
	State           string          `json:"state"`
	K8sVersion      string          `json:"k8s_version,omitempty"`
	Provider        string          `json:"provider,omitempty"`
	AgentVersion    string          `json:"agent_version,omitempty"`
	MCPBaseURL      string          `json:"mcp_base_url,omitempty"`
	CertFingerprint string          `json:"cert_fingerprint,omitempty"`
	Components      json.RawMessage `json:"components,omitempty"`
	NodeCount       int             `json:"node_count,omitempty"`
	MaxDatapacks    int             `json:"max_datapacks,omitempty"`
	Live            bool            `json:"live"`

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

	// Auth material -- persisted hashed, never serialized to clients. The enrollment
	// token is TTL-based and reusable until it expires (kubeadm-style bootstrap
	// token), so an agent restart can re-enroll within the window.
	EnrollTokenHash      string     `json:"-"`
	EnrollTokenExpiresAt *time.Time `json:"-"`
	StatusTokenHash      string     `json:"-"`
}

// DesiredResource is one typed item of an environment's desired state. Kind-agnostic
// so new resource types need no schema change; the agent dispatches by Kind.
type DesiredResource struct {
	Kind string          `json:"kind"`
	Name string          `json:"name"`
	Spec json.RawMessage `json:"spec,omitempty"`
}

// Entitlement is the plan/limits envelope the console reads to decide what it may
// offer. Sourced from the billing system upstream; static for now.
type Entitlement struct {
	Plan            string `json:"plan"`
	Source          string `json:"source"`
	MaxEnvironments *int   `json:"max_environments"`
	MaxDatapacks    *int   `json:"max_datapacks"`
}
