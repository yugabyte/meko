package api

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"meko/api_server/internal/repository"
	"meko/api_server/internal/service"
)

// ServerConfig holds the configuration for the API server.
//
// The api_server runs against two logical database connections so the
// meko_system control-plane schema can be hosted on a different
// physical database than the per-datapack data plane (datapacks.*,
// dist_rag.*). When both fields point at the same *sql.DB the layout
// collapses to the original single-database deployment.
type ServerConfig struct {
	// SystemDB serves the meko_system.* control-plane schema:
	// users, agents, datapack registry, knowledge_bases registry,
	// MCP PATs, feedback, sharing/invites, langfuse mappings,
	// yb_universes, tier_config.
	SystemDB *sql.DB
	// DB serves the per-datapack data plane: datapacks.* (mem0,
	// promoted_collection, knowledge_bases chunk tables) and
	// dist_rag.* (sources, documents, vector_indexes).
	DB                  *sql.DB
	CognitoRegion       string
	CognitoPoolID       string
	CognitoClientID     string
	CognitoClientSecret string
	CognitoDomain       string
	LangfuseHost        string
	LangfuseAdminAPIKey string
	// Optional Langfuse Postgres pool. Nil when unconfigured; dependent
	// endpoints return 503.
	LangfuseDB           *sql.DB
	OpenAIAPIKey         string
	OpenAILLMModel       string
	OpenAIEmbeddingModel string
	// MemoryListSkipVectorSort, when true, skips the OpenAI summary +
	// embedding step in the memory/learning list path and orders results by
	// meko_created_at DESC, id DESC. Default true.
	MemoryListSkipVectorSort bool
	Port                     int
	CRDNamespace             string
	S3AccessKey              string
	S3AuthMode               string
	S3Bucket                 string
	S3Endpoint               string
	S3PublicEndpoint         string
	S3Region                 string
	S3SecretKey              string
	MCPServerURL             string
	DeployEnv                repository.DeployEnv
	// MCPInternalSecret gates the internal MCP token validation
	// endpoint (POST /api/v1/internal/meko-tokens/validate). When
	// empty, the endpoint returns 503.
	MCPInternalSecret string

	// AdminAPIKey gates the /admin/* metrics endpoints consumed by
	// meko_admin (X-Meko-Admin-Secret header). When empty, those
	// endpoints return 503.
	AdminAPIKey string

	// ConsoleEmbedAdminSecret writes AdminAPIKey into the YCP console page, so the
	// console never asks for it. Off by default: it hands the fleet-admin credential
	// to anyone who can load the page, which is only reasonable where the console is
	// already the trust boundary (a dev control plane on an internal LoadBalancer or a
	// tailnet) and not where it is fronted at a shared public domain. See
	// handlers.ConsoleHandler.
	ConsoleEmbedAdminSecret bool

	// RequireAccountHeader rejects any fleet call that does not carry
	// X-Meko-Account-ID, instead of serving it against the whole fleet. For a
	// control plane reached only through an account-aware proxy such as YBM, which
	// stamps the header on every call; it takes away the unscoped path the built-in
	// console, the ycp e2e and an operator's curl rely on, so it is off by default.
	RequireAccountHeader bool

	// MetricsURL is the Prometheus-compatible query API the environment metrics
	// route reads (the control plane's Thanos). Empty leaves the route registered
	// and answering 503 rather than absent, so a control plane brought up before
	// the monitoring wiring says why instead of 404ing.
	MetricsURL string

	// YBAAPIToken authenticates read-only calls to YugabyteDB Anywhere
	// (X-AUTH-YW-API-TOKEN) for the admin Data Plane tab. When empty, the
	// /admin/dataplane endpoint reports the data plane as not configured.
	// The universe's management URL + UUID come from meko_system.yb_universes,
	// so the token is the only YBA config passed at runtime.
	YBAAPIToken string

	// SES configuration for invite / share notification emails.
	// When SESFromAddress is empty, email sending is disabled.
	SESRegion      string
	SESFromAddress string
	// AppURL is the base URL of the Meko web app included in invite emails.
	// Derived from DeployEnv: prod → https://cloud.mekodata.ai, dev → https://app.mekodev.com.
	AppURL string

	// Stripe configuration for the billing endpoints. When
	// StripeSecretKey or StripeWebhookSecret is empty the
	// /billing/* routes are not registered and the UI's Upgrade /
	// Manage payment buttons remain visibly disabled.
	StripeSecretKey     string
	StripeWebhookSecret string
	// One Stripe Price ID per Meko paid tier. Enterprise has no price
	// (billed off-platform); free obviously has no price either.
	StripePriceIDStarter string
	StripePriceIDGrowth  string
	StripePriceIDScale   string

	// AWS Marketplace SaaS Contract integration. When ProductCode is
	// empty the /marketplace/* routes are not registered and the UI's
	// AWS-upgrade CTA stays hidden. Region defaults to us-east-1
	// (Marketplace APIs are only served there today).
	//
	// TODO: AWS-PRODUCT-CODE — set once your listing publishes.
	// TODO: AWS-WELCOME-URL — meko_ui path to redirect the buyer to
	//       after a successful register call.
	AWSMarketplaceProductCode string
	AWSMarketplaceRegion      string
	AWSMarketplaceWelcomeURL  string
	// AWSMarketplaceRoleARN (optional): when set, api_server assumes this
	// role in the seller account before calling ResolveCustomer /
	// GetEntitlements. Needed when the app pod's AWS account is not the
	// Marketplace seller account. Empty for single-account deployments.
	AWSMarketplaceRoleARN string

	// Agent deployments (AWS Bedrock AgentCore). DeploymentsRegion defaults
	// to us-east-1 when empty. Create / BYOC-init return a clear error
	// until LabsAgentImagesRepo, DeploymentsManagedExecutionRoleARN,
	// and DeploymentsMekoAWSAccountID are all set (service.DeploymentConfig.Enabled).
	DeploymentsRegion string
	// LabsAgentImagesRepo is the shared, CI-published ECR repo every
	// deployable lab agent's image lives under, keyed by agent_slug:
	// {LabsAgentImagesRepo}/{agent_slug}:latest (service.DeploymentConfig.ImageURI).
	// Wiring up a new deployable lab needs no new env var here -- just a new
	// entry in service.SupportedDeploymentAgentSlugs and an image published
	// at the conventional path.
	LabsAgentImagesRepo string
	// DeploymentsManagedExecutionRoleARN is the Meko-owned IAM role
	// AgentCore assumes to run managed-mode deployments' containers.
	DeploymentsManagedExecutionRoleARN string
	// DeploymentsMekoAWSAccountID is embedded as the trusted principal in
	// every BYOC trust-policy template (see service/agentcore_client.go).
	DeploymentsMekoAWSAccountID string
	// LabsAgentKAdminEmail is the account a Labs datapack gets shared
	// with so the shared AgentK deployment (which authenticates as this
	// account, not the invoking user) can read it. Sharing normally
	// requires the recipient to accept before access is granted;
	// SharingService auto-accepts invites to this one specific address
	// so the lab's "share with AgentK" step doesn't need a human to
	// click accept on agentk-admin's behalf. Empty disables auto-accept
	// (every invite behaves exactly as it does today).
	LabsAgentKAdminEmail string
	// MCPPATFlushInterval controls how often the background
	// goroutine drains the in-memory last_used_at touch map onto the
	// database. Zero (the default) falls back to
	// defaultMCPPATFlushInterval. Values below mcpPATFlushIntervalFloor
	// are clamped up at startup to prevent a misconfigured tiny
	// interval from creating a write storm.
	MCPPATFlushInterval time.Duration

	// PATRateLimitPerMin caps requests per individual PAT. Zero or
	// negative disables the per-PAT limiter (relies on Cognito-side
	// limits + IP-level limits only). Default applied in main.go.
	PATRateLimitPerMin int

	// PATRateLimitBurst is the burst allowance on top of
	// PATRateLimitPerMin. Lets one-shot scripted operations succeed
	// even if the steady rate is below the cap. Default applied in
	// main.go.
	PATRateLimitBurst int

	// MaxArtifactSizeBytes caps the decoded file size accepted by
	// POST /datapacks/:id/artifacts. 0 falls back to the handler
	// default (5 MiB). Set via --max-artifact-size-bytes or
	// MEKO_MAX_ARTIFACT_SIZE_BYTES.
	MaxArtifactSizeBytes int64

	// ShowPGAMP gates the AMP (Postgres databases) feature. Off by default;
	// exposed to the UI via the runtime-config flag SHOW_PG_AMP. The AMP
	// controller endpoint itself is NOT configured here — an admin registers
	// it at runtime (meko_system.amp_registration, via POST /amp/registration).
	ShowPGAMP bool

	// EnableConversationSearch gates the conversation-search /
	// conversation-embedding feature (folded into the conversation detail's
	// can_index, which drives the UI's Index-conversation button). Off by
	// default; mirrors MEKO_ENABLE_CONVERSATION_SEARCH, the same env var that
	// registers the conversation_index_history MCP tool in meko-mcp-server
	// and feeds the UI runtime-config key of the same name.
	EnableConversationSearch bool

	// ControlPlaneMode gates the YCP fleet registry + agent API (BYOC control
	// plane). Off by default; when set, /api/v1/environments and /api/v1/agent/*
	// are registered against meko_system.environments.
	ControlPlaneMode bool

	// BootstrapDefaults is the stack a newly registered environment is told to run,
	// so an enrolling agent has something to do immediately instead of waiting for a
	// separate command. Control-plane mode only; see service.BootstrapDefaults.
	BootstrapDefaults service.BootstrapDefaults

	// BasePath mounts every route under a URL prefix (e.g. "/ycp") for
	// deployments fronted at a subpath of a shared domain instead of their own
	// host — YCP at https://devcloud.yugabyte.com/ycp/ is the case this exists
	// for. Empty (the default) serves at the domain root. Spelling is forgiving:
	// "ycp", "/ycp" and "/ycp/" all normalize to "/ycp" (see normalizeBasePath) —
	// but anything that is not a path prefix (a full URL, a route pattern, a dot
	// segment) fails NewRouter rather than being coerced into an unreachable
	// mount. GET /api/v1/health stays reachable at the domain root either way so
	// container probes don't have to know the prefix.
	BasePath string
}

