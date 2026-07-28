package repository

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"

	"meko/api_server/internal/models"
)

// EnvironmentRepo persists the MCCP fleet registry (meko_system.environments and
// meko_system.environment_desired_resources). Control-plane mode only.
type EnvironmentRepo struct {
	db *sql.DB
}

// NewEnvironmentRepo constructs the repo over the system DB.
func NewEnvironmentRepo(db *sql.DB) *EnvironmentRepo {
	return &EnvironmentRepo{db: db}
}

const envColumns = `id, account_id, placement, region, state, k8s_version, node_count, provider,
	datapacks, max_datapacks, agent_version, last_heartbeat_at, mcp_base_url, cert_fingerprint,
	COALESCE(components::text, '[]'), live, enroll_token_hash, enroll_token_expires_at, status_token_hash, created_at`

func scanEnvironment(row interface{ Scan(...any) error }) (*models.Environment, error) {
	var e models.Environment
	var accountID, region, k8sVersion, provider, agentVersion, mcpBaseURL, certFingerprint sql.NullString
	var lastHeartbeat, enrollExpires sql.NullTime
	var componentsText string
	if err := row.Scan(&e.ID, &accountID, &e.Placement, &region, &e.State, &k8sVersion, &e.NodeCount, &provider,
		&e.Datapacks, &e.MaxDatapacks, &agentVersion, &lastHeartbeat, &mcpBaseURL, &certFingerprint,
		&componentsText, &e.Live, &e.EnrollTokenHash, &enrollExpires, &e.StatusTokenHash, &e.CreatedAt); err != nil {
		return nil, err
	}
	if enrollExpires.Valid {
		t := enrollExpires.Time
		e.EnrollTokenExpiresAt = &t
	}
	e.AccountID = accountID.String
	e.Region = region.String
	e.K8sVersion = k8sVersion.String
	e.Provider = provider.String
	e.AgentVersion = agentVersion.String
	e.MCPBaseURL = mcpBaseURL.String
	e.CertFingerprint = certFingerprint.String
	if lastHeartbeat.Valid {
		t := lastHeartbeat.Time
		e.LastHeartbeatAt = &t
	}
	e.Components = json.RawMessage(componentsText)
	return &e, nil
}

// Get returns the environment by id, or (nil, nil) if not found.
func (r *EnvironmentRepo) Get(id string) (*models.Environment, error) {
	row := r.db.QueryRow(`SELECT `+envColumns+` FROM meko_system.environments WHERE id = $1`, id)
	e, err := scanEnvironment(row)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get environment: %w", err)
	}
	return e, nil
}

// List returns all environments, live ones first.
func (r *EnvironmentRepo) List() ([]*models.Environment, error) {
	rows, err := r.db.Query(`SELECT ` + envColumns + ` FROM meko_system.environments ORDER BY live DESC, id ASC`)
	if err != nil {
		return nil, fmt.Errorf("list environments: %w", err)
	}
	defer rows.Close()
	var out []*models.Environment
	for rows.Next() {
		e, err := scanEnvironment(rows)
		if err != nil {
			return nil, fmt.Errorf("scan environment: %w", err)
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// Upsert inserts or updates an environment by id.
func (r *EnvironmentRepo) Upsert(e *models.Environment) error {
	components := e.Components
	if len(components) == 0 {
		components = json.RawMessage("[]")
	}
	_, err := r.db.Exec(`
		INSERT INTO meko_system.environments (`+envColumnsInsert+`)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20)
		ON CONFLICT (id) DO UPDATE SET
			account_id=$2, placement=$3, region=$4, state=$5, k8s_version=$6, node_count=$7,
			provider=$8, datapacks=$9, max_datapacks=$10, agent_version=$11, last_heartbeat_at=$12,
			mcp_base_url=$13, cert_fingerprint=$14, components=$15, live=$16,
			enroll_token_hash=$17, enroll_token_expires_at=$18, status_token_hash=$19`,
		e.ID, nullStr(e.AccountID), e.Placement, nullStr(e.Region), e.State, nullStr(e.K8sVersion), e.NodeCount,
		nullStr(e.Provider), e.Datapacks, e.MaxDatapacks, nullStr(e.AgentVersion), e.LastHeartbeatAt,
		nullStr(e.MCPBaseURL), nullStr(e.CertFingerprint), []byte(components), e.Live,
		e.EnrollTokenHash, e.EnrollTokenExpiresAt, e.StatusTokenHash, e.CreatedAt)
	if err != nil {
		slog.Error("upsert environment", "id", e.ID, "err", err)
		return fmt.Errorf("upsert environment: %w", err)
	}
	return nil
}

const envColumnsInsert = `id, account_id, placement, region, state, k8s_version, node_count, provider,
	datapacks, max_datapacks, agent_version, last_heartbeat_at, mcp_base_url, cert_fingerprint,
	components, live, enroll_token_hash, enroll_token_expires_at, status_token_hash, created_at`

// EnrollLookup returns the id of the (single) environment holding a non-expired
// enrollment token with the given hash; found=false when none matches.
func (r *EnvironmentRepo) EnrollLookup(tokenHash string) (id string, found bool, err error) {
	err = r.db.QueryRow(
		`SELECT id FROM meko_system.environments WHERE enroll_token_hash = $1 AND enroll_token_expires_at > now() LIMIT 1`,
		tokenHash).Scan(&id)
	if err == sql.ErrNoRows {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("enroll lookup: %w", err)
	}
	return id, true, nil
}

// ReplaceDesired atomically replaces the desired resource set for an environment.
func (r *EnvironmentRepo) ReplaceDesired(envID string, res []models.DesiredResource) error {
	tx, err := r.db.Begin()
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.Exec(`DELETE FROM meko_system.environment_desired_resources WHERE env_id = $1`, envID); err != nil {
		return fmt.Errorf("clear desired: %w", err)
	}
	for _, d := range res {
		spec := d.Spec
		if len(spec) == 0 {
			spec = json.RawMessage("{}")
		}
		if _, err := tx.Exec(
			`INSERT INTO meko_system.environment_desired_resources (env_id, kind, name, spec)
			 VALUES ($1,$2,$3,$4) ON CONFLICT (env_id, kind, name) DO UPDATE SET spec = EXCLUDED.spec`,
			envID, d.Kind, d.Name, []byte(spec)); err != nil {
			return fmt.Errorf("insert desired: %w", err)
		}
	}
	return tx.Commit()
}

// ListDesired returns the desired resources for an environment.
func (r *EnvironmentRepo) ListDesired(envID string) ([]models.DesiredResource, error) {
	rows, err := r.db.Query(
		`SELECT kind, name, COALESCE(spec::text, '{}') FROM meko_system.environment_desired_resources
		 WHERE env_id = $1 ORDER BY kind, name`, envID)
	if err != nil {
		return nil, fmt.Errorf("list desired: %w", err)
	}
	defer rows.Close()
	var out []models.DesiredResource
	for rows.Next() {
		var d models.DesiredResource
		var spec string
		if err := rows.Scan(&d.Kind, &d.Name, &spec); err != nil {
			return nil, fmt.Errorf("scan desired: %w", err)
		}
		d.Spec = json.RawMessage(spec)
		out = append(out, d)
	}
	return out, rows.Err()
}

func nullStr(s string) any {
	if s == "" {
		return nil
	}
	return s
}
