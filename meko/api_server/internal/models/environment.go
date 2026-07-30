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
	Datapacks       int             `json:"datapacks"`
	MaxDatapacks    int             `json:"max_datapacks,omitempty"`
	Live            bool            `json:"live"`

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