// deploymentStatusPollInterval is how often runDeploymentStatusPoller checks
// AWS for every deployment currently in status="deploying". CreateAgentRuntime
// typically resolves in tens of seconds to a couple minutes, so this doesn't
// need MCPPATFlushInterval's configurability -- a fixed cadence is fine.
const deploymentStatusPollInterval = 15 * time.Second

// defaultMCPPATFlushInterval is the fallback flush cadence used when
// ServerConfig.MCPPATFlushInterval is zero. 30s balances staleness
// in the UI against write amplification at the validation hot-path.
const defaultMCPPATFlushInterval = 30 * time.Second

// mcpPATFlushIntervalFloor is the lower bound enforced on operator-
// supplied flush intervals. Anything below this defeats the whole
// point of coalescing -- the validator hot path could end up issuing
// roughly one UPDATE per validate call. We warn-and-clamp rather
// than reject so a typo in MCP_PAT_FLUSH_INTERVAL doesn't block boot.
const mcpPATFlushIntervalFloor = 1 * time.Second

// effectiveFlushInterval resolves the configured flush interval
// against the default and floor:
//
//   - zero      -> defaultMCPPATFlushInterval (30s)
//   - 0 < v < floor -> mcpPATFlushIntervalFloor (1s) + slog.Warn
//   - v >= floor    -> v as-is
//
// No upper bound: operators may legitimately want lazy flushes
// (e.g. 5m on a low-traffic preview env) and the in-memory map size
// is naturally bounded by MaxActivePATsPerUser * active users.
func effectiveFlushInterval(cfg ServerConfig) time.Duration {
	v := cfg.MCPPATFlushInterval
	if v <= 0 {
		return defaultMCPPATFlushInterval
	}
	if v < mcpPATFlushIntervalFloor {
		slog.Warn("mcp pat flush interval below floor, clamping",
			"requested", v,
			"floor", mcpPATFlushIntervalFloor,
		)
		return mcpPATFlushIntervalFloor
	}
	return v
}

