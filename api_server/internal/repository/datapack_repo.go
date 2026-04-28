// Package repository provides database access functions for the API server.
package repository

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"

	_ "github.com/lib/pq"

	"meko/api_server/internal/models"
)

// QuoteIdent quotes a SQL identifier (table, column, role) by wrapping
// it in double quotes and escaping any embedded double quotes. Used
// when building dynamic SQL where bind parameters cannot be applied
// (e.g. ALTER TABLE ... OWNER TO).
func QuoteIdent(s string) string {
	return `"` + strings.ReplaceAll(s, `"`, `""`) + `"`
}

// DefaultAgentID is the agent_id used for the user-scoped default agent
// created at signup. Mirrors the schema default on meko_system.agents.
const DefaultAgentID = "meko_agent"

// DatapackRepo encapsulates database operations for datapacks.
//
// Backing table: meko_system.datapacks. Identified by
// (user_id, datapack_id) with a UNIQUE (user_id, datapack_name)
// constraint that lets us look up by name for the URL-friendly
// /datapacks/:name routes.
type DatapackRepo struct {
	db *sql.DB
}

// NewDatapackRepo creates a new DatapackRepo.
func NewDatapackRepo(db *sql.DB) *DatapackRepo {
	return &DatapackRepo{db: db}
}

// CreateDatapackParams holds the inputs for registering a new datapack.
//
// AgentID is required and must reference an existing row in
// meko_system.agents for the same UserID. Callers default it to
// DefaultAgentID when the API caller did not supply one.
//
// In the multi-tenant model the universe is shared and the YSQL
// credentials are the user-level MCP role: ConnStr, MCPUserName, and
// MCPPassword are stored on the datapack row as
// datapack_ysql_connection_string / username / password so per-datapack
// connection strings can be emitted without re-joining to users.
type CreateDatapackParams struct {
	Name        string
	UserID      string
	AgentID     string
	UniverseID  string
	ConnStr     string
	MCPUserName string
	MCPPassword string
	Description string
}

// CreateResult holds the identifiers returned after datapack creation.
type CreateResult struct {
	DatapackID  string
	UserID      string
	AgentID     string
	MCPUserName string
	MCPPassword string
	ConnStr     string
}

// ErrAgentNotFound is returned when a datapack create references an
// agent_id that doesn't exist for the user. There is no FK between
// meko_system.datapacks / agent_access and meko_system.agents (YB
// avoids cross-tablet FKs), so this is enforced at the app layer.
var ErrAgentNotFound = fmt.Errorf("agent not found for user")

