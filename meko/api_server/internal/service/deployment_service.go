// Package service (this file) — agent deployments to a serverless runtime.
//
// Follows the datapack_service.go shape (validation -> repo -> best-effort
// side calls) rather than marketplace_service.go's narrower AWS-wrapper
// shape, since deployments need both DB writes and AWS orchestration.
package service

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"regexp"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/bedrockagentcore"
	"github.com/aws/aws-sdk-go-v2/service/bedrockagentcorecontrol"
	"github.com/aws/aws-sdk-go-v2/service/bedrockagentcorecontrol/types"
	"github.com/aws/aws-sdk-go-v2/service/sts"

	"meko/api_server/internal/models"
	"meko/api_server/internal/repository"
)

// SupportedDeploymentAgentSlugs are the agents wired end-to-end for
// deployment today. A second deployable agent (a second Labs demo) adds a
// second entry here -- no schema change (agent_slug itself is a
// loosely-typed column, see the migration) and no new env var: its image is
// resolved automatically from MEKO_LABS_AGENT_IMAGES_REPO by convention
// (DeploymentConfig.ImageURI), as long as CI publishes it to
// {repo}/{agent_slug}:latest.
var SupportedDeploymentAgentSlugs = map[string]bool{
	"adlc-review-agent": true,
}

var deploymentRoleARNPattern = regexp.MustCompile(`^arn:aws:iam::\d{12}:role/.+$`)

// DeploymentConfig carries the deploy-time config the service needs.
type DeploymentConfig struct {
	// DefaultRegion is the AWS region every deployment (managed or BYOC) is
	// created in. AgentCore Runtime is regional; multi-region deployments
	// are not modeled yet.
	DefaultRegion string
	// AgentImagesRepo is the shared, CI-published ECR repo every deployable
	// agent's image lives under. Resolved per agent_slug by convention (see
	// ImageURI) rather than one field/env var per agent -- adding a new
	// deployable lab is purely additive: a new SupportedDeploymentAgentSlugs
	// entry, and an image at {AgentImagesRepo}/{agent_slug}:latest. For
	// BYOC, the customer's execution role needs pull access to this same
	// repo via an ECR repository policy (validate this at implementation/
	// rollout time: if AgentCore Runtime rejects a cross-account
	// ContainerUri, the fallback is a one-time image copy into a repo in the
	// customer's own account per deployment).
	AgentImagesRepo string
	// ManagedExecutionRoleARN is the Meko-owned IAM role AgentCore assumes
	// to run managed-mode deployments' containers. Distinct from the BYOC
	// case, where the customer's own role (see agentcore_client.go) serves
	// as both the cross-account-assumed role AND the execution role.
	ManagedExecutionRoleARN string
	// MekoAWSAccountID is the account BYOC trust policies grant as a
	// trusted principal (see renderTrustPolicy).
	MekoAWSAccountID string
}

// Enabled reports whether deployments are configured. False when the
// operator hasn't set the managed execution role or Meko's own account id
// yet -- the routes stay registered either way (unlike Marketplace) since
// List/Get/Delete on existing rows should keep working, but Create/BYOC-init
// return a clear error instead of a confusing AWS failure.
func (cfg DeploymentConfig) Enabled() bool {
	return cfg.ManagedExecutionRoleARN != "" && cfg.MekoAWSAccountID != "" && cfg.AgentImagesRepo != ""
}

// ImageURI derives the ECR image URI for agentSlug by convention:
// {AgentImagesRepo}/{agentSlug}:latest. No per-agent config -- CI is
// responsible for publishing each supported agent's image to this path.
func (cfg DeploymentConfig) ImageURI(agentSlug string) string {
	return fmt.Sprintf("%s/%s:latest", cfg.AgentImagesRepo, agentSlug)
}

// DeploymentService orchestrates agent-deployment lifecycle: validation,
// managed/BYOC AWS client construction, and CreateAgentRuntime /
// GetAgentRuntime orchestration.
type DeploymentService struct {
	repo        *repository.DeploymentRepo
	sharedRepo  *repository.SharedDeploymentRepo
	cfg         DeploymentConfig
	sts         *sts.Client
	base        aws.Config
	managed     agentCoreAPI
	managedData agentCoreDataAPI
}

