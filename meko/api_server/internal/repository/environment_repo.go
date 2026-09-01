package repository

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/lib/pq"

	"meko/api_server/internal/models"
)

// EnvironmentRepo persists the YCP fleet registry (meko_system.environments,
// meko_system.environment_api_keys and meko_system.environment_desired_resources).
// Control-plane mode only.
//
// Rows are keyed by uuid; id is the operator-facing slug, unique only within an
// account. Any lookup starting from an id therefore needs the account, or another
// way to tell two accounts' environments apart -- see Get. Agent calls avoid the
// problem entirely: they resolve through environment_api_keys, whose primary key is
// the api key id.
//
// The key bindings are written by Insert and read through envColumns.
type EnvironmentRepo struct {
	db *sql.DB
}

// NewEnvironmentRepo constructs the repo over the system DB.
func NewEnvironmentRepo(db *sql.DB) *EnvironmentRepo {
	return &EnvironmentRepo{db: db}
}

// unaccountedAccount is what account_id holds for an environment that belongs to no
// account. A sentinel rather than NULL, because NULLs compare as distinct inside
// environments_account_id_uniq and would let unaccounted rows share a name.
//
// It is confined to this file: everything above the repository uses "" for
// unaccounted, and these two helpers translate at the boundary.
const unaccountedAccount = "00000000-0000-0000-0000-000000000000"

func accountForStore(accountID string) string {
	if accountID == "" {
		return unaccountedAccount
	}
	return accountID
}

func accountFromStore(accountID string) string {
	if accountID == unaccountedAccount {
		return ""
	}
	return accountID
}

// ErrAmbiguousID is returned when an unscoped lookup by id matches more than one
// account's environment. Impossible for rows predating the per-account unique
// index, since ids were globally unique then.
var ErrAmbiguousID = errors.New("environment id belongs to more than one account")

// envColumns reads an environment together with the id of the key its agent
// authenticates with. That id comes from a scalar subquery, not a join: an
// environment can hold more than one key row, and a join would return the
// environment once per key.
//
// Every column is qualified with the `e` alias the subquery correlates on, so a
// query selecting these must alias meko_system.environments as `e`.
const envColumns = `e.uuid, e.id, e.account_id, e.placement, e.region, e.state, e.k8s_version,
	e.node_count, e.provider, e.datapacks, e.max_datapacks, e.agent_version, e.last_heartbeat_at,
	e.mcp_base_url, e.cert_fingerprint, COALESCE(e.components::text, '[]'), e.live,
	(SELECT api_key_id FROM meko_system.environment_api_keys
	   WHERE env_uuid = e.uuid AND status = 'active'),
	e.created_at, COALESCE(e.kind, 'meko'), COALESCE(e.resources::text, '[]'), e.agent_base_url`

func scanEnvironment(row interface{ Scan(...any) error }) (*models.Environment, error) {
	var e models.Environment
	var region, k8sVersion, provider, agentVersion, mcpBaseURL, certFingerprint, agentBaseURL sql.NullString
	var apiKeyID sql.NullString
	var lastHeartbeat sql.NullTime
	var componentsText, resourcesText string
	if err := row.Scan(&e.UUID, &e.ID, &e.AccountID, &e.Placement, &region, &e.State, &k8sVersion,
		&e.NodeCount, &provider, &e.Datapacks, &e.MaxDatapacks, &agentVersion, &lastHeartbeat,
		&mcpBaseURL, &certFingerprint, &componentsText, &e.Live, &apiKeyID,
		&e.CreatedAt, &e.Kind, &resourcesText, &agentBaseURL); err != nil {
		return nil, err
	}
	e.AccountID = accountFromStore(e.AccountID)
	e.APIKeyID = apiKeyID.String
	e.Region = region.String
	e.K8sVersion = k8sVersion.String
	e.Provider = provider.String
	e.AgentVersion = agentVersion.String
	e.MCPBaseURL = mcpBaseURL.String
	e.CertFingerprint = certFingerprint.String
	e.AgentBaseURL = agentBaseURL.String
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
		`SELECT `+envColumns+` FROM meko_system.environments e WHERE e.uuid = $1`, envUUID))
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
			`SELECT `+envColumns+` FROM meko_system.environments e WHERE e.account_id = $1 AND e.id = $2`,
			accountForStore(accountID), id))
	}
	// LIMIT 2 distinguishes "one" from "more than one" without reading the rest.
	rows, err := r.db.Query(
		`SELECT `+envColumns+` FROM meko_system.environments e WHERE e.id = $1 LIMIT 2`, id)
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

// GetByAPIKeyID resolves the environment an agent is calling for, by the key id
// YBM forwarded. Status is not part of the lookup, so a retiring key resolves as
// readily as an active one -- that is what lets an agent keep working until it
// picks up a replacement.
//
// (nil, nil) when the key is bound to nothing, which is also how a revoked or
// never-registered key surfaces.
func (r *EnvironmentRepo) GetByAPIKeyID(apiKeyID string) (*models.Environment, error) {
	return scanOne(r.db.QueryRow(
		`SELECT `+envColumns+` FROM meko_system.environments e
		 WHERE e.uuid = (SELECT env_uuid FROM meko_system.environment_api_keys WHERE api_key_id = $1)`,
		apiKeyID))
}

