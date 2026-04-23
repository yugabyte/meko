// Package repository provides database access functions for the API server.
package repository

import (
	"bytes"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"fmt"
	"log/slog"
	"net/url"
	"strings"
	"text/template"
	"time"

	_ "github.com/lib/pq"

	"meko/api_server/internal/models"
)

// DatapackRepo encapsulates database operations for datapacks.
type DatapackRepo struct {
	db *sql.DB
}

// NewDatapackRepo creates a new DatapackRepo.
func NewDatapackRepo(db *sql.DB) *DatapackRepo {
	return &DatapackRepo{db: db}
}

// CreateDatapackParams holds the inputs for registering a new datapack.
type CreateDatapackParams struct {
	Name           string
	AccountID      string
	YBUniverseID   string
	YBHostname     string
	YBPort         int
	YBAdminConnStr string
	MCPUserName    string // account-level MCP YSQL role name
	MCPPassword    string // account-level MCP YSQL role password
	MCPUserExists  bool   // true when the YSQL role already exists on the cluster
}

// CreateResult holds the identifiers returned after datapack creation.
type CreateResult struct {
	DatapackID  string
	AgentID     string
	AccountID   string
	MCPUserName string
	MCPPassword string
	DBUserName  string
	DBPassword  string
	DBConnStr   string
}

// Create registers a new datapack with a default agent.
func (r *DatapackRepo) Create(p CreateDatapackParams) (*CreateResult, error) {
	slog.Info("creating datapack",
		"datapack", p.Name, "account_id", p.AccountID,
		"yb_universe_id", p.YBUniverseID)

	existing, err := r.GetByName(p.AccountID, p.Name)
	if err != nil {
		slog.Error("failed to check existing datapack",
			"datapack", p.Name, "error", err)
		return nil, fmt.Errorf("failed to check existing datapack: %w", err)
	}
	if existing != nil {
		slog.Warn("datapack already exists", "datapack", p.Name)
		return nil, fmt.Errorf("datapack %q already exists", p.Name)
	}

	dbUser := fmt.Sprintf("db_%s_%s", p.AccountID, p.Name)
	dbPass, err := randomHex(16)
	if err != nil {
		slog.Error("failed to generate db password", "datapack", p.Name, "error", err)
		return nil, fmt.Errorf("failed to generate db password: %w", err)
	}

	dbName := fmt.Sprintf("%s_%s", p.AccountID, p.Name)

	slog.Info("provisioning datapack database",
		"datapack", p.Name, "db_name", dbName,
		"mcp_user", p.MCPUserName, "mcp_user_exists", p.MCPUserExists)
	if err := provisionDatapackDB(
		p.YBAdminConnStr, dbName,
		p.MCPUserName, p.MCPPassword, p.MCPUserExists,
		dbUser, dbPass,
	); err != nil {
		slog.Error("failed to provision datapack database",
			"datapack", p.Name, "db_name", dbName, "error", err)
		return nil, fmt.Errorf("failed to provision datapack database: %w", err)
	}
	slog.Info("datapack database provisioned",
		"datapack", p.Name, "db_name", dbName)

	dbConnStr := fmt.Sprintf("postgresql://%s:%s@%s:%d/%s",
		dbUser, dbPass, p.YBHostname, p.YBPort, dbName)

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

	var datapackID string
	err = tx.QueryRow(
		`INSERT INTO meko_system.datapack_registry
			(account_id, datapack_name,
			 yb_universe_id, datapack_ysql_connection_string,
			 db_user_name, db_user_password)
		 VALUES ($1, $2, $3, $4, $5, $6)
		 RETURNING id`,
		p.AccountID, p.Name,
		p.YBUniverseID, dbConnStr,
		dbUser, dbPass,
	).Scan(&datapackID)
	if err != nil {
		slog.Error("failed to insert datapack",
			"datapack", p.Name, "account_id", p.AccountID, "error", err)
		return nil, fmt.Errorf("failed to insert datapack: %w", err)
	}

	var agentID string
	err = tx.QueryRow(
		`INSERT INTO meko_system.agent_registry
			(datapack_id, agent_name, agent_metadata)
		 VALUES ($1, $2, $3)
		 RETURNING id`,
		datapackID, "meko_user_agent", "{}",
	).Scan(&agentID)
	if err != nil {
		slog.Error("failed to create default agent",
			"datapack", p.Name, "datapack_id", datapackID, "error", err)
		return nil, fmt.Errorf("failed to create default agent: %w", err)
	}

	if err = tx.Commit(); err != nil {
		slog.Error("failed to commit datapack transaction",
			"datapack", p.Name, "datapack_id", datapackID, "error", err)
		return nil, fmt.Errorf("failed to commit datapack transaction: %w", err)
	}

	slog.Info("datapack created successfully",
		"datapack", p.Name, "datapack_id", datapackID,
		"agent_id", agentID, "account_id", p.AccountID)

	return &CreateResult{
		DatapackID:  datapackID,
		AgentID:     agentID,
		AccountID:   p.AccountID,
		MCPUserName: p.MCPUserName,
		MCPPassword: p.MCPPassword,
		DBUserName:  dbUser,
		DBPassword:  dbPass,
		DBConnStr:   dbConnStr,
	}, nil
}

