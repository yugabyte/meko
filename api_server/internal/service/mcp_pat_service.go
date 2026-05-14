package service

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"meko/api_server/internal/auth"
	"meko/api_server/internal/models"
	"meko/api_server/internal/repository"
)

// MCP PAT service-layer constants. The defaults are tuned for the
// self-service flow:
//
//   - MaxActivePATsPerUser=10 keeps a sane upper bound on how many
//     bearers a single user can have in flight (cursor + claude desktop
//   - a couple of CI machines is the realistic ceiling).
//   - DefaultTTLDays=90 mirrors the GitHub PAT default; long enough to
//     not be annoying, short enough that a forgotten token rotates out
//     within a quarter.
//   - MaxTTLDays=365 caps the worst case; requests above this are
//     clamped (and logged at INFO so we can see how often it happens).
//   - MinTTLDays=1 prevents accidental ttl_days=0 from turning into
//     "expires NOW".
//   - DefaultAuditRateLimit caps how often the validator emits an
//     audit row for the same (token_id, action) pair. Without this,
//     a leaked-and-revoked token being reused in a tight loop could
//     blow the audit table up. 5 minutes is short enough for forensic
//     work, long enough to dampen attack noise.
const (
	MaxActivePATsPerUser  = 10
	DefaultTTLDays        = 90
	MinTTLDays            = 1
	MaxTTLDays            = 365
	MaxPATNameLen         = 100
	DefaultAuditRateLimit = 5 * time.Minute
)

// PAT scope vocabulary. Day-one set is intentionally tiny:
//
//   - ScopeAll ("*") -- full access, equivalent to a Cognito session.
//   - ScopeRead    -- reads only; gated routes return 403 on write.
//   - ScopeWrite   -- writes (mutations).
//
// Anything outside this set is rejected at create time. Future
// granular scopes (e.g. "datapack:read:<id>") get added here without
// any schema change because the storage column is TEXT[].
const (
	ScopeAll   = "*"
	ScopeRead  = "read"
	ScopeWrite = "write"
)

// allowedScopes is the runtime allowlist enforced by normalizeScopes.
// Keep in sync with the constants above.
var allowedScopes = map[string]struct{}{
	ScopeAll:   {},
	ScopeRead:  {},
	ScopeWrite: {},
}

// DefaultScopes is what an empty / nil request resolves to. Equivalent
// to the column default in the migration; mirrored here so the service
// layer never has to round-trip through the DB to know what was stored.
var DefaultScopes = []string{ScopeAll}

// Sentinel errors returned by the service. Mapped to HTTP statuses by
// the handler.
var (
	// ErrMCPPATValidation is returned for any invalid input from the
	// caller (empty name, name too long, etc.).
	ErrMCPPATValidation = errors.New("mcp pat: validation failed")

	// ErrMCPPATCapReached is returned when the user already owns
	// MaxActivePATsPerUser active tokens.
	ErrMCPPATCapReached = errors.New("mcp pat: token cap reached")

	// ErrMCPPATNameInUse is returned when a non-revoked PAT with the
	// requested name already exists for the user.
	ErrMCPPATNameInUse = errors.New("mcp pat: name in use")

	// ErrMCPPATNotFound is returned when revoke / lookup target does
	// not belong to the caller.
	ErrMCPPATNotFound = errors.New("mcp pat: not found")
)

// MCPPATService encapsulates business rules for personal access
// tokens.
//
// The service owns:
//   - input validation (name shape, ttl bounds)
//   - per-user uniqueness check (defence-in-depth alongside the repo
//     unique-violation translation)
//   - per-user active-token cap
//   - PAT generation + hashing handoff to the auth package
//   - best-effort audit log entry on create / revoke
//   - validation hot-path called by the MCP server (ValidatePAT),
//     including rate-limited audit writes for expired_used /
//     revoked_used and last_used_at coalescing
type MCPPATService struct {
	repo  *repository.MCPPATRepo
	audit *repository.MCPPATAuditRepo

	// Audit rate-limit state. Keyed by (token_id, action) so that
	// expired_used and revoked_used are tracked independently and
	// different tokens never share a quota.
	auditMu     sync.Mutex
	auditLastAt map[auditKey]time.Time
	auditWindow time.Duration

	// nowFn is overrideable for deterministic time in tests.
	nowFn func() time.Time
}