// DeploymentServiceOption customizes NewDeploymentService's construction --
// currently just for injecting fake AWS clients in tests (see
// WithAgentCoreClients); NewDeploymentService's own AWS-config-loading
// default remains what every real caller uses.
type DeploymentServiceOption func(*DeploymentService)

// WithAgentCoreClients overrides the control- and data-plane AgentCore
// clients NewDeploymentService would otherwise build from ambient AWS
// config. For tests only -- lets Invoke/CreateManaged/etc. be exercised
// against a fake without a real AWS account.
func WithAgentCoreClients(control agentCoreAPI, data agentCoreDataAPI) DeploymentServiceOption {
	return func(s *DeploymentService) {
		s.managed = control
		s.managedData = data
	}
}

// NewDeploymentService builds the service. Loads Meko's own ambient AWS
// config once (IRSA / instance profile / env) -- used directly for managed
// mode and as the base a BYOC per-deployment assumed-role config is copied
// from, extending marketplace_service.go's pattern.
func NewDeploymentService(ctx context.Context, repo *repository.DeploymentRepo, sharedRepo *repository.SharedDeploymentRepo, cfg DeploymentConfig, opts ...DeploymentServiceOption) (*DeploymentService, error) {
	region := cfg.DefaultRegion
	if region == "" {
		region = "us-east-1"
	}
	awsConf, err := newAmbientAWSConfig(ctx, region)
	if err != nil {
		return nil, fmt.Errorf("deployments: load aws config: %w", err)
	}
	// Logged because a wrong region here (stale env var, process not
	// restarted after a config change) surfaces later as a confusing
	// AWS-side "resource not found" rather than an obvious region mismatch --
	// this line is the one place to confirm what actually got loaded.
	slog.Info("deployments: aws config loaded", "region", region)
	svc := &DeploymentService{
		repo:        repo,
		sharedRepo:  sharedRepo,
		cfg:         cfg,
		sts:         sts.NewFromConfig(awsConf),
		base:        awsConf,
		managed:     bedrockagentcorecontrol.NewFromConfig(awsConf),
		managedData: bedrockagentcore.NewFromConfig(awsConf),
	}
	for _, opt := range opts {
		opt(svc)
	}
	if !cfg.Enabled() {
		slog.Warn("deployments: disabled (set managed execution role arn, meko aws account id, and labs agent images repo to enable create/byoc-init)")
	}
	return svc, nil
}

func validateAgentSlug(slug string) error {
	if !SupportedDeploymentAgentSlugs[slug] {
		return fmt.Errorf("%w: agent_slug %q is not deployable yet", ErrValidation, slug)
	}
	return nil
}

// List returns every deployment owned by userID.
func (s *DeploymentService) List(ctx context.Context, userID string) ([]models.Deployment, error) {
	if userID == "" {
		return nil, fmt.Errorf("%w: user_id is required", ErrValidation)
	}
	return s.repo.ListByUser(ctx, userID)
}

// Get returns one deployment by (userID, id), or (nil, nil) if absent.
func (s *DeploymentService) Get(ctx context.Context, userID, id string) (*models.Deployment, error) {
	if id == "" {
		return nil, fmt.Errorf("%w: id is required", ErrValidation)
	}
	return s.repo.GetByID(ctx, userID, id)
}

// Delete best-effort tears down the AWS-side AgentCore Runtime (if one was
// ever created) and removes the row. AWS being unreachable does not block
// the delete -- an orphaned runtime is a cleanup task, not a reason to trap
// a user with a deployment row they can't remove.
func (s *DeploymentService) Delete(ctx context.Context, userID, id string) error {
	dep, err := s.repo.GetByID(ctx, userID, id)
	if err != nil {
		return fmt.Errorf("deployments: lookup before delete: %w", err)
	}
	if dep == nil {
		return fmt.Errorf("deployment %q not found", id)
	}
	if dep.AgentRuntimeID != nil && *dep.AgentRuntimeID != "" {
		client, clientErr := s.clientFor(dep)
		if clientErr != nil {
			slog.Warn("deployments: could not build aws client for delete cleanup",
				"deployment_id", id, "error", clientErr)
		} else if _, err := client.DeleteAgentRuntime(ctx, &bedrockagentcorecontrol.DeleteAgentRuntimeInput{
			AgentRuntimeId: dep.AgentRuntimeID,
		}); err != nil {
			slog.Warn("deployments: aws delete failed; deleting row anyway",
				"deployment_id", id, "error", err)
		}
	}
	return s.repo.DeleteByID(ctx, userID, id)
}

