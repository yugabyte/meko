package models

import "time"

// Deployment represents a hosted deployment of a Meko agent to a serverless
// runtime (AWS Bedrock AgentCore today).
//
// Backed by meko_system.deployments. Identified by (user_id, id).
// RoleARN / ExternalID are only set for account_mode=byoc; they authorize
// a request Meko signs with its own identity against a trust policy the
// caller writes and can revoke unilaterally, so they are not secrets in
// the credential-at-rest sense -- safe to return to the owning caller.
type Deployment struct {
	CreatedAt       time.Time `json:"created_at"`
	UpdatedAt       time.Time `json:"updated_at"`
	ID              string    `json:"id"`
	UserID          string    `json:"user_id"`
	AgentSlug       string    `json:"agent_slug"`
	Runtime         string    `json:"runtime"`
	AccountMode     string    `json:"account_mode"`
	Region          string    `json:"region"`
	RoleARN         *string   `json:"role_arn,omitempty"`
	ExternalID      *string   `json:"external_id,omitempty"`
	AgentRuntimeARN *string   `json:"agent_runtime_arn,omitempty"`
	AgentRuntimeID  *string   `json:"agent_runtime_id,omitempty"`
	ContainerURI    *string   `json:"container_uri,omitempty"`
	Status          string    `json:"status"`
	StatusMessage   *string   `json:"status_message,omitempty"`
}

// Deployment runtime identifiers. Only "agentcore" is functional today;
// "vercel" / "lyzr" are reserved so the UI can show them as coming soon
// without a schema change once they're wired up.
const (
	DeploymentRuntimeAgentCore = "agentcore"
	DeploymentRuntimeVercel    = "vercel"
	DeploymentRuntimeLyzr      = "lyzr"
)

// Deployment account modes.
const (
	DeploymentAccountModeManaged = "managed"
	DeploymentAccountModeBYOC    = "byoc"
)

// Deployment status values.
const (
	DeploymentStatusPending   = "pending"
	DeploymentStatusDeploying = "deploying"
	DeploymentStatusActive    = "active"
	DeploymentStatusFailed    = "failed"
	DeploymentStatusDeleting  = "deleting"
	DeploymentStatusDeleted   = "deleted"
)
