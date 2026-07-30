package repository

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"

	"meko/api_server/internal/models"
)

// MCPPATAuditRepo encapsulates writes and reads against the
// meko_system.mcp_pat_audit_log table.
//
// The api_server writes here on token create / revoke. The MCP server
// will eventually call Append (rate-limited) for revoked-used and
// expired-used events.
type MCPPATAuditRepo struct {
	db *sql.DB
}

// NewMCPPATAuditRepo creates a new MCPPATAuditRepo.
func NewMCPPATAuditRepo(db *sql.DB) *MCPPATAuditRepo {
	return &MCPPATAuditRepo{db: db}
}

// MCPPATAuditAppendParams holds the inputs for an audit row.
//
// ActorIP / ActorAppClient / Metadata are optional. ActorType defaults
// to MCPPATActorUser when empty so callers can omit it for the
// self-service path.
//
// ActorAppClient maps to the actor_app_client column -- intentionally
// broader than HTTP User-Agent so it can carry browser UAs on
// user-driven rows or tool identifiers (e.g. "Cursor/0.42.5
// (mcp-client)") on validator-driven rows.
type MCPPATAuditAppendParams struct {
	TokenID        string
	UserID         string
	Action         models.MCPPATAuditAction
	ActorID        string
	ActorType      models.MCPPATActorType
	ActorIP        string
	ActorAppClient string
	Metadata       map[string]any
}

// Append writes a single audit row. Best-effort by design: callers
// (handler / MCP server) should log and swallow errors so that audit
// failures don't cascade into user-visible 500s for the underlying
// action.
//
// The repo itself returns the row id on success and the error from
// the database on failure.
func (r *MCPPATAuditRepo) Append(
	ctx context.Context,
	p MCPPATAuditAppendParams,
) (string, error) {
	if p.TokenID == "" || p.UserID == "" || p.ActorID == "" || p.Action == "" {
		return "", fmt.Errorf(
			"mcp pat audit: token_id, user_id, actor_id and action are required",
		)
	}

	actorType := p.ActorType
	if actorType == "" {
		actorType = models.MCPPATActorUser
	}

	metadata := p.Metadata
	if metadata == nil {
		metadata = map[string]any{}
	}
	metaJSON, err := json.Marshal(metadata)
	if err != nil {
		return "", fmt.Errorf("mcp pat audit: marshal metadata: %w", err)
	}

	var actorIP any
	if p.ActorIP != "" {
		actorIP = p.ActorIP
	}

	var id string
	err = r.db.QueryRowContext(ctx,
		`INSERT INTO meko_system.mcp_pat_audit_log
			(token_id, user_id, action, actor_id, actor_type,
			 actor_ip, actor_app_client, metadata)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8::jsonb)
		 RETURNING id`,
		p.TokenID, p.UserID, string(p.Action), p.ActorID, string(actorType),
		actorIP, nullIfEmpty(p.ActorAppClient), metaJSON,
	).Scan(&id)
	if err != nil {
		slog.Error("mcp pat audit append failed",
			"token_id", p.TokenID,
			"user_id", p.UserID,
			"action", p.Action,
			"error", err,
		)
		return "", fmt.Errorf("mcp pat audit: insert: %w", err)
	}

	slog.Info("mcp pat audit appended",
		"id", id,
		"token_id", p.TokenID,
		"user_id", p.UserID,
		"action", p.Action,
		"actor_type", actorType,
	)
	return id, nil
}

// ListByToken returns every audit row for tokenID, newest first.
// Single-partition lookup thanks to the (token_id HASH, created_at
// DESC) primary key, so no LIMIT is enforced here -- the caller can
// page in-memory if a token ever accumulates a long history.
func (r *MCPPATAuditRepo) ListByToken(
	ctx context.Context,
	tokenID string,
) ([]models.MCPPATAuditEntry, error) {
	rows, err := r.db.QueryContext(ctx,
		`SELECT id, token_id, user_id, action, actor_id, actor_type,
			actor_ip, actor_app_client, metadata, created_at
		 FROM meko_system.mcp_pat_audit_log
		 WHERE token_id = $1
		 ORDER BY created_at DESC`,
		tokenID,
	)
	if err != nil {
		return nil, fmt.Errorf("mcp pat audit: list: %w", err)
	}
	defer func() {
		if cErr := rows.Close(); cErr != nil {
			slog.Warn("mcp pat audit list: rows.Close",
				"token_id", tokenID, "error", cErr)
		}
	}()

	var out []models.MCPPATAuditEntry
	for rows.Next() {
		var (
			e              models.MCPPATAuditEntry
			action         string
			actorType      string
			actorIP        sql.NullString
			actorAppClient sql.NullString
			metadata       []byte
		)
		if err := rows.Scan(
			&e.ID, &e.TokenID, &e.UserID, &action, &e.ActorID, &actorType,
			&actorIP, &actorAppClient, &metadata, &e.CreatedAt,
		); err != nil {
			return nil, fmt.Errorf("mcp pat audit: scan: %w", err)
		}
		e.Action = models.MCPPATAuditAction(action)
		e.ActorType = models.MCPPATActorType(actorType)
		if actorIP.Valid {
			v := actorIP.String
			e.ActorIP = &v
		}
		if actorAppClient.Valid {
			e.ActorAppClient = actorAppClient.String
		}
		if len(metadata) > 0 {
			e.Metadata = json.RawMessage(metadata)
		}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("mcp pat audit: rows: %w", err)
	}
	return out, nil
}

// nullIfEmpty returns nil for an empty string so the database stores
// NULL instead of "". Used for the optional actor_ip / actor_app_client
// columns.
func nullIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}