// Create registers a new datapack and links it to the supplied agent
// in a single transaction:
//
//  1. Verify (user_id, agent_id) exists in meko_system.agents.
//  2. INSERT into meko_system.datapacks with the created_by_agent_id
//     column stamped on the row (records the creator agent).
//  3. INSERT into meko_system.agent_access linking the agent to
//     the new datapack so it shows up in access lists.
//
// meko_system.agents is never written here — the user's default agent
// is created at signup by AccountRepo.Create, and additional agents
// are created via POST /agents (AgentRepo.CreateAgent).
//
// The MCP YSQL credentials are recorded as the datapack's username /
// password since every datapack for a given user shares the same
// universe-level role.
func (r *DatapackRepo) Create(p CreateDatapackParams) (*CreateResult, error) {
	if p.AgentID == "" {
		return nil, fmt.Errorf("agent_id is required")
	}

	slog.Info("creating datapack",
		"datapack", p.Name, "user_id", p.UserID,
		"agent_id", p.AgentID, "universe_id", p.UniverseID)

	tx, err := r.db.Begin()
	if err != nil {
		slog.Error("failed to begin transaction",
			"datapack", p.Name, "error", err)
		return nil, fmt.Errorf("failed to begin transaction: %w", err)
	}
	defer func() {
		if rbErr := tx.Rollback(); rbErr != nil && rbErr != sql.ErrTxDone {
			slog.Warn("rollback error", "datapack", p.Name, "error", rbErr)
		}
	}()

	var exists bool
	if err = tx.QueryRow(
		`SELECT EXISTS (
		     SELECT 1 FROM meko_system.agents
		     WHERE user_id = $1 AND agent_id = $2
		 )`,
		p.UserID, p.AgentID,
	).Scan(&exists); err != nil {
		slog.Error("failed to check agent existence",
			"user_id", p.UserID, "agent_id", p.AgentID, "error", err)
		return nil, fmt.Errorf("failed to check agent existence: %w", err)
	}
	if !exists {
		return nil, fmt.Errorf("%w: user=%s agent_id=%s",
			ErrAgentNotFound, p.UserID, p.AgentID)
	}

	var detailsJSON interface{}
	if p.Description != "" {
		b, marshalErr := json.Marshal(map[string]string{"description": p.Description})
		if marshalErr != nil {
			return nil, fmt.Errorf("failed to marshal datapack_details: %w", marshalErr)
		}
		detailsJSON = string(b)
	}

	var datapackID string
	err = tx.QueryRow(
		`INSERT INTO meko_system.datapacks
			(user_id, created_by_agent_id, datapack_name, universe_id,
			 datapack_ysql_connection_string,
			 datapack_ysql_username, datapack_ysql_password,
			 datapack_details)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
		 RETURNING datapack_id`,
		p.UserID, p.AgentID, p.Name, p.UniverseID,
		p.ConnStr, p.MCPUserName, p.MCPPassword,
		detailsJSON,
	).Scan(&datapackID)
	if err != nil {
		slog.Error("failed to insert datapack",
			"datapack", p.Name, "user_id", p.UserID, "error", err)
		return nil, fmt.Errorf("failed to insert datapack: %w", err)
	}

	if _, err = tx.Exec(
		`INSERT INTO meko_system.agent_access
			(user_id, agent_id, datapack_id)
		 VALUES ($1, $2, $3)
		 ON CONFLICT DO NOTHING`,
		p.UserID, p.AgentID, datapackID,
	); err != nil {
		slog.Error("failed to link agent to datapack",
			"user_id", p.UserID, "agent_id", p.AgentID,
			"datapack_id", datapackID, "error", err)
		return nil, fmt.Errorf("failed to link agent to datapack: %w", err)
	}

	if err = tx.Commit(); err != nil {
		slog.Error("failed to commit datapack transaction",
			"datapack", p.Name, "datapack_id", datapackID, "error", err)
		return nil, fmt.Errorf("failed to commit datapack transaction: %w", err)
	}

	slog.Info("datapack created successfully",
		"datapack", p.Name, "datapack_id", datapackID,
		"agent_id", p.AgentID, "user_id", p.UserID)

	return &CreateResult{
		DatapackID:  datapackID,
		UserID:      p.UserID,
		AgentID:     p.AgentID,
		MCPUserName: p.MCPUserName,
		MCPPassword: p.MCPPassword,
		ConnStr:     p.ConnStr,
	}, nil
}

// List returns datapacks belonging to the given user, with the user's
// MCP role name resolved from the user_details JSONB.
func (r *DatapackRepo) List(userID string) ([]models.Datapack, error) {
	rows, err := r.db.Query(
		`SELECT d.datapack_id, d.user_id, d.datapack_name,
		        COALESCE(d.created_by_agent_id, ''),
		        d.universe_id, d.datapack_ysql_connection_string,
		        d.datapack_ysql_username,
		        d.created_at, d.updated_at,
		        COALESCE(d.datapack_details->>'description', '')
		 FROM meko_system.datapacks d
		 WHERE d.user_id = $1
		 ORDER BY d.created_at DESC`,
		userID,
	)
	if err != nil {
		return nil, fmt.Errorf("failed to list datapacks: %w", err)
	}
	defer rows.Close()

	var result []models.Datapack
	for rows.Next() {
		var d models.Datapack
		if err := rows.Scan(
			&d.ID, &d.UserID, &d.Name,
			&d.CreatedByAgentID,
			&d.UniverseID, &d.YBUniverseConn,
			&d.MCPUserName,
			&d.CreatedAt, &d.UpdatedAt,
			&d.Description,
		); err != nil {
			return nil, fmt.Errorf("failed to scan datapack: %w", err)
		}
		result = append(result, d)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("rows iteration error: %w", err)
	}

	return result, nil
}