func randomHex(nBytes int) (string, error) {
	b := make([]byte, nBytes)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// quoteIdent quotes a SQL identifier (database name, role name) to prevent injection.
func quoteIdent(s string) string {
	return `"` + strings.ReplaceAll(s, `"`, `""`) + `"`
}

// quoteLiteral quotes a SQL string literal.
func quoteLiteral(s string) string {
	return `'` + strings.ReplaceAll(s, `'`, `''`) + `'`
}

// QuoteIdent quotes a SQL identifier (exported for reuse by handlers).
func QuoteIdent(s string) string { return quoteIdent(s) }

// connStrWithDB rewrites a postgres URI to point at a different database.
func connStrWithDB(adminConnStr, dbName string) (string, error) {
	u, err := url.Parse(adminConnStr)
	if err != nil {
		return "", fmt.Errorf("invalid admin connection string: %w", err)
	}
	u.Path = "/" + dbName
	return u.String(), nil
}

// createMCPUserTmpl idempotently creates the account-level MCP YSQL role.
// Runs once per (account, universe); skipped when the role already exists.
var createMCPUserTmpl = template.Must(template.New("createMCP").Parse(`
DO $$ BEGIN
  IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = {{.MCPRaw}}) THEN
    CREATE USER {{.MCP}} WITH PASSWORD {{.MCPPass}};
  END IF;
END $$;
`))

// grantDatapackAccessTmpl sets up per-datapack roles and grants.
// The MCP user already exists; this template creates the DB user,
// grants the MCP user access to the new database, and locks down
// ag_catalog for the DB user.

// connect using yugabyte user to yugabyte database
// CREATE USER mcpusr;
// CREATE USER dbusr;
// CREATE DATABASE DB1 WITH COLOCATION = true;
// REVOKE CONNECT ON DATABASE yugabyte FROM mcpusr, dbusr;
// REVOKE CONNECT ON DATABASE postgres FROM mcpusr, dbusr;
// REVOKE CONNECT ON DATABASE DB1 FROM PUBLIC;
// GRANT CONNECT ON DATABASE DB1 TO mcpusr, dbusr;

// Connect using yugabyte user to db1 database
// GRANT USAGE, CREATE ON SCHEMA public TO mcpusr, dbusr;
// GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA public TO mcpusr, dbusr;
// GRANT SELECT, UPDATE ON ALL SEQUENCES IN SCHEMA public TO mcpusr, dbusr;
// GRANT EXECUTE ON ALL FUNCTIONS IN SCHEMA public TO mcpusr, dbusr;

// check if you're able to create extensions using the MCP user: MCP user cannot create extensions
// connect to db1 database using the yugabyte user

// CREATE EXTENSION IF NOT EXISTS vector;
// CREATE EXTENSION IF NOT EXISTS pg_dist_rag;
// CREATE EXTENSION IF NOT EXISTS age;

// GRANT USAGE ON SCHEMA ag_catalog TO mcpusr;
// GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA ag_catalog TO mcpusr;
// GRANT SELECT, UPDATE ON ALL SEQUENCES IN SCHEMA ag_catalog TO mcpusr;
// GRANT EXECUTE ON ALL FUNCTIONS IN SCHEMA ag_catalog TO mcpusr;

// createUserAndConnectTmpl creates the DB user and manages CONNECT grants.
// Runs on adminDB (admin connection to the default database).
var createUserAndConnectTmpl = template.Must(template.New("userConnect").Parse(`
CREATE USER {{.User}} WITH PASSWORD {{.UserPass}};

REVOKE CONNECT ON DATABASE yugabyte FROM {{.User}}, {{.MCP}};
REVOKE CONNECT ON DATABASE postgres FROM {{.User}}, {{.MCP}};
REVOKE CONNECT ON DATABASE {{.DB}} FROM PUBLIC;

GRANT CONNECT ON DATABASE {{.DB}} TO {{.MCP}}, {{.User}};
GRANT CREATE ON DATABASE {{.DB}} TO {{.MCP}};
`))

// dbSchemaGrantsTmpl grants schema-level privileges to the MCP and DB users.
// Runs on dpDB (admin connection to the new datapack database).
var dbSchemaGrantsTmpl = template.Must(template.New("dbGrants").Parse(`
GRANT USAGE, CREATE ON SCHEMA PUBLIC TO {{.MCP}}, {{.User}};
GRANT SELECT, UPDATE ON ALL SEQUENCES IN SCHEMA PUBLIC TO {{.MCP}}, {{.User}};
GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA public TO {{.MCP}}, {{.User}};
GRANT EXECUTE ON ALL FUNCTIONS IN SCHEMA PUBLIC TO {{.MCP}}, {{.User}};
`))

// extensionsTmpl creates required extensions and grants ag_catalog + dist_rag access.
// Runs on dpDB (admin connection to the new datapack database).
var extensionsTmpl = template.Must(template.New("extensions").Parse(`
CREATE EXTENSION IF NOT EXISTS vector;
CREATE EXTENSION IF NOT EXISTS pg_dist_rag;
CREATE EXTENSION IF NOT EXISTS age;

GRANT USAGE, CREATE ON SCHEMA ag_catalog TO {{.MCP}};
GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA ag_catalog TO {{.MCP}};
GRANT SELECT, UPDATE ON ALL SEQUENCES IN SCHEMA ag_catalog TO {{.MCP}};
GRANT EXECUTE ON ALL FUNCTIONS IN SCHEMA ag_catalog TO {{.MCP}};

GRANT USAGE ON SCHEMA dist_rag TO {{.MCP}}, {{.User}};
GRANT SELECT ON ALL TABLES IN SCHEMA dist_rag TO {{.MCP}}, {{.User}};
GRANT EXECUTE ON ALL FUNCTIONS IN SCHEMA dist_rag TO {{.MCP}}, {{.User}};
`))

func renderSQL(tmpl *template.Template, data map[string]string) (string, error) {
	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, data); err != nil {
		return "", err
	}
	return buf.String(), nil
}