// CreateManaged creates a deployment into Meko's own AWS account and starts
// provisioning it. Returns immediately once CreateAgentRuntime accepts the
// request (status="deploying") -- the status poller (see
// server.go:runDeploymentStatusPoller) tracks it through to active/failed.
func (s *DeploymentService) CreateManaged(ctx context.Context, userID, agentSlug string) (*models.Deployment, error) {
	if !s.cfg.Enabled() {
		return nil, errors.New("deployments: service not configured")
	}
	if userID == "" {
		return nil, fmt.Errorf("%w: user_id is required", ErrValidation)
	}
	if err := validateAgentSlug(agentSlug); err != nil {
		return nil, err
	}

	dep, err := s.repo.Create(ctx, repository.CreateDeploymentParams{
		UserID:      userID,
		AgentSlug:   agentSlug,
		Runtime:     models.DeploymentRuntimeAgentCore,
		AccountMode: models.DeploymentAccountModeManaged,
		Region:      s.cfg.DefaultRegion,
	})
	if err != nil {
		return nil, fmt.Errorf("deployments: create managed: %w", err)
	}

	if err := s.startDeploy(ctx, dep, s.managed, s.cfg.ManagedExecutionRoleARN); err != nil {
		return nil, err
	}
	return dep, nil
}

// ByocInitResult is what StartBYOC returns for the caller to act on:
// the pending deployment plus the rendered trust-policy JSON to paste into
// their own AWS account.
type ByocInitResult struct {
	Deployment      *models.Deployment
	TrustPolicyJSON string
}

// StartBYOC begins BYOC onboarding (step 1 of 2): generates the
// confused-deputy external_id, creates a pending deployment row, and
// renders the trust-policy template the caller pastes into an IAM role in
// their own account. No AWS provisioning happens yet -- that's step 2,
// ConfirmBYOC, once the caller supplies the role ARN they created.
func (s *DeploymentService) StartBYOC(ctx context.Context, userID, agentSlug string) (*ByocInitResult, error) {
	if !s.cfg.Enabled() {
		return nil, errors.New("deployments: service not configured")
	}
	if userID == "" {
		return nil, fmt.Errorf("%w: user_id is required", ErrValidation)
	}
	if err := validateAgentSlug(agentSlug); err != nil {
		return nil, err
	}

	externalID, err := generateExternalID()
	if err != nil {
		return nil, fmt.Errorf("deployments: byoc init: %w", err)
	}

	dep, err := s.repo.Create(ctx, repository.CreateDeploymentParams{
		UserID:      userID,
		AgentSlug:   agentSlug,
		Runtime:     models.DeploymentRuntimeAgentCore,
		AccountMode: models.DeploymentAccountModeBYOC,
		Region:      s.cfg.DefaultRegion,
		ExternalID:  &externalID,
	})
	if err != nil {
		return nil, fmt.Errorf("deployments: byoc init: %w", err)
	}

	trustPolicy, err := renderTrustPolicy(s.cfg.MekoAWSAccountID, externalID)
	if err != nil {
		return nil, fmt.Errorf("deployments: byoc init: %w", err)
	}

	return &ByocInitResult{Deployment: dep, TrustPolicyJSON: trustPolicy}, nil
}

