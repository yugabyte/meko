// AWS Bedrock AgentCore control-plane integration for deployments.
//
// Two AWS identities are in play for a BYOC deployment, and this file's
// central design decision is collapsing them into one IAM role in the
// customer's account:
//
//   - the role Meko ASSUMES (via sts:AssumeRole) to call CreateAgentRuntime /
//     GetAgentRuntime -- this is deployment.RoleARN, extending
//     marketplace_service.go's stscreds.NewAssumeRoleProvider pattern from a
//     single static role at startup to one role per deployment row, resolved
//     at request time.
//   - the execution role AgentCore itself assumes at invoke time
//     (CreateAgentRuntimeInput.RoleArn) to run the container.
//
// The BYOC trust-policy template below grants BOTH Meko's AWS account and
// the bedrock-agentcore.amazonaws.com service principal on the same role, so
// the customer creates and pastes exactly one ARN. Managed-mode deployments
// use a separate, Meko-owned execution role (cfg.ManagedExecutionRoleARN)
// instead, since there's no cross-account assumption on that path at all.
package service

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsCfg "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials/stscreds"
	"github.com/aws/aws-sdk-go-v2/service/bedrockagentcore"
	"github.com/aws/aws-sdk-go-v2/service/bedrockagentcorecontrol"
	"github.com/aws/aws-sdk-go-v2/service/bedrockagentcorecontrol/types"
	"github.com/aws/aws-sdk-go-v2/service/sts"

	"meko/api_server/internal/models"
)

// agentCoreAPI is the narrow slice of *bedrockagentcorecontrol.Client the
// service depends on. Satisfied by the real client in production and a
// hand-rolled fake in tests, so deployment orchestration (status
// transitions, error mapping, BYOC-vs-managed branching) is unit-testable
// without a real AWS account -- there's no existing precedent in this
// codebase for mocking an AWS SDK client, so this interface is new
// infrastructure, not a copy of an established pattern.
type agentCoreAPI interface {
	CreateAgentRuntime(ctx context.Context, params *bedrockagentcorecontrol.CreateAgentRuntimeInput, optFns ...func(*bedrockagentcorecontrol.Options)) (*bedrockagentcorecontrol.CreateAgentRuntimeOutput, error)
	GetAgentRuntime(ctx context.Context, params *bedrockagentcorecontrol.GetAgentRuntimeInput, optFns ...func(*bedrockagentcorecontrol.Options)) (*bedrockagentcorecontrol.GetAgentRuntimeOutput, error)
	DeleteAgentRuntime(ctx context.Context, params *bedrockagentcorecontrol.DeleteAgentRuntimeInput, optFns ...func(*bedrockagentcorecontrol.Options)) (*bedrockagentcorecontrol.DeleteAgentRuntimeOutput, error)
}

// agentCoreDataAPI is the data-plane sibling of agentCoreAPI (control
// plane): invoking an already-running AgentCore Runtime, rather than
// creating/inspecting/deleting one. Same rationale for existing as an
// interface -- unit-testable without a real AWS account.
type agentCoreDataAPI interface {
	InvokeAgentRuntime(ctx context.Context, params *bedrockagentcore.InvokeAgentRuntimeInput, optFns ...func(*bedrockagentcore.Options)) (*bedrockagentcore.InvokeAgentRuntimeOutput, error)
}

// newAmbientAWSConfig loads Meko's own ambient AWS credentials (IRSA /
// instance profile / env), used both as the managed-mode client's config
// directly and as the base a BYOC per-deployment assumed-role config is
// copied from.
func newAmbientAWSConfig(ctx context.Context, region string) (aws.Config, error) {
	return awsCfg.LoadDefaultConfig(ctx, awsCfg.WithRegion(region))
}

// byocClient builds an AgentCore control-plane client that calls under the
// authority of the given deployment's cross-account role, assumed fresh per
// call. There is no long-lived caching of assumed credentials across HTTP
// requests to api_server -- for a UI-driven, infrequent action (deploy /
// redeploy / status poll) that's fine; it would not scale to a hot path.
func byocClient(base aws.Config, stsClient *sts.Client, region, roleARN, externalID string) *bedrockagentcorecontrol.Client {
	provider := stscreds.NewAssumeRoleProvider(stsClient, roleARN, func(o *stscreds.AssumeRoleOptions) {
		o.RoleSessionName = "meko-api-server-deployment"
		o.ExternalID = aws.String(externalID)
	})
	cfg := base.Copy()
	cfg.Region = region
	cfg.Credentials = aws.NewCredentialsCache(provider)
	return bedrockagentcorecontrol.NewFromConfig(cfg)
}

