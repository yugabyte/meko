package repository

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/lib/pq"

	"meko/api_server/internal/models"
)

// MCPPATRepo encapsulates database operations for personal access
// tokens issued to users for the MCP server.
//
// Backing table: meko_system.mcp_personal_access_tokens.
//
// All read paths return either a populated *MCPPersonalAccessToken
// or (nil, nil). Errors are reserved for actual storage failures.
type MCPPATRepo struct {
	db *sql.DB

	// touchMu guards the touchPending map.
	touchMu      sync.Mutex
	touchPending map[string]time.Time
}

// MCPPATCreateParams holds the inputs for inserting a new PAT row.
//
// All columns are required by the storage layer; ID is generated
// server-side via gen_random_uuid().
//
// ExpiresAt is mandatory (the column is NOT NULL since migration
// 000004). Service layer rejects zero / past values before reaching
// the repo.
//
// Scopes is optional at the param level: when nil/empty the column
// default ARRAY['*'] is used, giving full access. The service layer
// validates non-empty scope sets against the allowlist before insert.
type MCPPATCreateParams struct {
	UserID      string
	Name        string
	TokenPrefix string
	TokenHash   string
	ExpiresAt   time.Time
	Scopes      []string
}

// ErrMCPPATNameConflict is returned when a duplicate (user_id, name)
// PAT is requested. Matches the application-level uniqueness check
// performed by the service before insert; defence-in-depth in case of
// a race.
var ErrMCPPATNameConflict = errors.New("API Token: name already exists for user")

// NewMCPPATRepo creates a new MCPPATRepo.
func NewMCPPATRepo(db *sql.DB) *MCPPATRepo {
	return &MCPPATRepo{
		db:           db,
		touchPending: make(map[string]time.Time),
	}
}

// Create inserts a new PAT row and returns the persisted record.
//
// The plaintext token is intentionally not part of the input -- the
// caller hashes it once and discards the plaintext. Repo never sees
// it, so it cannot leak through logs or stack traces.
func (r *MCPPATRepo) Create(
	ctx context.Context,
	p MCPPATCreateParams,
) (*models.MCPPersonalAccessToken, error) {
	slog.Info("creating meko API token",
		"user_id", p.UserID,
		"name", p.Name,
		"token_prefix", p.TokenPrefix,
		"expires_at", p.ExpiresAt,
		"scopes", p.Scopes,
	)

	// Empty scopes -> let the column default (ARRAY['*']) take effect.
	// pq.Array on a nil slice would write '{}' instead, which would be
	// a token with no permissions at all -- not what callers expect.
	var scopesArg interface{}
	if len(p.Scopes) > 0 {
		scopesArg = pq.Array(p.Scopes)
	} else {
		scopesArg = nil
	}

	var rec models.MCPPersonalAccessToken
	err := r.db.QueryRowContext(ctx,
		`INSERT INTO meko_system.mcp_personal_access_tokens
			(user_id, name, token_prefix, token_hash, expires_at, scopes)
		 VALUES ($1, $2, $3, $4, $5, COALESCE($6, ARRAY['*']::TEXT[]))
		 RETURNING id, user_id, name, token_prefix,
			created_at, last_used_at, expires_at, revoked_at, scopes`,
		p.UserID, p.Name, p.TokenPrefix, p.TokenHash, p.ExpiresAt, scopesArg,
	).Scan(
		&rec.ID, &rec.UserID, &rec.Name, &rec.TokenPrefix,
		&rec.CreatedAt, &rec.LastUsedAt, &rec.ExpiresAt, &rec.RevokedAt,
		pq.Array(&rec.Scopes),
	)
	if err != nil {
		var pqErr *pq.Error
		if errors.As(err, &pqErr) && pqErr.Code == "23505" {
			slog.Warn("mcp pat insert hit unique violation",
				"user_id", p.UserID, "constraint", pqErr.Constraint,
			)
			return nil, fmt.Errorf("%w: user_id=%s name=%s",
				ErrMCPPATNameConflict, p.UserID, p.Name)
		}
		slog.Error("failed to insert mcp pat",
			"user_id", p.UserID, "error", err,
		)
		return nil, fmt.Errorf("mcp pat: insert: %w", err)
	}
	return &rec, nil
}