// GetByName returns a single datapack by user and name. Relies on the
// UNIQUE (user_id, datapack_name) constraint on meko_system.datapacks.
func (r *DatapackRepo) GetByName(userID, name string) (*models.Datapack, error) {
	var d models.Datapack
	err := r.db.QueryRow(
		`SELECT d.datapack_id, d.user_id, d.datapack_name,
		        COALESCE(d.created_by_agent_id, ''),
		        d.universe_id, d.datapack_ysql_connection_string,
		        d.datapack_ysql_username,
		        d.created_at, d.updated_at,
		        COALESCE(d.datapack_details->>'description', '')
		 FROM meko_system.datapacks d
		 WHERE d.user_id = $1 AND d.datapack_name = $2`,
		userID, name,
	).Scan(
		&d.ID, &d.UserID, &d.Name,
		&d.CreatedByAgentID,
		&d.UniverseID, &d.YBUniverseConn,
		&d.MCPUserName,
		&d.CreatedAt, &d.UpdatedAt,
		&d.Description,
	)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("failed to get datapack: %w", err)
	}
	return &d, nil
}

// UpdateDatapackParams holds the optional fields for a partial update.
// Only non-empty fields are applied.
type UpdateDatapackParams struct {
	YBConnStr   string
	Name        string
	Description string
}