// byocDataClient is byocClient's data-plane sibling -- same fresh-assumed-
// role-per-call tradeoff, but on a hotter path (Invoke runs on every "Run a
// review" click, not just deploy/status-poll/delete). Fine for v1; a
// fast-follow would cache the per-deployment client (keyed by deployment
// ID, TTL'd to the assumed credentials' lifetime) instead of re-assuming
// the role on every invocation.
func byocDataClient(base aws.Config, stsClient *sts.Client, region, roleARN, externalID string) *bedrockagentcore.Client {
	provider := stscreds.NewAssumeRoleProvider(stsClient, roleARN, func(o *stscreds.AssumeRoleOptions) {
		o.RoleSessionName = "meko-api-server-deployment-invoke"
		o.ExternalID = aws.String(externalID)
	})
	cfg := base.Copy()
	cfg.Region = region
	cfg.Credentials = aws.NewCredentialsCache(provider)
	return bedrockagentcore.NewFromConfig(cfg)
}

// generateExternalID mints the confused-deputy token embedded in the BYOC
// trust policy and later required on every sts:AssumeRole call against that
// role. Same crypto/rand + hex idiom as models.newToken / the MCP PAT flow.
func generateExternalID() (string, error) {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generate external id: %w", err)
	}
	return hex.EncodeToString(b), nil
}

// trustPolicyDocument mirrors the shape of an IAM AssumeRolePolicyDocument.
// Marshaled with indentation so it can be pasted directly into the AWS
// console's "Edit trust policy" JSON editor.
type trustPolicyDocument struct {
	Version   string                 `json:"Version"`
	Statement []trustPolicyStatement `json:"Statement"`
}

type trustPolicyStatement struct {
	Effect    string                       `json:"Effect"`
	Principal trustPolicyPrincipal         `json:"Principal"`
	Action    string                       `json:"Action"`
	Condition map[string]map[string]string `json:"Condition,omitempty"`
}

type trustPolicyPrincipal struct {
	AWS     string `json:"AWS,omitempty"`
	Service string `json:"Service,omitempty"`
}

// bedrockAgentCoreServicePrincipal is the AWS service principal AgentCore
// Runtime assumes an execution role as, at invoke time.
const bedrockAgentCoreServicePrincipal = "bedrock-agentcore.amazonaws.com"

// renderTrustPolicy builds the two-principal trust policy a BYOC customer
// pastes onto the one IAM role they create: Meko's own AWS account (gated
// by externalID, the standard confused-deputy mitigation for cross-account
// sts:AssumeRole) and the AgentCore service principal, so the SAME role
// also works as CreateAgentRuntimeInput.RoleArn (the execution role).
func renderTrustPolicy(mekoAWSAccountID, externalID string) (string, error) {
	doc := trustPolicyDocument{
		Version: "2012-10-17",
		Statement: []trustPolicyStatement{
			{
				Effect:    "Allow",
				Principal: trustPolicyPrincipal{AWS: fmt.Sprintf("arn:aws:iam::%s:root", mekoAWSAccountID)},
				Action:    "sts:AssumeRole",
				Condition: map[string]map[string]string{
					"StringEquals": {"sts:ExternalId": externalID},
				},
			},
			{
				Effect:    "Allow",
				Principal: trustPolicyPrincipal{Service: bedrockAgentCoreServicePrincipal},
				Action:    "sts:AssumeRole",
			},
		},
	}
	b, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return "", fmt.Errorf("render trust policy: %w", err)
	}
	return string(b), nil
}

// mapAgentRuntimeStatus converts AgentCore's AgentRuntimeStatus into the
// deployments.status vocabulary the UI polls on. Anything not explicitly
// terminal-good or terminal-bad stays "deploying" so the poller keeps
// watching it.
func mapAgentRuntimeStatus(s types.AgentRuntimeStatus) string {
	switch s {
	case types.AgentRuntimeStatusReady:
		return models.DeploymentStatusActive
	case types.AgentRuntimeStatusCreateFailed, types.AgentRuntimeStatusUpdateFailed:
		return models.DeploymentStatusFailed
	default: // CREATING, UPDATING, DELETING
		return models.DeploymentStatusDeploying
	}
}