// ListByUser returns every PAT row owned by userID, newest first.
//
// Includes revoked / expired rows so the UI can render history; the
// caller decides how to surface them.
func (r *MCPPATRepo) ListByUser(
	ctx context.Context,
	userID string,
) ([]models.MCPPersonalAccessToken, error) {
	rows, err := r.db.QueryContext(ctx,
		`SELECT id, user_id, name, token_prefix,
			created_at, last_used_at, expires_at, revoked_at, scopes
		 FROM meko_system.mcp_personal_access_tokens
		 WHERE user_id = $1
		 ORDER BY created_at DESC`,
		userID,
	)
	if err != nil {
		slog.Error("failed to list mcp pats",
			"user_id", userID, "error", err,
		)
		return nil, fmt.Errorf("mcp pat: list: %w", err)
	}
	defer func() {
		if cErr := rows.Close(); cErr != nil {
			slog.Warn("mcp pat list: rows.Close",
				"user_id", userID, "error", cErr)
		}
	}()

	var out []models.MCPPersonalAccessToken
	for rows.Next() {
		var t models.MCPPersonalAccessToken
		if err := rows.Scan(
			&t.ID, &t.UserID, &t.Name, &t.TokenPrefix,
			&t.CreatedAt, &t.LastUsedAt, &t.ExpiresAt, &t.RevokedAt,
			pq.Array(&t.Scopes),
		); err != nil {
			return nil, fmt.Errorf("mcp pat: scan: %w", err)
		}
		out = append(out, t)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("mcp pat: list rows: %w", err)
	}
	return out, nil
}

// FindByHash returns the PAT row whose token_hash equals tokenHash,
// regardless of revoked / expired state. The caller (typically the
// validation service) is expected to inspect RevokedAt / ExpiresAt
// (or call IsActive) to classify the result.
//
// This is the authentication hot-path called by the MCP server on
// every request; it performs a single-index lookup via
// mcp_pats_token_hash_idx.
//
// Returns (nil, nil) when no row matches.
func (r *MCPPATRepo) FindByHash(
	ctx context.Context,
	tokenHash string,
) (*models.MCPPersonalAccessToken, error) {
	var t models.MCPPersonalAccessToken
	err := r.db.QueryRowContext(ctx,
		`SELECT id, user_id, name, token_prefix,
			created_at, last_used_at, expires_at, revoked_at, scopes
		 FROM meko_system.mcp_personal_access_tokens
		 WHERE token_hash = $1`,
		tokenHash,
	).Scan(
		&t.ID, &t.UserID, &t.Name, &t.TokenPrefix,
		&t.CreatedAt, &t.LastUsedAt, &t.ExpiresAt, &t.RevokedAt,
		pq.Array(&t.Scopes),
	)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		slog.Error("mcp pat lookup by hash failed", "error", err)
		return nil, fmt.Errorf("mcp pat: find by hash: %w", err)
	}
	return &t, nil
}

// FindByID returns the PAT identified by (userID, id). The composite
// primary key means this is a single-tablet exact-key lookup and
// implicitly enforces cross-user safety: a token belonging to a
// different user is invisible.
func (r *MCPPATRepo) FindByID(
	ctx context.Context,
	userID, id string,
) (*models.MCPPersonalAccessToken, error) {
	var t models.MCPPersonalAccessToken
	err := r.db.QueryRowContext(ctx,
		`SELECT id, user_id, name, token_prefix,
			created_at, last_used_at, expires_at, revoked_at, scopes
		 FROM meko_system.mcp_personal_access_tokens
		 WHERE user_id = $1 AND id = $2`,
		userID, id,
	).Scan(
		&t.ID, &t.UserID, &t.Name, &t.TokenPrefix,
		&t.CreatedAt, &t.LastUsedAt, &t.ExpiresAt, &t.RevokedAt,
		pq.Array(&t.Scopes),
	)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		slog.Error("mcp pat find by id failed",
			"user_id", userID, "id", id, "error", err,
		)
		return nil, fmt.Errorf("mcp pat: find by id: %w", err)
	}
	return &t, nil
}