// provisionDatapackDB creates the database, roles, and extensions on the
// target universe using the admin connection string. When mcpUserExists
// is false the account-level MCP role is created for the first time;
// otherwise it is assumed to already exist on this cluster.
func provisionDatapackDB(adminConnStr, dbName, mcpUser, mcpPass string, mcpUserExists bool, dbUser, dbPass string) error {

	slog.Info("provisioning datapack database",
		"db_name", dbName, "mcp_user", mcpUser,
		"mcp_user_exists", mcpUserExists)

	adminDB, err := sql.Open("postgres", adminConnStr)
	if err != nil {
		return fmt.Errorf("failed to connect to universe: %w", err)
	}
	defer adminDB.Close()

	if err := adminDB.Ping(); err != nil {
		return fmt.Errorf("YB universe admin connection unreachable: %w", err)
	}

	provisionedAllSteps := false
	defer func() {
		if provisionedAllSteps {
			return
		}
		slog.Warn("cleaning up failed provisioning", "db_name", dbName)
		dropProvisioned(adminDB, dbName, mcpUser, mcpUserExists, dbUser)
	}()

	// CREATE DATABASE cannot run inside a transaction in PostgreSQL.
	if _, err := adminDB.Exec(fmt.Sprintf(
		`CREATE DATABASE %s WITH COLOCATION = true`, quoteIdent(dbName),
	)); err != nil {
		return fmt.Errorf("failed to create database %s: %w", dbName, err)
	}
	// dbCreated = true

	// Ensure the account-level MCP YSQL role exists on this cluster.
	createMCP, err := renderSQL(createMCPUserTmpl, map[string]string{
		"MCP":     quoteIdent(mcpUser),
		"MCPRaw":  quoteLiteral(mcpUser),
		"MCPPass": quoteLiteral(mcpPass),
	})
	if err != nil {
		return fmt.Errorf("failed to render create-MCP-user SQL: %w", err)
	}
	slog.Info("ensuring MCP user exists", "mcp_user", mcpUser)
	if _, err := adminDB.Exec(createMCP); err != nil {
		return fmt.Errorf("failed to create MCP user %s: %w", mcpUser, err)
	}

	// Create DB user and manage CONNECT grants on the admin connection (default DB).
	userConnectSQL, err := renderSQL(createUserAndConnectTmpl, map[string]string{
		"MCP":      quoteIdent(mcpUser),
		"User":     quoteIdent(dbUser),
		"UserPass": quoteLiteral(dbPass),
		"DB":       quoteIdent(dbName),
	})
	if err != nil {
		return fmt.Errorf("failed to render user-connect SQL: %w", err)
	}
	if _, err := adminDB.Exec(userConnectSQL); err != nil {
		return fmt.Errorf("failed to create user and connect grants for %s: %w", dbName, err)
	}

	// Open admin connection to the new datapack database for schema-level setup.
	dpDBConnStr, err := connStrWithDB(adminConnStr, dbName)
	if err != nil {
		return err
	}
	dpDB, err := sql.Open("postgres", dpDBConnStr)
	if err != nil {
		return fmt.Errorf("failed to connect to new database %s: %w", dbName, err)
	}
	defer dpDB.Close()

	// Grant schema-level privileges on the new database.
	schemaGrantsSQL, err := renderSQL(dbSchemaGrantsTmpl, map[string]string{
		"MCP":  quoteIdent(mcpUser),
		"User": quoteIdent(dbUser),
	})
	if err != nil {
		return fmt.Errorf("failed to render schema grants SQL: %w", err)
	}
	if _, err := dpDB.Exec(schemaGrantsSQL); err != nil {
		return fmt.Errorf("failed to apply schema grants for %s: %w", dbName, err)
	}

	// Install extensions and grant ag_catalog + dist_rag access on the new database.
	extensionsSQL, err := renderSQL(extensionsTmpl, map[string]string{
		"MCP":  quoteIdent(mcpUser),
		"User": quoteIdent(dbUser),
	})
	if err != nil {
		return fmt.Errorf("failed to render extensions SQL: %w", err)
	}
	if _, err := dpDB.Exec(extensionsSQL); err != nil {
		return fmt.Errorf("failed to install extensions in %s: %w", dbName, err)
	}

	// All steps succeeded — disarm cleanup.
	provisionedAllSteps = true
	return nil
}

