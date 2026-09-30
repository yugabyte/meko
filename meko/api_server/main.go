// Package main is the entry point for the meko datapack API server.
//
//	@title			Meko Datapack API
//	@version		1.0
//	@description	REST API for managing datapacks, agents, and knowledge bases in the meko platform.
//	@host			localhost:25433
//	@BasePath		/api/v1
//
//	@securityDefinitions.apikey	BearerAuth
//	@in							header
//	@name						Authorization
//	@description				JWT access token from /auth/login. Format: Bearer {token}
package main

import (
	"database/sql"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"time"

	_ "github.com/lib/pq"

	"meko/api_server/internal/api"
	"meko/api_server/internal/db"
	"meko/api_server/internal/repository"
	"meko/api_server/internal/service"
)

func run() error {

	// meko datapack api server configuration
	port := flag.Int("port", 25433, "API server listen port")

	// Logging level. Accepts debug, info, warn, error (case-insensitive).
	// Set via flag or LOG_LEVEL env. Defaults to info.
	logLevel := flag.String("log-level",
		envOrDefault("LOG_LEVEL", "info"),
		"slog level: debug, info, warn, error")

	// Data-plane database configuration. This pool serves the per-datapack
	// schemas (datapacks.* — mem0, promoted collections, KB chunk tables —
	// and dist_rag.* — sources, documents, vector indexes).
	dbHost := flag.String("db-host", "127.0.0.1", "data-plane database host")
	dbUserName := flag.String("db-user-name", "yugabyte", "data-plane database user name")
	dbPassword := flag.String("db-password", "", "data-plane database password")
	dbPort := flag.Int("db-port", 5433, "data-plane database port")
	dbName := flag.String("db-name", "yugabyte", "data-plane database name")
	dbSSLMode := flag.String("db-sslmode", "disable", "data-plane database sslmode (disable, require, verify-ca, verify-full)")

	// Control-plane (meko_system) database configuration. Empty sentinels
	// mean "inherit the data-plane value", so single-database deployments
	// keep working without any extra flags. Set these to point the
	// meko_system.* schema at a separate physical database.
	systemDBHost := flag.String("system-db-host", envOrDefault("SYSTEM_DB_HOST", ""), "meko_system database host (default: same as --db-host)")
	systemDBUserName := flag.String("system-db-user-name", envOrDefault("SYSTEM_DB_USER_NAME", ""), "meko_system database user name (default: same as --db-user-name)")
	systemDBPassword := flag.String("system-db-password", envOrDefault("SYSTEM_DB_PASSWORD", ""), "meko_system database password (default: same as --db-password)")
	systemDBPort := flag.Int("system-db-port", envIntOrDefault("SYSTEM_DB_PORT", 0), "meko_system database port (default: same as --db-port)")
	systemDBName := flag.String("system-db-name", envOrDefault("SYSTEM_DB_NAME", ""), "meko_system database name (default: same as --db-name)")
	systemDBSSLMode := flag.String("system-db-sslmode", envOrDefault("SYSTEM_DB_SSLMODE", ""), "meko_system database sslmode (default: same as --db-sslmode)")

	// migrate only flag
	migrateOnly := flag.Bool("migrate-only", false, "run migrations and exit without starting the server")

	// Scope the migration run to a single set. Both default to false,
	// which runs all migrations. The two flags are mutually exclusive.
	onlySystemMigrate := flag.Bool("only-system-migrate", false, "run only meko_system migrations (implies -migrate-only)")
	onlyDataMigrate := flag.Bool("only-data-migrate", false, "run only data-plane migrations (implies -migrate-only)")
	onlyControlPlaneMigrate := flag.Bool("only-controlplane-migrate", false,
		"run only the YCP fleet-registry migrations against the system DB (implies -migrate-only). "+
			"The only set that runs on plain Postgres; the system set needs YugabyteDB.")

	// Cognito configuration. The env fallbacks are what makes this configurable from a
	// Kubernetes Secret: a data plane the YCP agent deploys wires credentials in with
	// envFrom-secretRef and never as flags, since a flag value is visible in the pod
	// spec to anyone with read access to the namespace. The names match the ones
	// LOCAL_DEV_SETUP.md already documents.
	cognitoRegion := flag.String("cognito-region", envOrDefault("COGNITO_REGION", ""), "AWS region for Cognito User Pool")
	cognitoPoolID := flag.String("cognito-pool-id", envOrDefault("COGNITO_POOL_ID", ""), "Cognito User Pool ID")
	cognitoClientID := flag.String("cognito-client-id", envOrDefault("COGNITO_CLIENT_ID", ""), "Cognito App Client ID")
	cognitoClientSecret := flag.String("cognito-client-secret", envOrDefault("COGNITO_CLIENT_SECRET", ""), "Cognito App Client secret")
	cognitoDomain := flag.String("cognito-domain", envOrDefault("COGNITO_DOMAIN", ""), "Cognito hosted UI domain (e.g. myapp.auth.us-east-2.amazoncognito.com)")

	// Identity provider selection. Empty (or "cognito") keeps the Cognito path,
	// so an existing deployment is unchanged; "oidc" authenticates against any
	// issuer serving a discovery document and registers no password, signup or
	// email-verification routes, because the issuer owns its users.
	authProvider := flag.String("auth-provider", envOrDefault("AUTH_PROVIDER", ""),
		`identity provider: "cognito" (default) or "oidc"`)
	oidcIssuerURL := flag.String("oidc-issuer-url", envOrDefault("OIDC_ISSUER_URL", ""),
		"OIDC issuer, e.g. https://keycloak.example.com/realms/meko — endpoints are discovered from it")
	oidcClientID := flag.String("oidc-client-id", envOrDefault("OIDC_CLIENT_ID", ""), "OIDC client ID")
	oidcClientSecret := flag.String("oidc-client-secret", envOrDefault("OIDC_CLIENT_SECRET", ""),
		"OIDC client secret; empty for a public client, which then relies on PKCE")
	// Not derived from the request on purpose: it has to match the issuer's
	// registered redirect URI exactly, and behind a TLS-terminating proxy this
	// server cannot see its own public origin.
	oidcRedirectURL := flag.String("oidc-redirect-url", envOrDefault("OIDC_REDIRECT_URL", ""),
		"the callback URL registered with the issuer, e.g. https://meko.example.com/api/v1/auth/oidc/callback")
	oidcScopes := flag.String("oidc-scopes", envOrDefault("OIDC_SCOPES", ""),
		"comma-separated scopes beyond openid; empty defaults to profile,email")

	// Kubernetes namespace for MekoUserAccount CRDs
	crdNamespace := flag.String("crd-namespace", "meko-tenants", "Kubernetes namespace for MekoUserAccount CRDs")

	// Langfuse configuration
	langfuseHost := flag.String("langfuse-host", os.Getenv("LANGFUSE_HOST"), "Langfuse base URL (e.g. http://localhost:3000)")
	langfuseAdminAPIKey := flag.String("langfuse-admin-api-key", os.Getenv("LANGFUSE_ADMIN_API_KEY"), "Langfuse admin API key used for per-account org/project provisioning")

	// Langfuse Postgres connection — optional, only needed for read-only
	// observability features (e.g. listing conversations) that read Langfuse
	// trace/observation data directly. When unset, those endpoints return 503.
	langfuseDBHost := flag.String("langfuse-db-host", os.Getenv("LANGFUSE_DB_HOST"), "Langfuse Postgres host (optional)")
	langfuseDBPort := flag.Int("langfuse-db-port", envIntOrDefault("LANGFUSE_DB_PORT", 5433), "Langfuse Postgres port")
	langfuseDBName := flag.String("langfuse-db-name", envOrDefault("LANGFUSE_DB_NAME", "langfuse"), "Langfuse Postgres database name")
	langfuseDBUser := flag.String("langfuse-db-user", envOrDefault("LANGFUSE_DB_USER", "yugabyte"), "Langfuse Postgres user")
	langfuseDBPassword := flag.String("langfuse-db-password", os.Getenv("LANGFUSE_DB_PASSWORD"), "Langfuse Postgres password")
	langfuseDBSSLMode := flag.String("langfuse-db-sslmode", envOrDefault("LANGFUSE_DB_SSLMODE", "disable"), "Langfuse Postgres sslmode (disable, require, verify-ca, verify-full)")

	openAIAPIKey := flag.String("openai-api-key", os.Getenv("OPENAI_API_KEY"), "OpenAI API key")
	openAILLMModel := flag.String("openai-llm-model", os.Getenv("OPEN_AI_LLM_MODEL"), "OpenAI LLM model (default: gpt-4o)")
	openAIEmbeddingModel := flag.String("openai-embedding-model", os.Getenv("OPEN_AI_EMBEDDING_MODEL"), "OpenAI embedding model (default: text-embedding-3-small)")

	// When true (default), the memory/learning list endpoints skip the OpenAI
	// summary + embedding pre-step and order by meko_created_at DESC. Set to
	// false to restore the legacy vector-similarity sort.
	memoryListSkipVectorSort := flag.Bool("memory-list-skip-vector-sort", envBoolOrDefault("MEMORY_LIST_SKIP_VECTOR_SORT", true), "Skip OpenAI vector embedding for memory/learning list and sort by created_at DESC")
	deployEnv := flag.String("deploy-env", envOrDefault("MEKO_DEPLOY_ENV", "prod"), "deployment environment: prod or dev")

	// AMP (Postgres databases) integration. --show-pg-amp gates the feature
	// (mirrored to the UI via the SHOW_PG_AMP runtime flag). The AMP controller
	// endpoint is NOT an env var — an admin registers it at runtime via
	// POST /api/v1/amp/registration (stored in meko_system.amp_registration).
	showPGAMP := flag.Bool("show-pg-amp", envBoolOrDefault("SHOW_PG_AMP", false), "enable the AMP (Postgres databases) feature")

	// Conversation-search / conversation-embedding feature. The SAME env var
	// registers the conversation_index_history MCP tool in meko-mcp-server and
	// gates the UI button (runtime-config key of the same name), so one switch
	// turns the feature on/off end to end. Folded into can_index here.
	enableConversationSearch := flag.Bool("enable-conversation-search",
		envBoolOrDefault("MEKO_ENABLE_CONVERSATION_SEARCH", false),
		"enable the conversation-search / conversation-embedding feature (Index-conversation backfill)")

	// BYOC data plane — the customer runs this stack in their own account.
	// Creates new users 'pro' (no tier caps) instead of the 'free' default;
	// see api.ServerConfig.BYOCMode. The SAME env var hides the hosted-only surfaces
	// in the UI (pricing, tier names, Labs, Feedback, sign-out), so one switch
	// covers both sides.
	enableBYOC := flag.Bool("enable-byoc",
		envBoolOrDefault("MEKO_ENABLE_BYOC", false),
		"run as a BYOC data plane: new users are created pro (no tier caps)")
	// Collections & folders. The SAME env var gates the UI's Collections nav via
	// runtime-config, so one switch turns the feature on/off end to end.
	enableCollections := flag.Bool("enable-collections",
		envBoolOrDefault("MEKO_ENABLE_COLLECTIONS", false),
		"enable the collection tree (collections, folders, datapack placements)")

	// YCP control-plane mode (BYOC). Gates the fleet registry + agent API.
	controlPlaneMode := flag.Bool("control-plane-mode", envBoolOrDefault("CONTROL_PLANE_MODE", false), "enable YCP fleet registry + agent API (control plane)")

	// Deprecated: these all described the stack registration used to seed, which it no
	// longer does — a data plane's workloads come from its chart. Accepted only so a
	// control plane already passing them does not crash-loop on an undefined flag (the
	// warning below says so), and removed in the release after this.
	noBootstrapDefaults := flag.Bool("no-bootstrap-defaults", envBoolOrDefault("MEKO_NO_BOOTSTRAP_DEFAULTS", false),
		"register environments with empty desired state (they then run nothing until explicitly commanded)")
	dataplaneRegistry := flag.String("dataplane-registry", envOrDefault("MEKO_DATAPLANE_REGISTRY", service.DefaultDataPlaneRegistry),
		"registry the pinned data-plane images are pulled from")
	var dataplaneImages imageOverrides
	flag.Var(&dataplaneImages, "dataplane-image",
		"override one component of the pinned release, as component=image "+
			"(database, api_server, mcp_server, rag_worker, inference_gateway, ui). Repeatable.")
	// Kept because the chart and the local scripts set it, and because it predates the
	// release manifest. It is the mcp_server override by another name.
	dataplaneMCPImage := flag.String("dataplane-mcp-image", os.Getenv("MEKO_DATAPLANE_MCP_IMAGE"),
		"override the meko-mcp-server image in the pinned release (equivalent to -dataplane-image mcp_server=...)")
	dataplanePullSecret := flag.String("dataplane-image-pull-secret", os.Getenv("MEKO_DATAPLANE_IMAGE_PULL_SECRET"),
		"name of a docker-registry Secret in the agent's namespace, for pulling the images above")
	dataplaneSecretRef := flag.String("dataplane-secret-ref", os.Getenv("MEKO_DATAPLANE_SECRET_REF"),
		"name of a Secret in the agent's namespace holding data-plane credentials (OPENAI_API_KEY, AWS keys)")

	// Subpath mount. Set when a fronting proxy passes a prefix through instead
	// of stripping it — YCP is served at https://devcloud.yugabyte.com/ycp/, on
	// the same domain as YBM so browser auth tokens and session storage carry
	// over. Empty (the default) serves everything at the domain root.
	// GET /api/v1/health stays at the root either way, for container probes.
	// The env var is namespaced (MEKO_YCP_BASE_PATH) because "BASE_PATH" is a
	// name other things in a shared pod environment plausibly also want.
	basePath := flag.String("base-path", envOrDefault("MEKO_YCP_BASE_PATH", ""),
		"mount every route under this URL prefix (e.g. /ycp) — a plain path prefix, not a URL. Empty serves at the domain root.")

	// MCP internal validation endpoint shared secret. When empty, the
	// /api/v1/internal/meko-tokens/validate endpoint returns 503.
	adminAPIKey := flag.String("admin-api-key",
		os.Getenv("ADMIN_API_KEY"),
		"Shared secret gating the /admin/* metrics endpoints (X-Meko-Admin-Secret). Empty disables them (503).")
	consoleEmbedAdminSecret := flag.Bool("console-embed-admin-secret",
		envBoolOrDefault("MEKO_CONSOLE_EMBED_ADMIN_SECRET", false),
		"write the admin secret into the YCP console page so it never asks for one. "+
			"Hands the fleet-admin credential to anyone who can load the console, so only for "+
			"a control plane whose console is already the trust boundary (internal LB, tailnet).")

	// Prometheus-compatible query API backing GET /environments/:id/metrics. In the
	// YBM control plane this is the in-cluster Thanos, which takes no credentials —
	// a NetworkPolicy is what gates it — so this is a plain URL and not a secret.
	metricsURL := flag.String("metrics-url",
		envOrDefault("MEKO_METRICS_URL", ""),
		"Prometheus-compatible query API root (e.g. http://thanos-query-frontend.monitoring-core.svc.cluster.local:9090/api/v1) "+
			"for the environment metrics route. Empty makes that route 503.")

	// The write side of metrics-url: the OTLP/HTTP door a BYOC data plane ships to.
	// One value, two consumers that have to agree — the install hint prints it and
	// the enroll check accepts only it — because the install runs from a checkout
	// the customer has, so the endpoint is not theirs to choose.
	//
	// Defaults to DEVCLOUD, not prod: a control plane that has not been told which
	// fleet it serves is far more likely to be a dev or test one, and handing out the
	// prod door by default is the worse mistake of the two. Nothing refuses either
	// way — an agent reporting a different door enrols and is marked DEGRADED — so
	// this decides which door the install hint prints and which one an environment is
	// judged against, not whether anything comes up. Every non-devcloud YCP must
	// set it; deploy-ycp-ybm.sh refuses to run without it.
	byocMetricsURL := flag.String("byoc-metrics-url",
		envOrDefault("MEKO_BYOC_METRICS_URL", "https://byoc.devcloud.yugabyte.com/api/v1/otlp/metrics"),
		"OTLP/HTTP metrics door a BYOC data plane ships to. Printed in the install hint; an "+
			"environment whose agent reports a different one is marked DEGRADED, not refused. "+
			"Defaults to the devcloud door; a prod control plane must set "+
			"https://byoc.cloud.yugabyte.com/api/v1/otlp/metrics.")

	requireAccountHeader := flag.Bool("require-account-header",
		envBoolOrDefault("MEKO_REQUIRE_ACCOUNT_HEADER", false),
		"reject fleet calls that do not carry X-Meko-Account-ID, instead of serving them "+
			"against the whole fleet. For a control plane reached only through an account-aware "+
			"proxy such as YBM; it 400s the built-in console, the ycp e2e and operator curl, "+
			"which authenticate with the admin secret alone.")

	ybaAPIToken := flag.String("yba-api-token",
		os.Getenv("YBA_API_TOKEN"),
		"YugabyteDB Anywhere API token (X-AUTH-YW-API-TOKEN) for the admin Data Plane tab. Empty reports the data plane as not configured.")

	mcpInternalSecret := flag.String("mcp-internal-secret",
		os.Getenv("MCP_INTERNAL_SECRET"),
		"shared secret for /internal/meko-tokens/validate (MCP server -> api_server). Empty disables the endpoint.")

	// MCP PAT last_used_at flush cadence. The validator coalesces
	// touches in memory and the background goroutine drains them on
	// this interval. Lower = fresher UI / more DB writes; higher =
	// staler UI / fewer DB writes. Values < 1s are clamped at startup.
	mcpPATFlushInterval := flag.Duration("mcp-pat-flush-interval",
		envDurationOrDefault("MCP_PAT_FLUSH_INTERVAL", 30*time.Second),
		"cadence for flushing MCP PAT last_used_at touches to the DB (e.g. 30s, 2m). Values <1s are clamped to 1s.")

	// Per-PAT rate limit. Applied only on routes that opted into PAT
	// acceptance (the dpDirect group in api/router.go); JWT requests
	// are untouched. perMin <= 0 disables the limiter.
	patRateLimitPerMin := flag.Int("pat-rate-limit-per-min",
		envIntOrDefault("PAT_RATE_LIMIT_PER_MIN", 60),
		"max requests per minute per PAT on opted-in routes (<=0 disables)")
	patRateLimitBurst := flag.Int("pat-rate-limit-burst",
		envIntOrDefault("PAT_RATE_LIMIT_BURST", 10),
		"burst allowance on top of --pat-rate-limit-per-min")

	// S3 configuration
	s3AuthMode := flag.String("s3-auth-mode", envOrDefault("S3_AUTH_MODE", "minio"), "S3 auth mode: minio, iam-user, iam-role")
	s3Endpoint := flag.String("s3-endpoint", envOrDefault("S3_ENDPOINT", "http://localhost:19094"), "S3-compatible endpoint URL (minio mode)")
	s3PublicEndpoint := flag.String("s3-public-endpoint", envOrDefault("S3_PUBLIC_ENDPOINT", ""), "Public S3 endpoint used only for presigned upload/download URLs (browser-reachable host). Empty = use --s3-endpoint.")
	s3AccessKey := flag.String("s3-access-key", envOrDefault("S3_ACCESS_KEY", "meko"), "S3 access key (minio/iam-user modes)")
	s3SecretKey := flag.String("s3-secret-key", envOrDefault("S3_SECRET_KEY", "mekosecret"), "S3 secret key (minio/iam-user modes)")
	s3Bucket := flag.String("s3-bucket", envOrDefault("S3_BUCKET", "meko-kb-uploads"), "S3 bucket for KB file uploads")
	s3Region := flag.String("s3-region", envOrDefault("S3_REGION", "us-east-2"), "S3 region")

	// SES configuration for invite / share notification emails.
	sesFromAddress := flag.String("ses-from-address",
		envOrDefault("SES_FROM_ADDRESS", ""),
		"Verified SES sender address (e.g. no-reply@example.com). Empty disables invite emails.")
	sesRegion := flag.String("ses-region",
		envOrDefault("SES_REGION", ""),
		"AWS region for SES (e.g. us-east-2). Defaults to cognito-region when empty.")

	// Stripe billing configuration. All five values must be set for
	// the /billing/* routes to register; with any blank, the routes
	// stay off and the UI's Upgrade / Manage payment buttons remain
	// disabled. Price IDs come from the Stripe dashboard, one per
	// recurring product (Starter / Growth / Scale).
	stripeSecretKey := flag.String("stripe-secret-key",
		os.Getenv("STRIPE_SECRET_KEY"),
		"Stripe API secret key (sk_live_... or sk_test_...). Empty disables billing routes.")
	stripeWebhookSecret := flag.String("stripe-webhook-secret",
		os.Getenv("STRIPE_WEBHOOK_SECRET"),
		"Stripe webhook signing secret (whsec_...). Required to accept /billing/webhook calls.")
	stripePriceStarter := flag.String("stripe-price-starter",
		os.Getenv("STRIPE_PRICE_STARTER"),
		"Stripe Price ID for the Starter plan (e.g. price_...)")
	stripePriceGrowth := flag.String("stripe-price-growth",
		os.Getenv("STRIPE_PRICE_GROWTH"),
		"Stripe Price ID for the Growth plan (e.g. price_...)")
	stripePriceScale := flag.String("stripe-price-scale",
		os.Getenv("STRIPE_PRICE_SCALE"),
		"Stripe Price ID for the Scale plan (e.g. price_...)")

	// Artifact upload limit.
	maxArtifactSizeBytes := flag.Int64("max-artifact-size-bytes",
		envInt64OrDefault("MEKO_MAX_ARTIFACT_SIZE_BYTES", 5*1024*1024),
		"maximum decoded file size in bytes accepted by POST /datapacks/:id/artifacts (default 5 MiB)")

	// AWS Marketplace SaaS Contract integration. Empty ProductCode
	// disables /marketplace/* routes entirely.
	awsMarketplaceProductCode := flag.String("aws-marketplace-product-code",
		os.Getenv("AWS_MARKETPLACE_PRODUCT_CODE"),
		"AWS Marketplace Product Code assigned to your SaaS listing. Empty disables marketplace routes.")
	awsMarketplaceRegion := flag.String("aws-marketplace-region",
		os.Getenv("AWS_MARKETPLACE_REGION"),
		"AWS region for Marketplace APIs (default us-east-1; Marketplace only serves from there today).")
	awsMarketplaceWelcomeURL := flag.String("aws-marketplace-welcome-url",
		os.Getenv("AWS_MARKETPLACE_WELCOME_URL"),
		"URL to redirect the buyer to after a successful fulfillment (e.g. https://app.meko.io/marketplace/welcome).")
	// Cross-account: set when the AWS account that owns the Marketplace
	// listing (seller) is different from the account this api_server runs
	// in (app). Format: arn:aws:iam::<seller-account-id>:role/<role-name>.
	// Empty for single-account deployments.
	awsMarketplaceRoleARN := flag.String("aws-marketplace-role-arn",
		os.Getenv("AWS_MARKETPLACE_ROLE_ARN"),
		"Cross-account IAM role ARN in the Marketplace seller account (STS:AssumeRole target for ResolveCustomer + GetEntitlements). Empty = use ambient AWS credentials.")

	// Agent deployments (AWS Bedrock AgentCore). Create / BYOC-init return a
	// clear 500 error until labs-agent-images-repo, labs-managed-execution-role-arn,
	// and labs-deployments-aws-account-id are all set (DeploymentConfig.Enabled).
	// labs-agentk-admin-email is unrelated to this gate -- it drives datapack
	// auto-share, not deployment creation.
	deploymentsRegion := flag.String("labs-region",
		os.Getenv("MEKO_LABS_REGION"),
		"AWS region agent deployments are created in (default us-east-1).")
	labsAgentImagesRepo := flag.String("labs-agent-images-repo",
		os.Getenv("MEKO_LABS_AGENT_IMAGES_REPO"),
		"Shared ECR repo every deployable lab agent's image is published under by CI, keyed by agent_slug: {repo}/{agent_slug}:latest. Adding a new deployable lab needs no new env var -- just a new entry in service.SupportedDeploymentAgentSlugs and an image at the conventional path.")
	deploymentsManagedExecutionRoleARN := flag.String("labs-managed-execution-role-arn",
		os.Getenv("MEKO_LABS_MANAGED_EXECUTION_ROLE_ARN"),
		"Meko-owned IAM role AgentCore assumes to run managed-mode deployments' containers.")
	deploymentsMekoAWSAccountID := flag.String("labs-deployments-aws-account-id",
		os.Getenv("MEKO_LABS_DEPLOYMENTS_AWS_ACCOUNT_ID"),
		"Meko's own AWS account id, embedded as the trusted principal in every BYOC trust-policy template.")
	labsAgentKAdminEmail := flag.String("labs-agentk-admin-email",
		os.Getenv("MEKO_LABS_AGENTK_ADMIN_EMAIL"),
		"Account a Labs datapack is shared with so the shared AgentK deployment can read it. Invites to this one address are auto-accepted instead of left pending; empty disables auto-accept.")

	flag.Parse()

	resolvedLogLevel := parseLogLevel(*logLevel)
	slog.SetDefault(slog.New(
		slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{
			Level: resolvedLogLevel,
		}),
	))
	slog.Info("logger configured",
		"requested", *logLevel, "effective", resolvedLogLevel)

	// Resolve system-db settings: empty / zero sentinels inherit from
	// the data-db settings so a single-database deployment "just works".
	resolvedSystemDBHost := stringOrDefault(*systemDBHost, *dbHost)
	resolvedSystemDBPort := intOrDefault(*systemDBPort, *dbPort)
	resolvedSystemDBName := stringOrDefault(*systemDBName, *dbName)
	resolvedSystemDBUserName := stringOrDefault(*systemDBUserName, *dbUserName)
	resolvedSystemDBPassword := stringOrDefault(*systemDBPassword, *dbPassword)
	resolvedSystemDBSSLMode := stringOrDefault(*systemDBSSLMode, *dbSSLMode)

	dataConnStr := fmt.Sprintf(
		"postgresql://%s:%d/%s?user=%s&password=%s&sslmode=%s",
		*dbHost, *dbPort, *dbName, *dbUserName, *dbPassword, *dbSSLMode,
	)
	systemConnStr := fmt.Sprintf(
		"postgresql://%s:%d/%s?user=%s&password=%s&sslmode=%s",
		resolvedSystemDBHost, resolvedSystemDBPort, resolvedSystemDBName,
		resolvedSystemDBUserName, resolvedSystemDBPassword, resolvedSystemDBSSLMode,
	)

	dataConn, err := db.ConnectWithRetry(dataConnStr, 5, 3*time.Second)
	if err != nil {
		return fmt.Errorf("failed to connect to data-plane database: %w", err)
	}
	defer func() {
		if closeErr := dataConn.Close(); closeErr != nil {
			slog.Error("failed to close data-plane database", "error", closeErr)
		}
	}()

	// Reuse the data-plane pool when both halves point at the same
	// physical database to avoid double the connections in the
	// colocated default. Comparing the resolved DSN means any one
	// of host/port/name/user/sslmode being different forks a second
	// pool.
	var systemConn *sql.DB
	if systemConnStr == dataConnStr {
		systemConn = dataConn
		slog.Info("meko_system schema colocated with data-plane database",
			"host", *dbHost, "port", *dbPort, "db", *dbName)
	} else {
		systemConn, err = db.ConnectWithRetry(systemConnStr, 5, 3*time.Second)
		if err != nil {
			return fmt.Errorf("failed to connect to meko_system database: %w", err)
		}
		defer func() {
			if closeErr := systemConn.Close(); closeErr != nil {
				slog.Error("failed to close meko_system database", "error", closeErr)
			}
		}()
		slog.Info("meko_system schema on dedicated database",
			"host", resolvedSystemDBHost, "port", resolvedSystemDBPort, "db", resolvedSystemDBName)
	}

	// Optional Langfuse Postgres connection — only attempted when --langfuse-db-host
	// (or LANGFUSE_DB_HOST) is set. Disabled cleanly otherwise; observability
	// endpoints that need it will return 503.
	var langfuseDBConn *sql.DB
	if *langfuseDBHost != "" {
		langfuseConnStr := fmt.Sprintf(
			"postgresql://%s:%d/%s?user=%s&password=%s&sslmode=%s",
			*langfuseDBHost, *langfuseDBPort, *langfuseDBName, *langfuseDBUser, *langfuseDBPassword, *langfuseDBSSLMode,
		)
		langfuseDBConn, err = db.ConnectWithRetry(langfuseConnStr, 3, 2*time.Second)
		if err != nil {
			slog.Warn("failed to connect to langfuse database; observability features disabled",
				"host", *langfuseDBHost, "error", err)
			langfuseDBConn = nil
		} else {
			slog.Info("langfuse database connected", "host", *langfuseDBHost, "db", *langfuseDBName)
			defer func() {
				if closeErr := langfuseDBConn.Close(); closeErr != nil {
					slog.Error("failed to close langfuse database", "error", closeErr)
				}
			}()
		}
	}

	scopingFlags := 0
	for _, f := range []bool{*onlySystemMigrate, *onlyDataMigrate, *onlyControlPlaneMigrate} {
		if f {
			scopingFlags++
		}
	}
	if scopingFlags > 1 {
		return fmt.Errorf("-only-system-migrate, -only-data-migrate and -only-controlplane-migrate are mutually exclusive")
	}

	if *migrateOnly || scopingFlags > 0 {
		// Any scoping flag selects a single migration set and implies a
		// migrate-and-exit run.
		migrationScope := db.AllMigrations
		switch {
		case *onlySystemMigrate:
			migrationScope.Data = false
		case *onlyDataMigrate:
			migrationScope.System = false
		case *onlyControlPlaneMigrate:
			migrationScope = db.ControlPlaneMigrations
		// A bare -migrate-only in control-plane mode means "migrate what this mode
		// needs". Defaulting to AllMigrations here would fail on plain Postgres and
		// pointlessly build the data-plane schema on YugabyteDB.
		case *controlPlaneMode:
			migrationScope = db.ControlPlaneMigrations
		}
		details, err := db.RunMigrations(systemConn, dataConn, migrationScope)
		if err != nil {
			return fmt.Errorf("migration failed: %w", err)
		}
		for _, d := range details {
			slog.Info("migration", "detail", d)
		}
		slog.Info("migrations complete, exiting")
		return nil
	}

	slog.Info("provisioning config", "crd_namespace", *crdNamespace)

	if *mcpInternalSecret == "" {
		slog.Warn("mcp internal validate endpoint disabled (no --mcp-internal-secret / MCP_INTERNAL_SECRET configured)")
	} else {
		slog.Info("mcp internal validate endpoint enabled")
	}

	// Default SES region to Cognito region when not explicitly set.
	resolvedSESRegion := *sesRegion
	if resolvedSESRegion == "" {
		resolvedSESRegion = *cognitoRegion
	}
	appURL := appURLForEnv(repository.ParseDeployEnv(*deployEnv))
	if *sesFromAddress == "" {
		slog.Warn("ses invite emails disabled (no --ses-from-address / SES_FROM_ADDRESS configured)")
	} else {
		slog.Info("ses invite emails enabled", "from", *sesFromAddress, "region", resolvedSESRegion, "app_url", appURL)
	}

	// The deprecated flags are parsed and discarded. They are still *accepted* because
	// both chart copies render them as args (api-server/_apiserver.tpl here, and the
	// meko-devops fork that yugabyte-cloud's meko-ycp-deploy.yaml installs), each
	// under an `if` on its apiServer.dataplane value — so removing them here would
	// crash-loop any deployment whose values set one, with "flag provided but not
	// defined". Nothing in this repo sets them any more; they go once no out-of-tree
	// values file does either. --dataplane-registry and --dataplane-image have no
	// chart path at all and are kept only so the set is retired in one step.
	if *controlPlaneMode && (*noBootstrapDefaults || *dataplaneRegistry != service.DefaultDataPlaneRegistry ||
		*dataplaneMCPImage != "" || *dataplanePullSecret != "" || *dataplaneSecretRef != "" ||
		len(dataplaneImages) > 0) {
		slog.Warn("--no-bootstrap-defaults and the --dataplane-* flags no longer do anything: " +
			"registration seeds no desired state, and a data plane's workloads come from its " +
			"chart. Safe to drop from your deployment.")
	}

	return api.Run(api.ServerConfig{
		DB:                                 dataConn,
		SystemDB:                           systemConn,
		Port:                               *port,
		CognitoRegion:                      *cognitoRegion,
		CognitoPoolID:                      *cognitoPoolID,
		CognitoClientID:                    *cognitoClientID,
		CognitoClientSecret:                *cognitoClientSecret,
		CognitoDomain:                      *cognitoDomain,
		AuthProvider:                       *authProvider,
		OIDCIssuerURL:                      *oidcIssuerURL,
		OIDCClientID:                       *oidcClientID,
		OIDCClientSecret:                   *oidcClientSecret,
		OIDCRedirectURL:                    *oidcRedirectURL,
		OIDCScopes:                         splitCommaList(*oidcScopes),
		CRDNamespace:                       *crdNamespace,
		LangfuseHost:                       *langfuseHost,
		LangfuseAdminAPIKey:                *langfuseAdminAPIKey,
		LangfuseDB:                         langfuseDBConn,
		OpenAIAPIKey:                       *openAIAPIKey,
		OpenAILLMModel:                     *openAILLMModel,
		OpenAIEmbeddingModel:               *openAIEmbeddingModel,
		MemoryListSkipVectorSort:           *memoryListSkipVectorSort,
		S3AuthMode:                         *s3AuthMode,
		S3Endpoint:                         *s3Endpoint,
		S3PublicEndpoint:                   *s3PublicEndpoint,
		S3AccessKey:                        *s3AccessKey,
		S3SecretKey:                        *s3SecretKey,
		S3Bucket:                           *s3Bucket,
		S3Region:                           *s3Region,
		MCPServerURL:                       mcpServerURLForEnv(repository.ParseDeployEnv(*deployEnv)),
		DeployEnv:                          repository.ParseDeployEnv(*deployEnv),
		MCPInternalSecret:                  *mcpInternalSecret,
		AdminAPIKey:                        *adminAPIKey,
		ConsoleEmbedAdminSecret:            *consoleEmbedAdminSecret,
		RequireAccountHeader:               *requireAccountHeader,
		MetricsURL:                         *metricsURL,
		BYOCMetricsURL:                     *byocMetricsURL,
		YBAAPIToken:                        *ybaAPIToken,
		MCPPATFlushInterval:                *mcpPATFlushInterval,
		PATRateLimitPerMin:                 *patRateLimitPerMin,
		PATRateLimitBurst:                  *patRateLimitBurst,
		SESRegion:                          resolvedSESRegion,
		SESFromAddress:                     *sesFromAddress,
		AppURL:                             appURL,
		StripeSecretKey:                    *stripeSecretKey,
		StripeWebhookSecret:                *stripeWebhookSecret,
		StripePriceIDStarter:               *stripePriceStarter,
		StripePriceIDGrowth:                *stripePriceGrowth,
		StripePriceIDScale:                 *stripePriceScale,
		MaxArtifactSizeBytes:               *maxArtifactSizeBytes,
		ShowPGAMP:                          *showPGAMP,
		EnableConversationSearch:           *enableConversationSearch,
		BYOCMode:                           *enableBYOC,
		EnableCollections:                  *enableCollections,
		ControlPlaneMode:                   *controlPlaneMode,
		BasePath:                           *basePath,
		AWSMarketplaceProductCode:          *awsMarketplaceProductCode,
		AWSMarketplaceRegion:               *awsMarketplaceRegion,
		AWSMarketplaceWelcomeURL:           *awsMarketplaceWelcomeURL,
		AWSMarketplaceRoleARN:              *awsMarketplaceRoleARN,
		DeploymentsRegion:                  *deploymentsRegion,
		LabsAgentImagesRepo:                *labsAgentImagesRepo,
		DeploymentsManagedExecutionRoleARN: *deploymentsManagedExecutionRoleARN,
		DeploymentsMekoAWSAccountID:        *deploymentsMekoAWSAccountID,
		LabsAgentKAdminEmail:               *labsAgentKAdminEmail,
	})
}

