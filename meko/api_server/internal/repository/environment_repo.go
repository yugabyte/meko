package repository

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"

	"meko/api_server/internal/models"
)

// EnvironmentRepo persists the YCP fleet registry (meko_system.environments and
// meko_system.environment_desired_resources). Control-plane mode only.
//
// Rows are keyed by uuid; id is the operator-facing slug, unique only within an
// account. Any lookup starting from an id therefore needs the account, or another
// way to tell two accounts' environments apart -- see Get and GetByAgentToken.
type EnvironmentRepo struct {
	db *sql.DB
}

// NewEnvironmentRepo constructs the repo over the system DB.
func NewEnvironmentRepo(db *sql.DB) *EnvironmentRepo {
	return &EnvironmentRepo{db: db}
}

// ErrAmbiguousID is returned when an unscoped lookup by id matches more than one
// account's environment. Impossible for rows predating the per-account unique
// index, since ids were globally unique then.
var ErrAmbiguousID = errors.New("environment id belongs to more than one account")

const envColumns = `uuid, id, account_id, placement, region, state, k8s_version, node_count, provider,
	datapacks, max_datapacks, agent_version, last_heartbeat_at, mcp_base_url, cert_fingerprint,
	COALESCE(components::text, '[]'), live, enroll_token_hash, enroll_token_expires_at, status_token_hash, created_at,
	COALESCE(kind, 'meko'), COALESCE(resources::text, '[]')`

func scanEnvironment(row interface{ Scan(...any) error }) (*models.Environment, error) {
	var e models.Environment
	var region, k8sVersion, provider, agentVersion, mcpBaseURL, certFingerprint sql.NullString
	var lastHeartbeat, enrollExpires sql.NullTime
	var componentsText, resourcesText string
	if err := row.Scan(&e.UUID, &e.ID, &e.AccountID, &e.Placement, &region, &e.State, &k8sVersion,
		&e.NodeCount, &provider, &e.Datapacks, &e.MaxDatapacks, &agentVersion, &lastHeartbeat,
		&mcpBaseURL, &certFingerprint, &componentsText, &e.Live, &e.EnrollTokenHash, &enrollExpires,
		&e.StatusTokenHash, &e.CreatedAt, &e.Kind, &resourcesText); err != nil {
		return nil, err
	}
	if enrollExpires.Valid {
		t := enrollExpires.Time
		e.EnrollTokenExpiresAt = &t
	}
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
	e.Resources = json.RawMessage(resourcesText)
	return &e, nil
}

// scanOne maps the not-found row to (nil, nil), which every caller treats as a 404.
func scanOne(row *sql.Row) (*models.Environment, error) {
	e, err := scanEnvironment(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get environment: %w", err)
	}
	return e, nil
}

// GetByUUID returns the environment by its key, or (nil, nil) if not found.
func (r *EnvironmentRepo) GetByUUID(envUUID string) (*models.Environment, error) {
	return scanOne(r.db.QueryRow(
		`SELECT `+envColumns+` FROM meko_system.environments WHERE uuid = $1`, envUUID))
}

// Get resolves an operator-facing id, or (nil, nil) if not found.
//
// With an account it is an exact hit on the (account_id, id) unique index. Without
// one -- the console, the e2e, an operator's curl -- it matches on id across every
// account, which stays unambiguous until two accounts pick the same name; then it
// returns ErrAmbiguousID rather than guessing which environment was meant.
func (r *EnvironmentRepo) Get(accountID, id string) (*models.Environment, error) {
	if accountID != "" {
		return scanOne(r.db.QueryRow(
			`SELECT `+envColumns+` FROM meko_system.environments WHERE account_id = $1 AND id = $2`,
			accountID, id))
	}
	// LIMIT 2 distinguishes "one" from "more than one" without reading the rest.
	rows, err := r.db.Query(`SELECT `+envColumns+` FROM meko_system.environments WHERE id = $1 LIMIT 2`, id)
	if err != nil {
		return nil, fmt.Errorf("get environment: %w", err)
	}
	defer rows.Close()
	var found *models.Environment
	for rows.Next() {
		e, err := scanEnvironment(rows)
		if err != nil {
			return nil, fmt.Errorf("scan environment: %w", err)
		}
		if found != nil {
			return nil, fmt.Errorf("%w: %q", ErrAmbiguousID, id)
		}
		found = e
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("get environment: %w", err)
	}
	return found, nil
}