type auditKey struct {
	tokenID string
	action  models.MCPPATAuditAction
}

// NewMCPPATService creates a new MCPPATService. The audit repo is
// optional: when nil, audit writes are skipped (useful for tests).
func NewMCPPATService(
	repo *repository.MCPPATRepo,
	audit *repository.MCPPATAuditRepo,
) *MCPPATService {
	return &MCPPATService{
		repo:        repo,
		audit:       audit,
		auditLastAt: make(map[auditKey]time.Time),
		auditWindow: DefaultAuditRateLimit,
		nowFn:       func() time.Time { return time.Now().UTC() },
	}
}

// MCPPATCreateInput is the service-layer input for creating a PAT.
//
// ActorIP / ActorUserAgent are best-effort and used only for the
// audit log row.
//
// Scopes is the requested permission set for the new token. nil/empty
// is normalised to {"*"} (full access). Each entry must be in the
// service-layer scope allowlist; otherwise CreatePAT rejects with
// ErrMCPPATValidation.
type MCPPATCreateInput struct {
	UserID         string
	Name           string
	TTLDays        int
	Scopes         []string
	ActorIP        string
	ActorUserAgent string
}

// CreatePAT validates the input, mints a new token, persists the
// row, and writes a "created" audit entry. The plaintext token is
// returned exactly once in the response and is not stored anywhere.
func (s *MCPPATService) CreatePAT(
	ctx context.Context,
	in MCPPATCreateInput,
) (*models.CreateMCPTokenResponse, error) {
	name, err := normalizeName(in.Name)
	if err != nil {
		return nil, err
	}
	if in.UserID == "" {
		return nil, fmt.Errorf("%w: user_id is required", ErrMCPPATValidation)
	}

	scopes, err := normalizeScopes(in.Scopes)
	if err != nil {
		return nil, err
	}

	ttl := clampTTLDays(in.TTLDays)

	count, err := s.repo.CountActiveByUser(ctx, in.UserID)
	if err != nil {
		return nil, fmt.Errorf("counting active mcp pats: %w", err)
	}
	if count >= MaxActivePATsPerUser {
		slog.Warn("mcp pat cap reached",
			"user_id", in.UserID, "active", count, "cap", MaxActivePATsPerUser,
		)
		return nil, fmt.Errorf("%w: %d active tokens (max %d)",
			ErrMCPPATCapReached, count, MaxActivePATsPerUser)
	}

	exists, err := s.repo.NameExistsForUser(ctx, in.UserID, name)
	if err != nil {
		return nil, fmt.Errorf("checking pat name uniqueness: %w", err)
	}
	if exists {
		return nil, fmt.Errorf("%w: %q", ErrMCPPATNameInUse, name)
	}

	plaintext, prefix, hash, err := auth.GeneratePAT()
	if err != nil {
		return nil, fmt.Errorf("generating pat: %w", err)
	}

	expires := s.nowFn().AddDate(0, 0, ttl)

	// Defence-in-depth against a future code path that bypasses
	// clampTTLDays. Storage is NOT NULL since migration 000004; an
	// "expires now" token would be useless from the moment it's
	// minted, and a zero-value timestamp would surprise downstream
	// callers. Reject explicitly.
	if expires.IsZero() || !expires.After(s.nowFn()) {
		return nil, fmt.Errorf("%w: computed expires_at is not in the future",
			ErrMCPPATValidation)
	}

	rec, err := s.repo.Create(ctx, repository.MCPPATCreateParams{
		UserID:      in.UserID,
		Name:        name,
		TokenPrefix: prefix,
		TokenHash:   hash,
		ExpiresAt:   expires,
		Scopes:      scopes,
	})
	if err != nil {
		if errors.Is(err, repository.ErrMCPPATNameConflict) {
			return nil, fmt.Errorf("%w: %q", ErrMCPPATNameInUse, name)
		}
		return nil, fmt.Errorf("inserting pat: %w", err)
	}

	s.appendAudit(ctx, repository.MCPPATAuditAppendParams{
		TokenID:        rec.ID,
		UserID:         rec.UserID,
		Action:         models.MCPPATAuditCreated,
		ActorID:        rec.UserID,
		ActorType:      models.MCPPATActorUser,
		ActorIP:        in.ActorIP,
		ActorAppClient: in.ActorUserAgent,
		Metadata: map[string]any{
			"ttl_days":     ttl,
			"name":         rec.Name,
			"token_prefix": rec.TokenPrefix,
			"scopes":       rec.Scopes,
		},
	})

	slog.Info("mcp pat created",
		"user_id", rec.UserID,
		"id", rec.ID,
		"token_prefix", rec.TokenPrefix,
		"ttl_days", ttl,
		"scopes", rec.Scopes,
	)

	return &models.CreateMCPTokenResponse{
		Token:       plaintext,
		TokenRecord: *rec,
	}, nil
}