func main() {
	if err := run(); err != nil {
		slog.Error("fatal", "error", err)
		os.Exit(1)
	}
}

// imageOverride is one -dataplane-image component=image pair.
type imageOverride struct{ component, image string }

// imageOverrides collects repeated -dataplane-image flags.
//
// One repeatable flag rather than one flag per component: the release has six
// components and will grow, and six flags plus six env vars plus six chart values is a
// lot of surface for something almost nobody sets — the pinned release is the answer
// for every deployment that is not testing a single new image.
type imageOverrides []imageOverride

func (o *imageOverrides) String() string {
	parts := make([]string, 0, len(*o))
	for _, v := range *o {
		parts = append(parts, v.component+"="+v.image)
	}
	return strings.Join(parts, ",")
}

func (o *imageOverrides) Set(v string) error {
	component, image, ok := strings.Cut(v, "=")
	component, image = strings.TrimSpace(component), strings.TrimSpace(image)
	if !ok || component == "" || image == "" {
		return fmt.Errorf("want component=image, got %q", v)
	}
	*o = append(*o, imageOverride{component, image})
	return nil
}

// splitCommaList turns a comma-separated flag into a trimmed slice, dropping
// empties so a trailing comma or a blank value yields nil rather than a scope
// the issuer would reject.
func splitCommaList(v string) []string {
	var out []string
	for _, p := range strings.Split(v, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func envOrDefault(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func envIntOrDefault(key string, fallback int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return fallback
}

func envInt64OrDefault(key string, fallback int64) int64 {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil && n > 0 {
			return n
		}
	}
	return fallback
}

// stringOrDefault returns v if non-empty, otherwise fallback. Used to
// inherit --system-db-* settings from --db-* when the operator doesn't
// override them.
func stringOrDefault(v, fallback string) string {
	if v == "" {
		return fallback
	}
	return v
}

// intOrDefault returns v if non-zero, otherwise fallback. Mirrors
// stringOrDefault for the --system-db-port flag, where 0 means "unset".
func intOrDefault(v, fallback int) int {
	if v == 0 {
		return fallback
	}
	return v
}

func envBoolOrDefault(key string, fallback bool) bool {
	if v := os.Getenv(key); v != "" {
		if b, err := strconv.ParseBool(v); err == nil {
			return b
		}
	}
	return fallback
}

const (
	mcpServerURLProd = "https://mcp.mekodata.ai/mcp"
	mcpServerURLDev  = "https://mcp.mekodev.com/mcp"

	appURLProd = "https://cloud.mekodata.ai"
	appURLDev  = "https://app.mekodev.com"
)

func mcpServerURLForEnv(env repository.DeployEnv) string {
	if env == repository.DeployEnvDev {
		return mcpServerURLDev
	}
	return mcpServerURLProd
}

func appURLForEnv(env repository.DeployEnv) string {
	if env == repository.DeployEnvDev {
		return appURLDev
	}
	return appURLProd
}

// parseLogLevel maps a CLI/env string to a slog.Level. Unknown
// values fall back to Info with a warn log so a typo never silences
// the process or crashes boot.
func parseLogLevel(raw string) slog.Level {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "debug":
		return slog.LevelDebug
	case "info", "":
		return slog.LevelInfo
	case "warn", "warning":
		return slog.LevelWarn
	case "error", "err":
		return slog.LevelError
	default:
		slog.Warn("unknown log level, defaulting to info",
			"requested", raw)
		return slog.LevelInfo
	}
}

// envDurationOrDefault parses the named env var as a Go duration
// (e.g. "30s", "2m", "500ms"). On parse error we log and fall back
// to the default rather than crashing the process -- a bad value in
// a deployment manifest should not block boot.
func envDurationOrDefault(key string, fallback time.Duration) time.Duration {
	v := os.Getenv(key)
	if v == "" {
		return fallback
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		//nolint:gosec // G706: key/value are operator-supplied env config, not attacker input
		slog.Warn("invalid duration env var, using default",
			"key", key, "value", v, "error", err, "default", fallback)
		return fallback
	}
	return d
}