// Revoke stamps revoked_at = NOW() for the (userID, id) PAT. The
// composite PK makes the WHERE clause a single-tablet hit and
// prevents cross-user revocation.
//
// Returns true when a row was actually updated; false when no row
// matched OR when the row was already revoked (idempotent).
func (r *MCPPATRepo) Revoke(
	ctx context.Context,
	userID, id string,
) (bool, error) {
	res, err := r.db.ExecContext(ctx,
		`UPDATE meko_system.mcp_personal_access_tokens
		 SET revoked_at = NOW()
		 WHERE user_id = $1 AND id = $2 AND revoked_at IS NULL`,
		userID, id,
	)
	if err != nil {
		slog.Error("mcp pat revoke failed",
			"user_id", userID, "id", id, "error", err,
		)
		return false, fmt.Errorf("mcp pat: revoke: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("mcp pat: revoke rows affected: %w", err)
	}
	if n > 0 {
		slog.Info("mcp pat revoked", "user_id", userID, "id", id)
	}
	return n > 0, nil
}

// CountActiveByUser returns the number of non-revoked, non-expired
// PATs owned by userID. Used by the service to enforce the per-user
// token cap before issuing a new one.
func (r *MCPPATRepo) CountActiveByUser(
	ctx context.Context,
	userID string,
) (int, error) {
	var n int
	err := r.db.QueryRowContext(ctx,
		`SELECT COUNT(*)
		 FROM meko_system.mcp_personal_access_tokens
		 WHERE user_id = $1
		   AND revoked_at IS NULL
		   AND expires_at > NOW()`,
		userID,
	).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("mcp pat: count active: %w", err)
	}
	return n, nil
}

// NameExistsForUser reports whether userID already has a non-revoked
// PAT with the given name. Used by the service for app-level
// uniqueness before insert. A race against this check still cannot
// produce a duplicate because the (user_id, name) constraint is
// enforced at the service layer + a 23505 fallback in Create.
func (r *MCPPATRepo) NameExistsForUser(
	ctx context.Context,
	userID, name string,
) (bool, error) {
	var one int
	err := r.db.QueryRowContext(ctx,
		`SELECT 1
		 FROM meko_system.mcp_personal_access_tokens
		 WHERE user_id = $1 AND name = $2 AND revoked_at IS NULL
		 LIMIT 1`,
		userID, name,
	).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("mcp pat: name exists: %w", err)
	}
	return true, nil
}

// QueueLastUsedTouch records that tokenID was used at `at`. The
// update is NOT issued synchronously -- it is coalesced in memory and
// flushed in batches by FlushLastUsedTouches.
//
// Rationale: every MCP tool call would otherwise produce one UPDATE,
// turning last_used_at into a write-amplification hot-spot on what is
// otherwise a read-mostly table. Coalescing per-token to a single
// "latest seen" timestamp lets the flusher emit one row per active
// token per flush window.
func (r *MCPPATRepo) QueueLastUsedTouch(tokenID string, at time.Time) {
	if tokenID == "" {
		return
	}
	r.touchMu.Lock()
	defer r.touchMu.Unlock()
	if existing, ok := r.touchPending[tokenID]; ok && existing.After(at) {
		return
	}
	r.touchPending[tokenID] = at
}

// FlushLastUsedTouches drains the in-memory touch map and writes one
// UPDATE per pending token. The caller (typically a background
// goroutine on a tick) is responsible for scheduling.
//
// The update predicate guards against decreasing last_used_at and
// against touching revoked rows (so a leaked token still being used
// after revocation does not silently keep last_used_at fresh).
//
// Returns the number of tokens flushed (rows touched, not the count
// of UPDATE statements -- failed updates are skipped and logged).
func (r *MCPPATRepo) FlushLastUsedTouches(ctx context.Context) (int, error) {
	r.touchMu.Lock()
	pending := r.touchPending
	r.touchPending = make(map[string]time.Time, len(pending))
	r.touchMu.Unlock()

	if len(pending) == 0 {
		return 0, nil
	}

	flushed := 0
	for id, at := range pending {
		res, err := r.db.ExecContext(ctx,
			`UPDATE meko_system.mcp_personal_access_tokens
			 SET last_used_at = $1
			 WHERE id = $2
			   AND revoked_at IS NULL
			   AND (last_used_at IS NULL OR last_used_at < $1)`,
			at, id,
		)
		if err != nil {
			slog.Warn("mcp pat last_used flush: update failed",
				"token_id", id, "error", err)
			continue
		}
		n, _ := res.RowsAffected()
		flushed += int(n)
	}
	slog.Debug("mcp pat last_used flushed",
		"queued", len(pending), "rows_updated", flushed,
	)
	return flushed, nil
}

// PendingTouchCount returns the number of tokens currently buffered
// in the in-memory touch map. Exposed for instrumentation / tests.
func (r *MCPPATRepo) PendingTouchCount() int {
	r.touchMu.Lock()
	defer r.touchMu.Unlock()
	return len(r.touchPending)
}
