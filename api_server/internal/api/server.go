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
	S3Region                 string
	S3SecretKey              string
	MCPServerURL             string
	DeployEnv                repository.DeployEnv
	// MCPInternalSecret gates the internal MCP token validation
	// endpoint (POST /api/v1/internal/meko-tokens/validate). When
	// empty, the endpoint returns 503.
	MCPInternalSecret string

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
}

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
			return err
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	slog.Info("shutting down api server")
	if err := e.Shutdown(ctx); err != nil {
		flusherCancel()
		<-flusherDone
		return fmt.Errorf("server shutdown error: %w", err)
	}

	flusherCancel()
	<-flusherDone

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