// GetByAgentToken resolves the environment an agent is reporting for. The agent
// knows only its id, which no longer identifies a row on its own, so the status
// token it authenticates with is what disambiguates. Returns (nil, nil) when
// nothing matches both -- which is also how an invalid token surfaces.
func (r *EnvironmentRepo) GetByAgentToken(id, statusTokenHash string) (*models.Environment, error) {
	if statusTokenHash == "" {
		return nil, nil
	}
	return scanOne(r.db.QueryRow(
		`SELECT `+envColumns+` FROM meko_system.environments WHERE id = $1 AND status_token_hash = $2`,
		id, statusTokenHash))
}

// List returns environments, live ones first. A non-empty accountID restricts the
// result to that account; empty returns the whole fleet.
func (r *EnvironmentRepo) List(accountID string) ([]*models.Environment, error) {
	query := `SELECT ` + envColumns + ` FROM meko_system.environments`
	var args []any
	if accountID != "" {
		query += ` WHERE account_id = $1`
		args = append(args, accountID)
	}
	query += ` ORDER BY live DESC, id ASC`
	rows, err := r.db.Query(query, args...)
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

const envColumnsInsert = `id, account_id, placement, region, state, k8s_version, node_count, provider,
	datapacks, max_datapacks, agent_version, last_heartbeat_at, mcp_base_url, cert_fingerprint,
	components, live, enroll_token_hash, enroll_token_expires_at, status_token_hash, created_at,
	kind, resources`

// Insert registers an environment, or re-registers the caller's own, and sets
// e.UUID to the key of the row that resulted.
//
// The conflict target is (account_id, id), so a name another account holds is not a
// conflict at all -- it inserts that account's own environment. Only re-registering
// your own name updates a row, which is what registration always did.
func (r *EnvironmentRepo) Insert(e *models.Environment) error {
	components := e.Components
	if len(components) == 0 {
		components = json.RawMessage("[]")
	}
	resources := e.Resources
	if len(resources) == 0 {
		resources = json.RawMessage("[]")
	}
	kind := e.Kind
	if kind == "" {
		kind = "meko"
	}
	err := r.db.QueryRow(`
		INSERT INTO meko_system.environments (`+envColumnsInsert+`)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21,$22)
		ON CONFLICT (account_id, id) DO UPDATE SET
			placement=$3, region=$4, state=$5, k8s_version=$6, node_count=$7,
			provider=$8, datapacks=$9, max_datapacks=$10, agent_version=$11, last_heartbeat_at=$12,
			mcp_base_url=$13, cert_fingerprint=$14, components=$15, live=$16,
			enroll_token_hash=$17, enroll_token_expires_at=$18, status_token_hash=$19,
			kind=$21, resources=$22
		RETURNING uuid`,
		e.ID, e.AccountID, e.Placement, nullStr(e.Region), e.State, nullStr(e.K8sVersion), e.NodeCount,
		nullStr(e.Provider), e.Datapacks, e.MaxDatapacks, nullStr(e.AgentVersion), e.LastHeartbeatAt,
		nullStr(e.MCPBaseURL), nullStr(e.CertFingerprint), []byte(components), e.Live,
		e.EnrollTokenHash, e.EnrollTokenExpiresAt, e.StatusTokenHash, e.CreatedAt, kind,
		[]byte(resources)).Scan(&e.UUID)
	if err != nil {
		slog.Error("insert environment", "id", e.ID, "account", e.AccountID, "err", err)
		return fmt.Errorf("insert environment: %w", err)
	}
	return nil
}

// Update writes an existing environment back by its key. Every caller has already
// read the row; id and account_id are not written, because neither is editable.
func (r *EnvironmentRepo) Update(e *models.Environment) error {
	components := e.Components
	if len(components) == 0 {
		components = json.RawMessage("[]")
	}
	resources := e.Resources
	if len(resources) == 0 {
		resources = json.RawMessage("[]")
	}
	kind := e.Kind
	if kind == "" {
		kind = "meko"
	}
	_, err := r.db.Exec(`
		UPDATE meko_system.environments SET
			placement=$2, region=$3, state=$4, k8s_version=$5, node_count=$6,
			provider=$7, datapacks=$8, max_datapacks=$9, agent_version=$10, last_heartbeat_at=$11,
			mcp_base_url=$12, cert_fingerprint=$13, components=$14, live=$15,
			enroll_token_hash=$16, enroll_token_expires_at=$17, status_token_hash=$18,
			kind=$19, resources=$20
		WHERE uuid = $1`,
		e.UUID, e.Placement, nullStr(e.Region), e.State, nullStr(e.K8sVersion), e.NodeCount,
		nullStr(e.Provider), e.Datapacks, e.MaxDatapacks, nullStr(e.AgentVersion), e.LastHeartbeatAt,
		nullStr(e.MCPBaseURL), nullStr(e.CertFingerprint), []byte(components), e.Live,
		e.EnrollTokenHash, e.EnrollTokenExpiresAt, e.StatusTokenHash, kind, []byte(resources))
	if err != nil {
		slog.Error("update environment", "uuid", e.UUID, "err", err)
		return fmt.Errorf("update environment: %w", err)
	}
	return nil
}

// EnrollLookup returns the uuid of the (single) environment holding a non-expired
// enrollment token with the given hash; found=false when none matches.
func (r *EnvironmentRepo) EnrollLookup(tokenHash string) (envUUID string, found bool, err error) {
	err = r.db.QueryRow(
		`SELECT uuid FROM meko_system.environments WHERE enroll_token_hash = $1 AND enroll_token_expires_at > now() LIMIT 1`,
		tokenHash).Scan(&envUUID)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("enroll lookup: %w", err)
	}
	return envUUID, true, nil
}