// Update modifies a datapack's mutable fields. Only non-empty params
// are written; an all-empty request is a no-op.
func (r *DatapackRepo) Update(
	userID, name string, p UpdateDatapackParams,
) error {
	setClauses := []string{}
	args := []interface{}{}
	idx := 1

	if p.YBConnStr != "" {
		setClauses = append(setClauses, fmt.Sprintf("datapack_ysql_connection_string = $%d", idx))
		args = append(args, p.YBConnStr)
		idx++
	}
	if p.Name != "" {
		setClauses = append(setClauses, fmt.Sprintf("datapack_name = $%d", idx))
		args = append(args, p.Name)
		idx++
	}
	if p.Description != "" {
		detailsJSON, marshalErr := json.Marshal(map[string]string{"description": p.Description})
		if marshalErr != nil {
			return fmt.Errorf("failed to marshal datapack_details: %w", marshalErr)
		}
		setClauses = append(setClauses, fmt.Sprintf("datapack_details = $%d", idx))
		args = append(args, string(detailsJSON))
		idx++
	}

	if len(setClauses) == 0 {
		return nil
	}

	setClauses = append(setClauses, "updated_at = NOW()")
	query := fmt.Sprintf(
		"UPDATE meko_system.datapacks SET %s WHERE user_id = $%d AND datapack_name = $%d",
		strings.Join(setClauses, ", "), idx, idx+1,
	)
	args = append(args, userID, name)

	res, err := r.db.Exec(query, args...)
	if err != nil {
		return fmt.Errorf("failed to update datapack: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("failed to check rows affected: %w", err)
	}
	if n == 0 {
		return fmt.Errorf("datapack %q not found", name)
	}
	return nil
}

// Delete removes a datapack by user and name. Child rows in
// agent_access / agent_access_permissions / knowledge_bases must
// either be cascaded by FK or handled separately by the caller.
func (r *DatapackRepo) Delete(userID, name string) error {
	res, err := r.db.Exec(
		`DELETE FROM meko_system.datapacks
		 WHERE user_id = $1 AND datapack_name = $2`,
		userID, name,
	)
	if err != nil {
		return fmt.Errorf("failed to delete datapack: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("failed to check rows affected: %w", err)
	}
	if n == 0 {
		return fmt.Errorf("datapack %q not found", name)
	}
	return nil
}

// DefaultDatapackDetails holds the full credentials returned only
// during provisioning (the internal provision-status endpoint).
type DefaultDatapackDetails struct {
	DatapackID   string `json:"datapack_id"`
	DatapackName string `json:"datapack_name"`
	MCPServerURL string `json:"mcp_server_url"`
	MCPAPIKey    string `json:"mcp_api_key"`
	DBUserName   string `json:"db_user_name"`
	DBPassword   string `json:"db_password"`
	DBConnStr    string `json:"db_conn_str"`
}

// GetDefaultDatapackDetails returns the full credentials for the
// default datapack belonging to the given user. Returns (nil, nil)
// when the datapack does not exist yet (provisioning still in
// progress). The MCP API key is read from the user's user_details
// JSONB payload so we never store the password twice.
func (r *DatapackRepo) GetDefaultDatapackDetails(userID string) (*DefaultDatapackDetails, error) {
	var d DefaultDatapackDetails
	var mcpKey sql.NullString
	err := r.db.QueryRow(
		`SELECT d.datapack_id, d.datapack_name,
		        d.datapack_ysql_connection_string,
		        d.datapack_ysql_username, d.datapack_ysql_password,
		        u.user_details->'mcp_credentials'->>'mcp_user_password'
		 FROM meko_system.datapacks d
		 JOIN meko_system.users u ON u.user_id = d.user_id
		 WHERE d.user_id = $1 AND d.datapack_name = $2`,
		userID, "meko_default_datapack",
	).Scan(
		&d.DatapackID, &d.DatapackName,
		&d.DBConnStr,
		&d.DBUserName, &d.DBPassword,
		&mcpKey,
	)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		slog.Error("failed to get default datapack details",
			"user_id", userID, "error", err)
		return nil, fmt.Errorf("failed to get default datapack details: %w", err)
	}

	if mcpKey.Valid {
		d.MCPAPIKey = mcpKey.String
	}
	d.MCPServerURL = fmt.Sprintf("https://%s.mcp.mekodev.com/mcp", userID)
	return &d, nil
}

// DatapackStatus holds status information for a datapack.
type DatapackStatus struct {
	CreatedAt        time.Time `json:"created_at"`
	Name             string    `json:"datapack_name"`
	ID               string    `json:"datapack_id"`
	CreatedByAgentID string    `json:"created_by_agent_id,omitempty"`
	UniverseID       string    `json:"universe_id"`
	YBUniverseConn   string    `json:"datapack_ysql_connection_string"`
	MCPUserName      string    `json:"datapack_ysql_username"`
	AgentCount       int       `json:"agent_count"`
	KBCount          int       `json:"knowledge_base_count"`
}

// GetStatus returns a summary status for a datapack.
//
// agent_count is the number of agents linked to the datapack via
// agent_access. knowledge_base_count comes from
// meko_system.knowledge_bases keyed on datapack_id.
func (r *DatapackRepo) GetStatus(userID, name string) (*DatapackStatus, error) {
	var s DatapackStatus
	err := r.db.QueryRow(
		`SELECT d.datapack_id, d.datapack_name,
		        COALESCE(d.created_by_agent_id, ''),
		        d.universe_id, d.datapack_ysql_connection_string,
		        d.datapack_ysql_username,
		        d.created_at,
		        (SELECT COUNT(*) FROM meko_system.agent_access ad
		           WHERE ad.user_id = d.user_id
		             AND ad.datapack_id = d.datapack_id),
		        (SELECT COUNT(*) FROM meko_system.knowledge_bases kb
		           WHERE kb.datapack_id = d.datapack_id)
		 FROM meko_system.datapacks d
		 WHERE d.user_id = $1 AND d.datapack_name = $2`,
		userID, name,
	).Scan(
		&s.ID, &s.Name,
		&s.CreatedByAgentID,
		&s.UniverseID, &s.YBUniverseConn,
		&s.MCPUserName,
		&s.CreatedAt, &s.AgentCount, &s.KBCount,
	)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("failed to get datapack status: %w", err)
	}
	return &s, nil
}