// ConfirmBYOC completes BYOC onboarding (step 2 of 2): validates the
// caller-supplied role ARN, live-verifies it's actually assumable (using
// the external_id minted in StartBYOC) so a trust-policy mistake fails fast
// with a clear error instead of surfacing later as an opaque AWS error,
// stores the ARN, and kicks off provisioning via that same role (used both
// as the cross-account-assumed role and as AgentCore's execution role).
func (s *DeploymentService) ConfirmBYOC(ctx context.Context, userID, id, roleARN string) (*models.Deployment, error) {
	if !deploymentRoleARNPattern.MatchString(roleARN) {
		return nil, fmt.Errorf("%w: role_arn is not a valid IAM role ARN", ErrValidation)
	}

	dep, err := s.repo.GetByID(ctx, userID, id)
	if err != nil {
		return nil, fmt.Errorf("deployments: confirm byoc: lookup: %w", err)
	}
	if dep == nil {
		return nil, fmt.Errorf("deployment %q not found", id)
	}
	if dep.AccountMode != models.DeploymentAccountModeBYOC {
		return nil, fmt.Errorf("%w: deployment %q is not a byoc deployment", ErrValidation, id)
	}
	if dep.ExternalID == nil || *dep.ExternalID == "" {
		return nil, fmt.Errorf("deployment %q has no external_id (byoc init was never completed)", id)
	}

	if _, err := s.sts.AssumeRole(ctx, &sts.AssumeRoleInput{
		RoleArn:         aws.String(roleARN),
		RoleSessionName: aws.String("meko-api-server-deployment-verify"),
		ExternalId:      dep.ExternalID,
	}); err != nil {
		return nil, fmt.Errorf("role not assumable -- check that the trust policy was pasted correctly: %w", err)
	}

	if err := s.repo.SetRoleARN(ctx, userID, id, roleARN); err != nil {
		return nil, fmt.Errorf("deployments: confirm byoc: %w", err)
	}
	dep.RoleARN = &roleARN

	client := byocClient(s.base, s.sts, dep.Region, roleARN, *dep.ExternalID)
	if err := s.startDeploy(ctx, dep, client, roleARN); err != nil {
		return nil, err
	}
	return dep, nil
}

// startDeploy calls CreateAgentRuntime and writes the resulting identifiers
// + status="deploying" back to the row. executionRoleARN is
// CreateAgentRuntimeInput.RoleArn -- the role AgentCore itself assumes at
// invoke time (see the package doc comment in agentcore_client.go for why
// this is the same ARN as the BYOC cross-account role, but a distinct,
// Meko-owned ARN for managed mode).
func (s *DeploymentService) startDeploy(ctx context.Context, dep *models.Deployment, client agentCoreAPI, executionRoleARN string) error {
	out, err := client.CreateAgentRuntime(ctx, &bedrockagentcorecontrol.CreateAgentRuntimeInput{
		AgentRuntimeName: aws.String(runtimeName(dep)),
		RoleArn:          aws.String(executionRoleARN),
		AgentRuntimeArtifact: &types.AgentRuntimeArtifactMemberContainerConfiguration{
			Value: types.ContainerConfiguration{ContainerUri: aws.String(s.cfg.ImageURI(dep.AgentSlug))},
		},
	})
	if err != nil {
		_ = s.repo.UpdateStatus(ctx, dep.ID, models.DeploymentStatusFailed, err.Error(), "", "", "")
		return fmt.Errorf("deployments: create agent runtime: %w", err)
	}

	arn, id, containerURI := aws.ToString(out.AgentRuntimeArn), aws.ToString(out.AgentRuntimeId), s.cfg.ImageURI(dep.AgentSlug)
	if err := s.repo.UpdateStatus(ctx, dep.ID, models.DeploymentStatusDeploying, "", arn, id, containerURI); err != nil {
		return fmt.Errorf("deployments: record create result: %w", err)
	}
	dep.Status = models.DeploymentStatusDeploying
	dep.AgentRuntimeARN = &arn
	dep.AgentRuntimeID = &id
	dep.ContainerURI = &containerURI
	slog.Info("deployment provisioning started",
		"deployment_id", dep.ID, "agent_runtime_id", id, "account_mode", dep.AccountMode)
	return nil
}

// clientFor resolves the AWS client a deployment's own calls (delete,
// status poll) should use: the shared managed client, or a fresh
// per-deployment assumed-role client for BYOC.
func (s *DeploymentService) clientFor(dep *models.Deployment) (agentCoreAPI, error) {
	if dep.AccountMode == models.DeploymentAccountModeManaged {
		return s.managed, nil
	}
	if dep.RoleARN == nil || dep.ExternalID == nil {
		return nil, errors.New("byoc deployment missing role_arn/external_id")
	}
	return byocClient(s.base, s.sts, dep.Region, *dep.RoleARN, *dep.ExternalID), nil
}