// DropDatapackDB drops the database and the per-datapack DB user role.
// The shared account-level MCP role is never dropped here.
func DropDatapackDB(adminDB *sql.DB, dbName, dbUser string) error {
	slog.Info("dropping datapack DB and DB user role",
		"db_name", dbName, "db_user", dbUser)

	if _, err := adminDB.Exec(fmt.Sprintf(
		`DROP DATABASE IF EXISTS %s`, quoteIdent(dbName),
	)); err != nil {
		return fmt.Errorf("failed to drop database %s: %w", dbName, err)
	}
	if _, err := adminDB.Exec(fmt.Sprintf(
		`DROP ROLE IF EXISTS %s`, quoteIdent(dbUser),
	)); err != nil {
		slog.Warn("dropped database but failed to drop DB user role",
			"db_user", dbUser, "error", err)
	}
	return nil
}

// dropProvisioned is a best-effort cleanup of a partially provisioned datapack.
// When mcpUserExists is true the MCP role was pre-existing and must not be dropped.
func dropProvisioned(adminDB *sql.DB, dbName, mcpUser string, mcpUserExists bool, dbUser string) {
	if _, err := adminDB.Exec(fmt.Sprintf(
		`DROP DATABASE IF EXISTS %s`, quoteIdent(dbName),
	)); err != nil {
		slog.Warn("cleanup: failed to drop database", "db_name", dbName, "error", err)
	}
	if !mcpUserExists {
		if _, err := adminDB.Exec(fmt.Sprintf(
			`DROP ROLE IF EXISTS %s`, quoteIdent(mcpUser),
		)); err != nil {
			slog.Warn("cleanup: failed to drop MCP role", "mcp_user", mcpUser, "error", err)
		}
	}
	if _, err := adminDB.Exec(fmt.Sprintf(
		`DROP ROLE IF EXISTS %s`, quoteIdent(dbUser),
	)); err != nil {
		slog.Warn("cleanup: failed to drop DB role", "db_user", dbUser, "error", err)
	}
}

