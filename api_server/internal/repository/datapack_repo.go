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
	Name         string
	AccountID    string
	YBUniverseID string
	YBHostname   string
	YBPort       int
	YBAdminConnStr string
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

	existing, err := r.GetByName(p.Name)
	if err != nil {
		slog.Error("failed to check existing datapack",
			"datapack", p.Name, "error", err)
		return nil, fmt.Errorf("failed to check existing datapack: %w", err)
	}
	if existing != nil {
		slog.Warn("datapack already exists", "datapack", p.Name)
		return nil, fmt.Errorf("datapack %q already exists", p.Name)
	}

	mcpUser := fmt.Sprintf("mcp_%s", p.Name)
	mcpPass, err := randomHex(16)
	if err != nil {
		slog.Error("failed to generate mcp password", "datapack", p.Name, "error", err)
		return nil, fmt.Errorf("failed to generate mcp password: %w", err)
	}
	dbUser := fmt.Sprintf("db_%s", p.Name)
	dbPass, err := randomHex(16)
	if err != nil {
		slog.Error("failed to generate db password", "datapack", p.Name, "error", err)
		return nil, fmt.Errorf("failed to generate db password: %w", err)
	}

	dbName := fmt.Sprintf("%s_%s", p.AccountID, p.Name)

	slog.Info("provisioning datapack database",
		"datapack", p.Name, "db_name", dbName)
	if err := provisionDatapackDB(
		p.YBAdminConnStr, dbName,
		mcpUser, mcpPass,
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
			 mcp_user_name, mcp_user_password,
			 db_user_name, db_user_password)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
		 RETURNING id`,
		p.AccountID, p.Name,
		p.YBUniverseID, dbConnStr,
		mcpUser, mcpPass,
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
		MCPUserName: mcpUser,
		MCPPassword: mcpPass,
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

// connStrWithDB rewrites a postgres URI to point at a different database.
func connStrWithDB(adminConnStr, dbName string) (string, error) {
	u, err := url.Parse(adminConnStr)
	if err != nil {
		return "", fmt.Errorf("invalid admin connection string: %w", err)
	}
	u.Path = "/" + dbName
	return u.String(), nil
}

// var rolesAndGrantsTmpl = template.Must(template.New("roles").Parse(`
// CREATE ROLE {{.MCP}} WITH LOGIN PASSWORD {{.MCPPass}};
// CREATE ROLE {{.User}} WITH LOGIN PASSWORD {{.UserPass}};
// GRANT ALL PRIVILEGES ON DATABASE {{.DB}} TO {{.User}};
// GRANT CONNECT ON DATABASE {{.DB}} TO {{.MCP}};
// `))

var rolesAndGrantsTmpl = template.Must(template.New("roles").Parse(`
REVOKE CONNECT ON DATABASE {{.DB}} FROM PUBLIC;

CREATE USER {{.MCP}} WITH PASSWORD {{.MCPPass}};
CREATE USER {{.User}} WITH PASSWORD {{.UserPass}};

GRANT CONNECT ON DATABASE {{.DB}} TO {{.MCP}};
GRANT USAGE, CREATE ON SCHEMA public TO {{.MCP}};
GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA public TO {{.MCP}};
GRANT SELECT, UPDATE ON ALL SEQUENCES IN SCHEMA public TO {{.MCP}};

GRANT {{.MCP}} TO {{.User}};

REVOKE ALL ON SCHEMA ag_catalog FROM {{.User}};
REVOKE ALL ON ALL TABLES IN SCHEMA ag_catalog FROM {{.User}};
ALTER DEFAULT PRIVILEGES IN SCHEMA ag_catalog REVOKE ALL ON TABLES FROM {{.User}};
`))

var extensionsTmpl = template.Must(template.New("extensions").Parse(`
CREATE EXTENSION IF NOT EXISTS vector;
CREATE EXTENSION IF NOT EXISTS pg_dist_rag;
CREATE EXTENSION IF NOT EXISTS age;
`))

func renderSQL(tmpl *template.Template, data map[string]string) (string, error) {
	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, data); err != nil {
		return "", err
	}
	return buf.String(), nil
}