// ListPATs returns every PAT owned by userID, newest first.
func (s *MCPPATService) ListPATs(
	ctx context.Context,
	userID string,
) ([]models.MCPPersonalAccessToken, error) {
	if userID == "" {
		return nil, fmt.Errorf("%w: user_id is required", ErrMCPPATValidation)
	}
	return s.repo.ListByUser(ctx, userID)
}

// MCPPATRevokeInput is the service-layer input for revoking a PAT.
type MCPPATRevokeInput struct {
	UserID         string
	TokenID        string
	ActorIP        string
	ActorUserAgent string
}

// RevokePAT marks the token revoked and writes a "revoked" audit
// entry. Returns ErrMCPPATNotFound when the token does not belong to
// the caller (or has already been revoked and is not present in any
// active form).
func (s *MCPPATService) RevokePAT(
	ctx context.Context,
	in MCPPATRevokeInput,
) error {
	if in.UserID == "" || in.TokenID == "" {
		return fmt.Errorf("%w: user_id and token_id are required",
			ErrMCPPATValidation)
	}

	// Pre-check existence so we can disambiguate "already revoked"
	// (idempotent success) from "wrong owner" (404). The composite PK
	// makes this a single-tablet hit.
	rec, err := s.repo.FindByID(ctx, in.UserID, in.TokenID)
	if err != nil {
		return fmt.Errorf("looking up pat for revoke: %w", err)
	}
	if rec == nil {
		return fmt.Errorf("%w: id=%s", ErrMCPPATNotFound, in.TokenID)
	}

	updated, err := s.repo.Revoke(ctx, in.UserID, in.TokenID)
	if err != nil {
		return fmt.Errorf("revoking pat: %w", err)
	}

	if !updated {
		// Already revoked; treat as success but skip the audit row to
		// avoid double-logging.
		slog.Info("mcp pat revoke is a no-op (already revoked)",
			"user_id", in.UserID, "id", in.TokenID,
		)
		return nil
	}

	s.appendAudit(ctx, repository.MCPPATAuditAppendParams{
		TokenID:        rec.ID,
		UserID:         rec.UserID,
		Action:         models.MCPPATAuditRevoked,
		ActorID:        rec.UserID,
		ActorType:      models.MCPPATActorUser,
		ActorIP:        in.ActorIP,
		ActorAppClient: in.ActorUserAgent,
		Metadata: map[string]any{
			"name":         rec.Name,
			"token_prefix": rec.TokenPrefix,
		},
	})
	return nil
}