// Run starts the Echo server, launches the MCP PAT last_used_at
// flusher goroutine, and blocks until a shutdown signal is received.
// On shutdown the server is drained, the flusher is stopped, and one
// final flush is attempted so we don't lose pending touches across a
// rolling restart.
func Run(cfg ServerConfig) error {
	e, reg, err := NewRouter(cfg)
	if err != nil {
		return fmt.Errorf("failed to initialize router: %w", err)
	}

	flushInterval := effectiveFlushInterval(cfg)
	slog.Info("mcp pat last_used flusher configured",
		"interval", flushInterval,
		"requested", cfg.MCPPATFlushInterval,
	)

	flusherCtx, flusherCancel := context.WithCancel(context.Background())
	flusherDone := make(chan struct{})
	go runMCPPATFlusher(flusherCtx, reg, flushInterval, flusherDone)

	deployPollerCtx, deployPollerCancel := context.WithCancel(context.Background())
	deployPollerDone := make(chan struct{})
	go runDeploymentStatusPoller(deployPollerCtx, reg, deploymentStatusPollInterval, deployPollerDone)

	errCh := make(chan error, 1)
	go func() {
		addr := fmt.Sprintf(":%d", cfg.Port)
		slog.Info("api server starting", "port", cfg.Port)
		if err := e.Start(addr); err != nil {
			errCh <- fmt.Errorf("server error: %w", err)
		}
		close(errCh)
	}()

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)

	select {
	case sig := <-quit:
		slog.Info("shutdown signal received", "signal", sig)
	case err := <-errCh:
		if err != nil {
			flusherCancel()
			<-flusherDone
			deployPollerCancel()
			<-deployPollerDone
			return err
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	slog.Info("shutting down api server")
	if err := e.Shutdown(ctx); err != nil {
		flusherCancel()
		<-flusherDone
		deployPollerCancel()
		<-deployPollerDone
		return fmt.Errorf("server shutdown error: %w", err)
	}

	flusherCancel()
	<-flusherDone
	deployPollerCancel()
	<-deployPollerDone

	slog.Info("api server stopped")
	return nil
}

// runMCPPATFlusher drains MCPPATRepo.QueueLastUsedTouch on a fixed
// interval. It exits cleanly when ctx is cancelled, performing one
// last flush against a fresh background context so any in-flight
// touches are persisted across shutdown.
//
// The interval is supplied by the caller (resolved via
// effectiveFlushInterval); the function itself does no
// validation/clamping so it can be unit-tested with any cadence.
func runMCPPATFlusher(
	ctx context.Context,
	reg *serviceRegistry,
	interval time.Duration,
	done chan<- struct{},
) {
	defer close(done)
	if reg == nil || reg.mcpPATRepo == nil {
		slog.Warn("mcp pat flusher: registry/repo nil, exiting")
		return
	}

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	slog.Info("mcp pat last_used flusher started",
		"interval", interval)

	for {
		select {
		case <-ctx.Done():
			finalCtx, cancel := context.WithTimeout(
				context.Background(), 5*time.Second)
			n, err := reg.mcpPATRepo.FlushLastUsedTouches(finalCtx)
			cancel()
			if err != nil {
				slog.Warn("mcp pat last_used flusher: final flush error",
					"error", err)
			} else if n > 0 {
				slog.Info("mcp pat last_used flusher: final flush",
					"rows_updated", n)
			}
			slog.Info("mcp pat last_used flusher stopped")
			return
		case <-ticker.C:
			n, err := reg.mcpPATRepo.FlushLastUsedTouches(ctx)
			if err != nil {
				slog.Warn("mcp pat last_used flush error", "error", err)
				continue
			}
			if n > 0 {
				slog.Debug("mcp pat last_used flushed",
					"rows_updated", n)
			}
		}
	}
}

// runDeploymentStatusPoller drains DeploymentService.PollDeploying on a
// fixed interval, same shape as runMCPPATFlusher above: exits cleanly when
// ctx is cancelled, running one last pass against a fresh background
// context before returning.
func runDeploymentStatusPoller(
	ctx context.Context,
	reg *serviceRegistry,
	interval time.Duration,
	done chan<- struct{},
) {
	defer close(done)
	if reg == nil || reg.deploymentSvc == nil {
		slog.Warn("deployment status poller: registry/service nil, exiting")
		return
	}

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	slog.Info("deployment status poller started", "interval", interval)

	for {
		select {
		case <-ctx.Done():
			finalCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			reg.deploymentSvc.PollDeploying(finalCtx)
			cancel()
			slog.Info("deployment status poller stopped")
			return
		case <-ticker.C:
			reg.deploymentSvc.PollDeploying(ctx)
		}
	}
}
