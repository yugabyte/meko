package repository

import (
	"database/sql"
	"fmt"
)

// ConversationSearchOptOutRepo wraps `meko_system.conversation_search_opt_outs`.
// Presence of a (datapack_id) row means the datapack has opted out of
// conversation-search embedding; absence means enabled (the default). Written
// directly by both api_server (this repo) and meko-mcp-server (its own SQL,
// see `_set_conversation_search_opt_out` in tools.py) against the same table
// with identical semantics — the same split already used for
// meko_system.free_tier_usage / token_savings_log.
type ConversationSearchOptOutRepo struct {
	db *sql.DB
}

func NewConversationSearchOptOutRepo(db *sql.DB) *ConversationSearchOptOutRepo {
	return &ConversationSearchOptOutRepo{db: db}
}

// IsOptedOut reports whether datapackID has an opt-out row. Returns false,
// nil when no row exists (the common, default-enabled case).
func (r *ConversationSearchOptOutRepo) IsOptedOut(datapackID string) (bool, error) {
	var exists int
	err := r.db.QueryRow(
		`SELECT 1 FROM meko_system.conversation_search_opt_outs WHERE datapack_id = $1`,
		datapackID,
	).Scan(&exists)
	if err == sql.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("conversation_search_opt_outs: read %s: %w", datapackID, err)
	}
	return true, nil
}

// SetOptedOut upserts the opt-out row for datapackID, stamping optedOutBy
// (the acting user's id) and refreshing opted_out_at.
func (r *ConversationSearchOptOutRepo) SetOptedOut(datapackID, optedOutBy string) error {
	_, err := r.db.Exec(
		`INSERT INTO meko_system.conversation_search_opt_outs (datapack_id, opted_out_by)
		 VALUES ($1, $2)
		 ON CONFLICT (datapack_id) DO UPDATE
		     SET opted_out_at = now(), opted_out_by = EXCLUDED.opted_out_by`,
		datapackID, optedOutBy,
	)
	if err != nil {
		return fmt.Errorf("conversation_search_opt_outs: set opted-out for %s: %w", datapackID, err)
	}
	return nil
}

// ClearOptedOut deletes the opt-out row for datapackID, re-enabling
// conversation-search embedding (the default when no row exists).
func (r *ConversationSearchOptOutRepo) ClearOptedOut(datapackID string) error {
	_, err := r.db.Exec(
		`DELETE FROM meko_system.conversation_search_opt_outs WHERE datapack_id = $1`,
		datapackID,
	)
	if err != nil {
		return fmt.Errorf("conversation_search_opt_outs: clear opt-out for %s: %w", datapackID, err)
	}
	return nil
}