// appendAudit writes one audit row best-effort. Errors are logged
// and dropped because the underlying user-facing action has already
// committed.
func (s *MCPPATService) appendAudit(
	ctx context.Context,
	p repository.MCPPATAuditAppendParams,
) {
	if s.audit == nil {
		return
	}
	if _, err := s.audit.Append(ctx, p); err != nil {
		slog.Warn("mcp pat audit append failed (best-effort)",
			"token_id", p.TokenID,
			"action", p.Action,
			"error", err,
		)
	}
}

// MCPPATValidateInput is the service-layer input for ValidatePAT.
//
// Plaintext is the raw bearer the MCP server received from the client
// (Authorization header). ActorIP / ActorUserAgent are best-effort
// metadata for the audit row, taken from the MCP server's view of the
// caller (forwarded via the validation request body / HTTP headers).
type MCPPATValidateInput struct {
	Plaintext      string
	ActorIP        string
	ActorUserAgent string
}

// ValidatePAT is the authentication hot-path called by the MCP
// server on every inbound tool call. It returns a populated
// *models.ValidatePATResponse rather than an error so the handler
// can always respond 200 with a structured verdict; errors are
// reserved for storage failures the caller should surface as 500.
//
// Side effects:
//   - On a valid token, queues a last_used_at touch (debounced; the
//     api_server flusher goroutine emits the actual UPDATE).
//   - On an expired or revoked match, writes a rate-limited audit
//     row (one per (token_id, action) per auditWindow). Unknown
//     tokens never produce an audit row -- there is no row to
//     attribute it to and an attacker spraying random bytes could
//     otherwise saturate the audit table.
func (s *MCPPATService) ValidatePAT(
	ctx context.Context,
	in MCPPATValidateInput,
) (*models.ValidatePATResponse, error) {
	plaintext := strings.TrimSpace(in.Plaintext)
	if plaintext == "" || !auth.IsPATFormat(plaintext) {
		return &models.ValidatePATResponse{
			Valid:  false,
			Reason: models.MCPPATValidationUnknown,
		}, nil
	}

	hash := auth.HashPAT(plaintext)
	rec, err := s.repo.FindByHash(ctx, hash)
	if err != nil {
		return nil, fmt.Errorf("looking up pat: %w", err)
	}
	if rec == nil {
		return &models.ValidatePATResponse{
			Valid:  false,
			Reason: models.MCPPATValidationUnknown,
		}, nil
	}

	now := s.nowFn()

	if rec.IsActive(now) {
		s.repo.QueueLastUsedTouch(rec.ID, now)
		exp := rec.ExpiresAt
		return &models.ValidatePATResponse{
			Valid:     true,
			TokenID:   rec.ID,
			UserID:    rec.UserID,
			ExpiresAt: &exp,
			Scopes:    rec.Scopes,
		}, nil
	}

	// Inactive: classify and emit a rate-limited audit row.
	var (
		reason models.MCPPATValidationReason
		action models.MCPPATAuditAction
	)
	if rec.RevokedAt != nil {
		reason = models.MCPPATValidationRevoked
		action = models.MCPPATAuditRevokedUsed
	} else {
		reason = models.MCPPATValidationExpired
		action = models.MCPPATAuditExpiredUsed
	}

	if s.shouldEmitAudit(rec.ID, action, now) {
		s.appendAudit(ctx, repository.MCPPATAuditAppendParams{
			TokenID:        rec.ID,
			UserID:         rec.UserID,
			Action:         action,
			ActorID:        rec.UserID,
			ActorType:      models.MCPPATActorSystem,
			ActorIP:        in.ActorIP,
			ActorAppClient: in.ActorUserAgent,
			Metadata: map[string]any{
				"name":         rec.Name,
				"token_prefix": rec.TokenPrefix,
				"source":       "mcp_validate",
			},
		})
	}

	slog.Info("mcp pat validation rejected",
		"token_id", rec.ID,
		"user_id", rec.UserID,
		"reason", reason,
	)

	return &models.ValidatePATResponse{
		Valid:  false,
		Reason: reason,
	}, nil
}