// dataClientFor is clientFor's data-plane sibling, for Invoke.
func (s *DeploymentService) dataClientFor(dep *models.Deployment) (agentCoreDataAPI, error) {
	if dep.AccountMode == models.DeploymentAccountModeManaged {
		return s.managedData, nil
	}
	if dep.RoleARN == nil || dep.ExternalID == nil {
		return nil, errors.New("byoc deployment missing role_arn/external_id")
	}
	return byocDataClient(s.base, s.sts, dep.Region, *dep.RoleARN, *dep.ExternalID), nil
}

// Invoke streams a request through to the deployed agent's AgentCore
// Runtime, via the data-plane InvokeAgentRuntime call. Returns the raw
// response body (already SSE-framed by the agent's own container, if it
// supports streaming -- this method does no parsing of it, the same way
// startDeploy doesn't parse AgentCore's provisioning events) for the
// caller to copy through to the HTTP client verbatim. Callers must Close()
// the returned ReadCloser.
func (s *DeploymentService) Invoke(ctx context.Context, userID, id string, payload []byte) (io.ReadCloser, error) {
	dep, err := s.repo.GetByID(ctx, userID, id)
	if err != nil {
		return nil, fmt.Errorf("deployments: invoke: lookup: %w", err)
	}
	if dep == nil {
		return nil, fmt.Errorf("deployment %q not found", id)
	}
	if dep.Status != models.DeploymentStatusActive {
		return nil, fmt.Errorf("%w: deployment %q is not active (status=%s)", ErrValidation, id, dep.Status)
	}
	if dep.AgentRuntimeARN == nil || *dep.AgentRuntimeARN == "" {
		// Should be unreachable once status is active -- startDeploy sets the
		// ARN before status ever transitions there. A data-integrity bug if
		// it happens, not a caller mistake, so not ErrValidation.
		return nil, fmt.Errorf("deployment %q is active but has no agent_runtime_arn", id)
	}

	client, err := s.dataClientFor(dep)
	if err != nil {
		return nil, fmt.Errorf("deployments: invoke: %w", err)
	}

	out, err := client.InvokeAgentRuntime(ctx, &bedrockagentcore.InvokeAgentRuntimeInput{
		AgentRuntimeArn: dep.AgentRuntimeARN,
		Payload:         payload,
		Accept:          aws.String("text/event-stream"),
		ContentType:     aws.String("application/json"),
	})
	if err != nil {
		return nil, fmt.Errorf("deployments: invoke agent runtime: %w", err)
	}
	return out.Response, nil
}

// SharedStatus reports on the shared AgentCore Runtime configured for
// agentSlug -- one entry in the farm of pre-deployed instances any
// signed-in user can invoke without deploying their own (see the
// shared-deployment handlers). Looked up from meko_system.shared_deployments,
// not the deployments table: these runtimes are created and operated
// entirely outside Meko (typically by hand via the AWS CLI/console), so
// there's no owning Meko account and no deploy lifecycle to track here.
// Returns (nil, nil) when agentSlug has no shared ARN configured yet.
//
// There's no cheap way to ask AWS for a shared ARN's live status --
// GetAgentRuntime takes an AgentRuntimeId, not an ARN, and the id isn't
// derivable from the ARN's own suffix (compare runtimeName's naming, which
// AWS doesn't preserve verbatim into the id it assigns) -- so this always
// reports "active" once an ARN is configured: the operator is expected to
// only add a farm entry once they've confirmed the runtime is ready. A
// caller that invokes it before it's actually ready just gets whatever
// error AWS's InvokeAgentRuntime returns.
func (s *DeploymentService) SharedStatus(ctx context.Context, agentSlug string) (*models.Deployment, error) {
	arn, err := s.sharedRepo.GetARN(ctx, agentSlug)
	if err != nil {
		return nil, fmt.Errorf("deployments: shared status: %w", err)
	}
	if arn == "" {
		return nil, nil
	}
	now := time.Now()
	return &models.Deployment{
		ID:              "shared:" + agentSlug,
		AgentSlug:       agentSlug,
		Runtime:         models.DeploymentRuntimeAgentCore,
		AccountMode:     models.DeploymentAccountModeManaged,
		Status:          models.DeploymentStatusActive,
		AgentRuntimeARN: &arn,
		CreatedAt:       now,
		UpdatedAt:       now,
	}, nil
}