// ReplaceDesired atomically replaces the desired resource set for an environment,
// maintaining a per-resource generation. A resource whose spec is unchanged keeps
// its generation (so the agent skips it); a changed spec bumps it (so the agent
// re-applies); a new resource starts at 0; a withdrawn resource is deleted. This
// is an upsert-and-prune rather than a delete-and-reinsert precisely so the
// generation survives a replace. The spec comparison is JSONB `IS DISTINCT FROM`,
// so it is semantic (key order / whitespace do not spuriously bump the generation).
//
// New rows start at 0, not 1, on purpose: a backend that cannot echo the applied
// generation back on List (the amp backend) relies on the engine's in-process
// cache to skip no-op ticks, and that cache is empty after a restart. At
// generation 0 the diff is a clean no-op cache-cold; a resource only crosses into
// "re-apply" territory once its spec has actually changed.
func (r *EnvironmentRepo) ReplaceDesired(envUUID string, res []models.DesiredResource) error {
	tx, err := r.db.Begin()
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	// Existing keys, so we can prune the ones no longer desired.
	type key struct{ kind, name string }
	existing := map[key]bool{}
	const listExisting = `SELECT kind, name FROM meko_system.environment_desired_resources WHERE env_uuid = $1`
	rows, err := tx.Query(listExisting, envUUID)
	if err != nil {
		return fmt.Errorf("list existing desired: %w", err)
	}
	for rows.Next() {
		var k key
		if err := rows.Scan(&k.kind, &k.name); err != nil {
			rows.Close()
			return fmt.Errorf("scan existing desired: %w", err)
		}
		existing[k] = true
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate existing desired: %w", err)
	}

	desired := make(map[key]bool, len(res))
	for _, d := range res {
		spec := d.Spec
		if len(spec) == 0 {
			spec = json.RawMessage("{}")
		}
		if _, err := tx.Exec(
			`INSERT INTO meko_system.environment_desired_resources (env_uuid, kind, name, spec, generation)
			 VALUES ($1,$2,$3,$4,0)
			 ON CONFLICT (env_uuid, kind, name) DO UPDATE
			   SET spec = EXCLUDED.spec,
			       generation = CASE
			         WHEN meko_system.environment_desired_resources.spec IS DISTINCT FROM EXCLUDED.spec
			         THEN meko_system.environment_desired_resources.generation + 1
			         ELSE meko_system.environment_desired_resources.generation
			       END`,
			envUUID, d.Kind, d.Name, []byte(spec)); err != nil {
			return fmt.Errorf("upsert desired: %w", err)
		}
		desired[key{d.Kind, d.Name}] = true
	}
	for k := range existing {
		if desired[k] {
			continue
		}
		const prune = `DELETE FROM meko_system.environment_desired_resources
			 WHERE env_uuid=$1 AND kind=$2 AND name=$3`
		if _, err := tx.Exec(prune, envUUID, k.kind, k.name); err != nil {
			return fmt.Errorf("prune desired %s/%s: %w", k.kind, k.name, err)
		}
	}
	return tx.Commit()
}

// ListDesired returns the desired resources for an environment.
func (r *EnvironmentRepo) ListDesired(envUUID string) ([]models.DesiredResource, error) {
	rows, err := r.db.Query(
		`SELECT kind, name, COALESCE(spec::text, '{}'), generation FROM meko_system.environment_desired_resources
		 WHERE env_uuid = $1 ORDER BY kind, name`, envUUID)
	if err != nil {
		return nil, fmt.Errorf("list desired: %w", err)
	}
	defer rows.Close()
	var out []models.DesiredResource
	for rows.Next() {
		var d models.DesiredResource
		var spec string
		if err := rows.Scan(&d.Kind, &d.Name, &spec, &d.Generation); err != nil {
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