// shouldEmitAudit returns true if (tokenID, action) has not been
// audited within the configured window. On true, it stamps the new
// emission time; on false, it leaves the existing entry alone so the
// window is anchored to the first hit, not refreshed by every retry.
func (s *MCPPATService) shouldEmitAudit(
	tokenID string,
	action models.MCPPATAuditAction,
	now time.Time,
) bool {
	if s.audit == nil {
		return false
	}
	key := auditKey{tokenID: tokenID, action: action}
	s.auditMu.Lock()
	defer s.auditMu.Unlock()
	if last, ok := s.auditLastAt[key]; ok && now.Sub(last) < s.auditWindow {
		return false
	}
	s.auditLastAt[key] = now
	return true
}

// normalizeName trims whitespace and validates length / charset.
//
// Allowed: printable Unicode, length 1..MaxPATNameLen runes after
// trimming. Disallowed: control characters (would corrupt logs and
// audit reports).
func normalizeName(raw string) (string, error) {
	name := strings.TrimSpace(raw)
	if name == "" {
		return "", fmt.Errorf("%w: name is required", ErrMCPPATValidation)
	}
	if utf8.RuneCountInString(name) > MaxPATNameLen {
		return "", fmt.Errorf("%w: name must be <= %d characters",
			ErrMCPPATValidation, MaxPATNameLen)
	}
	for _, r := range name {
		if r < 0x20 || r == 0x7f {
			return "", fmt.Errorf("%w: name contains control characters",
				ErrMCPPATValidation)
		}
	}
	return name, nil
}

// clampTTLDays applies default + min/max bounds to caller-supplied
// TTLs. Out-of-range requests are clamped silently (logged at INFO)
// rather than rejected, since the caller's intent ("a long-lived
// token") is preserved.
func clampTTLDays(req int) int {
	if req <= 0 {
		return DefaultTTLDays
	}
	if req < MinTTLDays {
		slog.Info("mcp pat ttl clamped up to minimum",
			"requested", req, "min", MinTTLDays,
		)
		return MinTTLDays
	}
	if req > MaxTTLDays {
		slog.Info("mcp pat ttl clamped down to maximum",
			"requested", req, "max", MaxTTLDays,
		)
		return MaxTTLDays
	}
	return req
}

// normalizeScopes resolves a caller-supplied scope set:
//
//   - nil / empty -> DefaultScopes ({"*"}).
//   - duplicates -> deduplicated, order preserved.
//   - any entry not in allowedScopes -> ErrMCPPATValidation.
//
// Returns the canonical slice that should be persisted to the
// scopes column.
func normalizeScopes(raw []string) ([]string, error) {
	if len(raw) == 0 {
		out := make([]string, len(DefaultScopes))
		copy(out, DefaultScopes)
		return out, nil
	}
	seen := make(map[string]struct{}, len(raw))
	out := make([]string, 0, len(raw))
	for _, s := range raw {
		s = strings.TrimSpace(s)
		if s == "" {
			return nil, fmt.Errorf("%w: scope entry must not be empty",
				ErrMCPPATValidation)
		}
		if _, ok := allowedScopes[s]; !ok {
			return nil, fmt.Errorf("%w: unknown scope %q (allowed: %s)",
				ErrMCPPATValidation, s, allowedScopeList())
		}
		if _, dup := seen[s]; dup {
			continue
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	return out, nil
}

// allowedScopeList returns the allowed scope vocabulary as a stable,
// human-readable string for error messages.
func allowedScopeList() string {
	// Hand-rolled to keep deterministic order for error message stability.
	return ScopeAll + ", " + ScopeRead + ", " + ScopeWrite
}