// InvokeShared streams a request through to the shared AgentCore Runtime
// configured for agentSlug -- see SharedStatus for how it's looked up. Uses
// the same ambient-AWS-config data-plane client managed-mode deployments
// use, since a manually-created runtime reachable this way is expected to
// live in Meko's own AWS account too.
func (s *DeploymentService) InvokeShared(ctx context.Context, agentSlug string, payload []byte) (io.ReadCloser, error) {
	arn, err := s.sharedRepo.GetARN(ctx, agentSlug)
	if err != nil {
		return nil, fmt.Errorf("deployments: invoke shared: %w", err)
	}
	if arn == "" {
		return nil, fmt.Errorf("shared deployment for agent_slug %q not found", agentSlug)
	}

	out, err := s.managedData.InvokeAgentRuntime(ctx, &bedrockagentcore.InvokeAgentRuntimeInput{
		AgentRuntimeArn: aws.String(arn),
		Payload:         payload,
		Accept:          aws.String("text/event-stream"),
		ContentType:     aws.String("application/json"),
	})
	if err != nil {
		return nil, fmt.Errorf("deployments: invoke shared agent runtime: %w", err)
	}
	return out.Response, nil
}

// PollDeploying is called by the background status poller
// (server.go:runDeploymentStatusPoller) on a fixed interval. It reads every
// deployment currently in status="deploying", calls GetAgentRuntime for
// each, and writes the resulting status back. Errors for one deployment are
// logged and skipped rather than aborting the whole pass.
func (s *DeploymentService) PollDeploying(ctx context.Context) {
	deps, err := s.repo.ListDeploying(ctx)
	if err != nil {
		slog.Warn("deployment status poll: list failed", "error", err)
		return
	}
	for i := range deps {
		dep := &deps[i]
		if dep.AgentRuntimeID == nil || *dep.AgentRuntimeID == "" {
			continue
		}
		client, err := s.clientFor(dep)
		if err != nil {
			slog.Warn("deployment status poll: client build failed", "deployment_id", dep.ID, "error", err)
			continue
		}
		out, err := client.GetAgentRuntime(ctx, &bedrockagentcorecontrol.GetAgentRuntimeInput{
			AgentRuntimeId: dep.AgentRuntimeID,
		})
		if err != nil {
			slog.Warn("deployment status poll: get agent runtime failed", "deployment_id", dep.ID, "error", err)
			continue
		}
		newStatus := mapAgentRuntimeStatus(out.Status)
		message := aws.ToString(out.FailureReason)
		if err := s.repo.UpdateStatus(ctx, dep.ID, newStatus, message, "", "", ""); err != nil {
			slog.Warn("deployment status poll: update failed", "deployment_id", dep.ID, "error", err)
			continue
		}
		if newStatus != models.DeploymentStatusDeploying {
			slog.Info("deployment status resolved", "deployment_id", dep.ID, "status", newStatus)
		}
	}
}

// runtimeName derives a DNS-safe AgentRuntimeName from the owning user and
// agent slug. AgentCore Runtime names must be unique per account/region;
// scoping by user_id keeps two different Meko users deploying the same
// agent_slug into the same managed account from colliding.
func runtimeName(dep *models.Deployment) string {
	return fmt.Sprintf("meko_%s_%s", sanitizeRuntimeNamePart(dep.UserID), sanitizeRuntimeNamePart(dep.AgentSlug))
}

var runtimeNameDisallowed = regexp.MustCompile(`[^a-zA-Z0-9_]`)

func sanitizeRuntimeNamePart(s string) string {
	return runtimeNameDisallowed.ReplaceAllString(s, "_")
}