// provisionDatapackDB creates the database, roles, and extensions on the
// target universe using the admin connection string.
func provisionDatapackDB(adminConnStr, dbName, mcpUser, mcpPass, dbUser, dbPass string) error {

	slog.Info("provisioning datapack database",
		"db_name", dbName, "admin_conn_str", adminConnStr)

	adminDB, err := sql.Open("postgres", adminConnStr)
	if err != nil {
		return fmt.Errorf("failed to connect to universe: %w", err)
	}
	defer adminDB.Close()

	if err := adminDB.Ping(); err != nil {
		return fmt.Errorf("YB universe admin connection unreachable: %w", err)
	}

	// Track whether the DB was created so we know if cleanup is needed.
	provisionedAllSteps := false
	defer func() {
		if provisionedAllSteps {
			return
		}
		slog.Warn("cleaning up failed provisioning", "db_name", dbName)
		dropProvisioned(adminDB, dbName, mcpUser, dbUser)
	}()

	// CREATE DATABASE cannot run inside a transaction in PostgreSQL.
	if _, err := adminDB.Exec(fmt.Sprintf(
		`CREATE DATABASE %s WITH COLOCATION = true`, quoteIdent(dbName),
	)); err != nil {
		return fmt.Errorf("failed to create database %s: %w", dbName, err)
	}
	// dbCreated = true

	rolesAndGrants, err := renderSQL(rolesAndGrantsTmpl, map[string]string{
		"MCP":      quoteIdent(mcpUser),
		"MCPPass":  quoteLiteral(mcpPass),
		"User":     quoteIdent(dbUser),
		"UserPass": quoteLiteral(dbPass),
		"DB":       quoteIdent(dbName),
	})
	if err != nil {
		return fmt.Errorf("failed to render roles/grants SQL: %w", err)
	}
	if _, err := adminDB.Exec(rolesAndGrants); err != nil {
		return fmt.Errorf("failed to create roles and grants for %s: %w", dbName, err)
	}

	dpDBConnStr, err := connStrWithDB(adminConnStr, dbName)
	if err != nil {
		return err
	}
	dpDB, err := sql.Open("postgres", dpDBConnStr)
	if err != nil {
		return fmt.Errorf("failed to connect to new database %s: %w", dbName, err)
	}
	defer dpDB.Close()

	extensionsSQL, err := renderSQL(extensionsTmpl, map[string]string{
		"DB": quoteIdent(dbName),
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

// DropDatapackDB drops the database and roles for a datapack, returning
// an error if the database drop fails.
func DropDatapackDB(adminDB *sql.DB, dbName, mcpUser, dbUser string) error {
	if _, err := adminDB.Exec(fmt.Sprintf(
		`DROP DATABASE IF EXISTS %s`, quoteIdent(dbName),
	)); err != nil {
		return fmt.Errorf("failed to drop database %s: %w", dbName, err)
	}
	if _, err := adminDB.Exec(fmt.Sprintf(
		`DROP ROLE IF EXISTS %s; DROP ROLE IF EXISTS %s`,
		quoteIdent(mcpUser), quoteIdent(dbUser),
	)); err != nil {
		slog.Warn("dropped database but failed to drop roles",
			"mcp_user", mcpUser, "db_user", dbUser, "error", err)
	}
	return nil
}

// dropProvisioned is a best-effort cleanup of a partially provisioned datapack.
func dropProvisioned(adminDB *sql.DB, dbName, mcpUser, dbUser string) {
	_ = DropDatapackDB(adminDB, dbName, mcpUser, dbUser)
}

// List returns datapacks belonging to the given account.
func (r *DatapackRepo) List(accountID string) ([]models.Datapack, error) {
	rows, err := r.db.Query(
		`SELECT id, account_id, datapack_name,
		        yb_universe_id, datapack_ysql_connection_string,
		        mcp_user_name, db_user_name,
		        created_at, updated_at
		 FROM meko_system.datapack_registry
		 WHERE account_id = $1
		 ORDER BY created_at DESC`,
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

// GetByName returns a single datapack by name.
func (r *DatapackRepo) GetByName(name string) (*models.Datapack, error) {
	var d models.Datapack
	err := r.db.QueryRow(
		`SELECT id, account_id, datapack_name,
		        yb_universe_id, datapack_ysql_connection_string,
		        mcp_user_name, db_user_name,
		        created_at, updated_at
		 FROM meko_system.datapack_registry
		 WHERE datapack_name = $1`,
		name,
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
	name, ybConnStr string,
) error {
	if ybConnStr == "" {
		return nil
	}

	res, err := r.db.Exec(
		`UPDATE meko_system.datapack_registry
		 SET datapack_ysql_connection_string = $1, updated_at = NOW()
		 WHERE datapack_name = $2`,
		ybConnStr, name,
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

// Delete removes a datapack by name. CASCADE handles child rows.
func (r *DatapackRepo) Delete(name string) error {
	res, err := r.db.Exec(
		`DELETE FROM meko_system.datapack_registry
		 WHERE datapack_name = $1`, name,
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
func (r *DatapackRepo) GetStatus(name string) (*DatapackStatus, error) {
	var s DatapackStatus
	err := r.db.QueryRow(
		`SELECT d.id, d.datapack_name,
		        d.yb_universe_id, d.datapack_ysql_connection_string,
		        d.mcp_user_name, d.db_user_name,
		        d.created_at,
		        (SELECT COUNT(*) FROM meko_system.agent_registry
		         WHERE datapack_id = d.id),
		        (SELECT COUNT(*)
		         FROM meko_system.datapack_knowledge_base_registry
		         WHERE datapack_id = d.id)
		 FROM meko_system.datapack_registry d
		 WHERE d.datapack_name = $1`, name,
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