// List returns datapacks belonging to the given account.
func (r *DatapackRepo) List(accountID string) ([]models.Datapack, error) {
	rows, err := r.db.Query(
		`SELECT d.id, d.account_id, d.datapack_name,
		        d.yb_universe_id, d.datapack_ysql_connection_string,
		        a.mcp_user_name, d.db_user_name,
		        d.created_at, d.updated_at
		 FROM meko_system.datapack_registry d
		 JOIN meko_system.account_registry a ON a.id = d.account_id
		 WHERE d.account_id = $1
		 ORDER BY d.created_at DESC`,
		accountID,
	)
	if err != nil {
		return nil, fmt.Errorf("failed to list datapacks: %w", err)
	}
	defer rows.Close()

	var result []models.Datapack
	for rows.Next() {
		var d models.Datapack
		if err := rows.Scan(
			&d.ID, &d.AccountID, &d.Name,
			&d.YBUniverseID, &d.YBUniverseConn,
			&d.MCPUserName, &d.DBUserName,
			&d.CreatedAt, &d.UpdatedAt,
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

// GetByName returns a single datapack by account and name.
func (r *DatapackRepo) GetByName(accountID, name string) (*models.Datapack, error) {
	var d models.Datapack
	err := r.db.QueryRow(
		`SELECT d.id, d.account_id, d.datapack_name,
		        d.yb_universe_id, d.datapack_ysql_connection_string,
		        a.mcp_user_name, d.db_user_name,
		        d.created_at, d.updated_at
		 FROM meko_system.datapack_registry d
		 JOIN meko_system.account_registry a ON a.id = d.account_id
		 WHERE d.account_id = $1 AND d.datapack_name = $2`,
		accountID, name,
	).Scan(
		&d.ID, &d.AccountID, &d.Name,
		&d.YBUniverseID, &d.YBUniverseConn,
		&d.MCPUserName, &d.DBUserName,
		&d.CreatedAt, &d.UpdatedAt,
	)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("failed to get datapack: %w", err)
	}
	return &d, nil
}

// Update modifies a datapack's mutable fields.
func (r *DatapackRepo) Update(
	accountID, name, ybConnStr string,
) error {
	if ybConnStr == "" {
		return nil
	}

	res, err := r.db.Exec(
		`UPDATE meko_system.datapack_registry
		 SET datapack_ysql_connection_string = $1, updated_at = NOW()
		 WHERE account_id = $2 AND datapack_name = $3`,
		ybConnStr, accountID, name,
	)
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

// Delete removes a datapack by account and name. CASCADE handles child rows.
func (r *DatapackRepo) Delete(accountID, name string) error {
	res, err := r.db.Exec(
		`DELETE FROM meko_system.datapack_registry
		 WHERE account_id = $1 AND datapack_name = $2`, accountID, name,
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
// default datapack belonging to the given account. Returns (nil, nil)
// when the datapack does not exist yet (provisioning still in progress).
func (r *DatapackRepo) GetDefaultDatapackDetails(accountID string) (*DefaultDatapackDetails, error) {
	var d DefaultDatapackDetails
	err := r.db.QueryRow(
		`SELECT d.id, d.datapack_name,
		        d.datapack_ysql_connection_string,
		        d.db_user_name, d.db_user_password,
		        a.mcp_user_password
		 FROM meko_system.datapack_registry d
		 JOIN meko_system.account_registry a ON a.id = d.account_id
		 WHERE d.account_id = $1 AND d.datapack_name = 'meko_default_datapack'`,
		accountID,
	).Scan(
		&d.DatapackID, &d.DatapackName,
		&d.DBConnStr,
		&d.DBUserName, &d.DBPassword,
		&d.MCPAPIKey,
	)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		slog.Error("failed to get default datapack details",
			"account_id", accountID, "error", err)
		return nil, fmt.Errorf("failed to get default datapack details: %w", err)
	}

	d.MCPServerURL = fmt.Sprintf("https://%s.mcp.mekodev.com/mcp", accountID)
	return &d, nil
}

// DatapackStatus holds status information for a datapack.
type DatapackStatus struct {
	CreatedAt      time.Time `json:"created_at"`
	Name           string    `json:"name"`
	ID             string    `json:"id"`
	YBUniverseID   string    `json:"yb_universe_id"`
	YBUniverseConn string    `json:"datapack_ysql_connection_string"`
	MCPUserName    string    `json:"mcp_user_name"`
	DBUserName     string    `json:"db_user_name"`
	AgentCount     int       `json:"agent_count"`
	KBCount        int       `json:"knowledge_base_count"`
}

// GetStatus returns a summary status for a datapack.
func (r *DatapackRepo) GetStatus(accountID, name string) (*DatapackStatus, error) {
	var s DatapackStatus
	err := r.db.QueryRow(
		`SELECT d.id, d.datapack_name,
		        d.yb_universe_id, d.datapack_ysql_connection_string,
		        a.mcp_user_name, d.db_user_name,
		        d.created_at,
		        (SELECT COUNT(*) FROM meko_system.agent_registry
		         WHERE datapack_id = d.id),
		        (SELECT COUNT(*)
		         FROM meko_system.datapack_knowledge_base_registry
		         WHERE datapack_id = d.id)
		 FROM meko_system.datapack_registry d
		 JOIN meko_system.account_registry a ON a.id = d.account_id
		 WHERE d.account_id = $1 AND d.datapack_name = $2`, accountID, name,
	).Scan(
		&s.ID, &s.Name,
		&s.YBUniverseID, &s.YBUniverseConn,
		&s.MCPUserName, &s.DBUserName,
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