// List returns environments, live ones first. A non-empty accountID restricts the
// result to that account; empty returns the whole fleet.
func (r *EnvironmentRepo) List(accountID string) ([]*models.Environment, error) {
	query := `SELECT ` + envColumns + ` FROM meko_system.environments e`
	var args []any
	if accountID != "" {
		query += ` WHERE e.account_id = $1`
		args = append(args, accountForStore(accountID))
	}
	query += ` ORDER BY e.live DESC, e.id ASC`
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
	components, live, created_at,
	kind, resources, agent_base_url`

// ErrAPIKeyBound is returned when a key binding would attach an api key that
// another environment already holds, which environment_api_keys' primary key
// forbids: one key drives one environment, whichever status it holds. Mapped to 409 by
// handlers.
var ErrAPIKeyBound = errors.New("api key is already bound to another environment")

// apiKeyUniqueConstraint is the index whose violation means ErrAPIKeyBound.
const apiKeyUniqueConstraint = "environment_api_keys_pkey"

// ErrEnvironmentExists is returned when a registration's (account_id, id) is
// already taken, which environments_account_id_uniq forbids. Mapped to 409 by
// handlers. The name stays taken for the life of the row, deregistered or not.
var ErrEnvironmentExists = errors.New("environment already registered")

// accountIDUniqueConstraint is the index whose violation means ErrEnvironmentExists.
const accountIDUniqueConstraint = "environments_account_id_uniq"

// Insert registers a brand-new environment with e.APIKeyID as its primary key
// binding, and sets e.UUID to the generated row key. A name already claimed by this
// account returns ErrEnvironmentExists, and a key already bound to another
// environment returns ErrAPIKeyBound -- one key drives one environment.
//
// The environment row and its key binding go in one transaction: an environment with
// no key binding is one no agent can ever attach to.
//
// Plain INSERTs relying on the unique indexes need no lock: the loser of a race
// between two registrations of the same (account_id, id) simply gets
// ErrEnvironmentExists.
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
	account := accountForStore(e.AccountID)

	tx, err := r.db.Begin()
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	err = tx.QueryRow(`
		INSERT INTO meko_system.environments (`+envColumnsInsert+`)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20)
		RETURNING uuid`,
		e.ID, account, e.Placement, nullStr(e.Region), e.State, nullStr(e.K8sVersion), e.NodeCount,
		nullStr(e.Provider), e.Datapacks, e.MaxDatapacks, nullStr(e.AgentVersion), e.LastHeartbeatAt,
		nullStr(e.MCPBaseURL), nullStr(e.CertFingerprint), []byte(components), e.Live,
		e.CreatedAt, kind,
		[]byte(resources), nullStr(e.AgentBaseURL)).Scan(&e.UUID)
	if err != nil {
		if isUniqueViolationOf(err, accountIDUniqueConstraint) {
			return fmt.Errorf("%w: %s", ErrEnvironmentExists, e.ID)
		}
		slog.Error("insert environment", "id", e.ID, "account", e.AccountID, "err", err)
		return fmt.Errorf("insert environment: %w", err)
	}
	if _, err := tx.Exec(insertPrimaryKeyBinding, e.APIKeyID, e.UUID); err != nil {
		if isUniqueViolationOf(err, apiKeyUniqueConstraint) {
			return fmt.Errorf("%w: %s", ErrAPIKeyBound, e.APIKeyID)
		}
		slog.Error("bind environment api key", "id", e.ID, "err", err)
		return fmt.Errorf("bind environment api key: %w", err)
	}
	return tx.Commit()
}

// insertPrimaryKeyBinding attaches a key to an environment as its live credential.
const insertPrimaryKeyBinding = `
	INSERT INTO meko_system.environment_api_keys (api_key_id, env_uuid, status)
	VALUES ($1, $2, 'active')`

// isUniqueViolationOf reports whether err is a unique violation (SQLSTATE 23505) of
// an index whose name contains constraint. Matched on the driver error's code and
// constraint name rather than on message text, which is not a stable contract; by
// name so a violation of any OTHER unique index still surfaces as itself.
func isUniqueViolationOf(err error, constraint string) bool {
	var pqErr *pq.Error
	return errors.As(err, &pqErr) && pqErr.Code == "23505" &&
		strings.Contains(pqErr.Constraint, constraint)
}

// Update writes an existing environment back by its key. Every caller has already
// read the row; id and account_id are not written because they are not editable.
// The key bindings are not written either -- they live in their own table, so a
// status heartbeat cannot disturb them.
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
			kind=$16, resources=$17
		WHERE uuid = $1`,
		e.UUID, e.Placement, nullStr(e.Region), e.State, nullStr(e.K8sVersion), e.NodeCount,
		nullStr(e.Provider), e.Datapacks, e.MaxDatapacks, nullStr(e.AgentVersion), e.LastHeartbeatAt,
		nullStr(e.MCPBaseURL), nullStr(e.CertFingerprint), []byte(components), e.Live,
		kind, []byte(resources))
	if err != nil {
		slog.Error("update environment", "uuid", e.UUID, "err", err)
		return fmt.Errorf("update environment: %w", err)
	}
	return nil
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
