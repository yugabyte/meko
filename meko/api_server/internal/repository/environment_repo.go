package repository

import (
	"bytes"
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
	e.created_at, COALESCE(e.kind, 'meko'), COALESCE(e.resources::text, '[]'), e.agent_base_url,
	COALESCE(e.type, 'BYOC')`

func scanEnvironment(row interface{ Scan(...any) error }) (*models.Environment, error) {
	var e models.Environment
	var region, k8sVersion, provider, agentVersion, mcpBaseURL, certFingerprint, agentBaseURL sql.NullString
	var apiKeyID sql.NullString
	var lastHeartbeat sql.NullTime
	var componentsText, resourcesText string
	if err := row.Scan(&e.UUID, &e.ID, &e.AccountID, &e.Placement, &region, &e.State, &k8sVersion,
		&e.NodeCount, &provider, &e.Datapacks, &e.MaxDatapacks, &agentVersion, &lastHeartbeat,
		&mcpBaseURL, &certFingerprint, &componentsText, &e.Live, &apiKeyID,
		&e.CreatedAt, &e.Kind, &resourcesText, &agentBaseURL, &e.Type); err != nil {
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
	kind, resources, agent_base_url, type`

// ErrAPIKeyBound is returned when a key binding would attach an api key that
// another environment already holds, which environment_api_keys' primary key
// forbids: one key drives one environment, whichever status it holds. Mapped to 409 by
// handlers.
var ErrAPIKeyBound = errors.New("api key is already bound to another environment")

// ErrEnvironmentExists is returned when a registration's (account_id, id) is
// already taken, which environments_account_id_uniq forbids. Mapped to 409 by
// handlers. The name stays taken for the life of the row, deregistered or not.
var ErrEnvironmentExists = errors.New("environment already registered")

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
	components := jsonArray(e.Components)
	resources := jsonArray(e.Resources)
	kind := e.Kind
	if kind == "" {
		kind = "meko"
	}
	typ := e.Type
	if typ == "" {
		typ = "BYOC"
	}
	account := accountForStore(e.AccountID)

	tx, err := r.db.Begin()
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	err = tx.QueryRow(`
		INSERT INTO meko_system.environments (`+envColumnsInsert+`)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21)
		RETURNING uuid`,
		e.ID, account, e.Placement, nullStr(e.Region), e.State, nullStr(e.K8sVersion), e.NodeCount,
		nullStr(e.Provider), e.Datapacks, e.MaxDatapacks, nullStr(e.AgentVersion), e.LastHeartbeatAt,
		nullStr(e.MCPBaseURL), nullStr(e.CertFingerprint), []byte(components), e.Live,
		e.CreatedAt, kind,
		[]byte(resources), nullStr(e.AgentBaseURL), typ).Scan(&e.UUID)
	if err != nil {
		// A 23505 from THIS statement can only be environments_account_id_uniq: the
		// table's other unique key is its primary key, whose uuid the INSERT never
		// supplies -- gen_random_uuid() fills it.
		if isUniqueViolation23505(err) {
			return fmt.Errorf("%w: %s", ErrEnvironmentExists, e.ID)
		}
		slog.Error("insert environment", "id", e.ID, "account", e.AccountID, "err", err)
		return fmt.Errorf("insert environment: %w", err)
	}
	if _, err := tx.Exec(insertPrimaryKeyBinding, e.APIKeyID, e.UUID); err != nil {
		// Likewise: the other unique key on environment_api_keys is (env_uuid, status),
		// and e.UUID is the row created just above, so no pre-existing row can collide
		// on it. Only the api_key_id primary key is reachable here.
		if isUniqueViolation23505(err) {
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

// isUniqueViolation23505 reports whether err is a unique violation. WHICH index was
// violated is the caller's to know from the statement that raised it, because no
// portable field carries the name: upstream PostgreSQL raises it through
// errtableconstraint(), which fills the wire protocol's `n` field that lib/pq exposes
// as pq.Error.Constraint, while YSQL builds the same error in pg_yb_utils.c and only
// formats the name into the message, leaving Constraint and Detail empty. Keying on
// that field is what made every duplicate registration a 500 instead of a 409
// (CLOUDGA-36978), and keying on the message text instead would just trade a silent
// break for a brittle one. https://github.com/yugabyte/amp/issues/400 is the same bug
// -- see amp/controller/internal/task/store.go. (A bare "#400" would resolve to an
// unrelated meko PR: amp's issues were renumbered by the monorepo migration.)
//
// Named apart from sharing_repo.go's isUniqueViolation, which does match on text.
func isUniqueViolation23505(err error) bool {
	var pqErr *pq.Error
	return errors.As(err, &pqErr) && pqErr.Code == "23505"
}

// Update writes an existing environment back by its key. Every caller has already
// read the row; id and account_id are not written because they are not editable.
// The key bindings are not written either -- they live in their own table, so a
// status heartbeat cannot disturb them.
func (r *EnvironmentRepo) Update(e *models.Environment) error {
	components := jsonArray(e.Components)
	resources := jsonArray(e.Resources)
	kind := e.Kind
	if kind == "" {
		kind = "meko"
	}
	typ := e.Type
	if typ == "" {
		typ = "BYOC"
	}
	_, err := r.db.Exec(`
		UPDATE meko_system.environments SET
			placement=$2, region=$3, state=$4, k8s_version=$5, node_count=$6,
			provider=$7, datapacks=$8, max_datapacks=$9, agent_version=$10, last_heartbeat_at=$11,
			mcp_base_url=$12, cert_fingerprint=$13, components=$14, live=$15,
			kind=$16, resources=$17, type=$18
		WHERE uuid = $1`,
		e.UUID, e.Placement, nullStr(e.Region), e.State, nullStr(e.K8sVersion), e.NodeCount,
		nullStr(e.Provider), e.Datapacks, e.MaxDatapacks, nullStr(e.AgentVersion), e.LastHeartbeatAt,
		nullStr(e.MCPBaseURL), nullStr(e.CertFingerprint), []byte(components), e.Live,
		kind, []byte(resources), typ)
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

// applyDesiredPatchesTx applies a batch of targeted desired-state changes on an
// EXISTING transaction, so a caller can span several environments in one atomic call
// (the storage-pool region PATCH does). It neither begins nor commits: the caller
// owns the transaction boundary and its rollback. Items are applied in slice order,
// so two items naming the same (kind, name) compose as a shallow-merge sequence (the
// second reads back what the first wrote).
//
// Each item is a cascade subtree delete (Delete true) or a partial-spec write. A
// write honors the intent flags: Create asserts the resource is absent, Update asserts
// it is present. For a cluster Create the absence check spans every environment the
// storagePool is mapped to (a cluster short name is unique per pool across regions);
// for every other kind it is the environment's own rows. Unlike ReplaceDesired this
// does not prune: only the rows the batch names are read or written, so no unrelated
// resource is touched. The per-item upsert reuses ReplaceDesired's spec-defaulting ({})
// and generation rule — a generation bumps only when the merged spec is semantically
// different.
func applyDesiredPatchesTx(tx *sql.Tx, envUUID, storagePool string, patches []models.ResourcePatch) error {
	for _, p := range patches {
		if p.Delete {
			// Removes the resource AND every descendant (endpoints under a branch,
			// branches+endpoints under a cluster), mirroring the amp controller's own
			// delete cascade. starts_with rather than LIKE so a _/% in a name is not a
			// wildcard. Deleting an absent resource is a no-op, not an error.
			const del = `DELETE FROM meko_system.environment_desired_resources
				 WHERE env_uuid=$1 AND (name=$2 OR starts_with(name, $2 || '/'))`
			if _, err := tx.Exec(del, envUUID, p.Name); err != nil {
				return fmt.Errorf("delete desired subtree %s: %w", p.Name, err)
			}
			continue
		}
		merged, existed, err := mergeDesiredSpec(tx, envUUID, p.Kind, p.Name, p.Spec)
		if err != nil {
			return err
		}
		if p.Update && !existed {
			return fmt.Errorf("%w: %s/%s", ErrResourceNotFound, p.Kind, p.Name)
		}
		if p.Create {
			if p.Kind == "cluster" {
				inUse, cerr := clusterNameInPool(tx, storagePool, p.Name)
				if cerr != nil {
					return cerr
				}
				if inUse {
					return fmt.Errorf("%w: %s", ErrClusterNameInUse, p.Name)
				}
			} else if existed {
				return fmt.Errorf("%w: %s/%s", ErrResourceExists, p.Kind, p.Name)
			}
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
			envUUID, p.Kind, p.Name, []byte(merged)); err != nil {
			return fmt.Errorf("upsert desired %s/%s: %w", p.Kind, p.Name, err)
		}
	}
	return nil
}

// mergeDesiredSpec reads the resource's current spec (locked FOR UPDATE) and shallow-
// merges the patch onto it: top-level keys present in the patch replace those in the
// stored spec, unmentioned keys are preserved, and a nested object (e.g. pg_settings)
// is replaced wholesale — matching the backend's "a declared settings surface owns
// the whole map". An absent row (or an empty stored/patch spec) merges onto {}, so a
// first patch becomes the whole spec. The returned existed reports whether the row was
// already present, so a caller can enforce a create/update assertion off the same read.
func mergeDesiredSpec(tx *sql.Tx, envUUID, kind, name string, patch json.RawMessage) (json.RawMessage, bool, error) {
	var existingText string
	err := tx.QueryRow(
		`SELECT COALESCE(spec::text, '{}') FROM meko_system.environment_desired_resources
		 WHERE env_uuid=$1 AND kind=$2 AND name=$3 FOR UPDATE`,
		envUUID, kind, name).Scan(&existingText)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, false, fmt.Errorf("read desired %s/%s: %w", kind, name, err)
	}
	existed := err == nil

	merged := map[string]json.RawMessage{}
	if existed && strings.TrimSpace(existingText) != "" {
		if uerr := json.Unmarshal([]byte(existingText), &merged); uerr != nil {
			// A stored spec that is not a JSON object cannot be shallow-merged onto;
			// treat it as opaque and let the patch replace it wholesale.
			merged = map[string]json.RawMessage{}
		}
	}
	if len(bytes.TrimSpace(patch)) > 0 {
		var incoming map[string]json.RawMessage
		if uerr := json.Unmarshal(patch, &incoming); uerr != nil {
			return nil, false, fmt.Errorf("%w: spec for %s/%s is not a JSON object", ErrSpecNotObject, kind, name)
		}
		for k, v := range incoming {
			merged[k] = v
		}
	}
	out, err := json.Marshal(merged)
	if err != nil {
		return nil, false, fmt.Errorf("marshal merged spec for %s/%s: %w", kind, name, err)
	}
	return out, existed, nil
}

// clusterNameInPool reports whether a cluster of the given full name already exists in
// any non-deregistered environment the storage pool is mapped to. Because a cluster's
// name is <pool>/<cluster> and the pool prefix is fixed within a pool, this is the
// per-pool short-name uniqueness check, spanning regions. Callers must already hold the
// pool-scoped advisory lock so the check-then-write is atomic against a concurrent PATCH
// to another region of the same pool.
func clusterNameInPool(tx *sql.Tx, storagePool, name string) (bool, error) {
	var inUse bool
	err := tx.QueryRow(
		`SELECT EXISTS(
		    SELECT 1
		      FROM meko_system.environment_aeon_account_mapping m
		      JOIN meko_system.environments e ON e.uuid = m.environment_id
		      JOIN meko_system.environment_desired_resources r ON r.env_uuid = m.environment_id
		     WHERE m.storage_pool_name = $1
		       AND e.state <> 'DEREGISTERED'
		       AND r.kind = 'cluster'
		       AND r.name = $2)`, storagePool, name).Scan(&inUse)
	if err != nil {
		return false, fmt.Errorf("check cluster name %s in pool %s: %w", name, storagePool, err)
	}
	return inUse, nil
}

// ErrSpecNotObject is returned when a patch spec is present but is not a JSON object,
// so it cannot be shallow-merged. Mapped to 400 by the service.
var ErrSpecNotObject = errors.New("spec must be a JSON object")

// ErrResourceExists is returned when a create patch names a resource that already
// exists in the environment. Mapped to 409 by the service.
var ErrResourceExists = errors.New("resource already exists")

// ErrResourceNotFound is returned when an update patch names a resource that does not
// exist in the environment. Mapped to 404 by the service.
var ErrResourceNotFound = errors.New("resource does not exist")

// ErrClusterNameInUse is returned when a cluster create names a cluster whose name is
// already in use somewhere in the storage pool — possibly in another region/environment
// the pool is mapped to. A cluster short name is unique per pool across regions. Mapped
// to 409 by the service.
var ErrClusterNameInUse = errors.New("cluster name already in use in this storage pool")

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

// jsonArray coerces an absent, blank or literal-null document to an empty array.
// Both columns are read as arrays everywhere, and json.Marshal of a nil slice
// yields `null` -- four bytes, so a plain length check waves it through and the
// next reader gets a null where it expects a list. This is the last gate before
// the column, so it covers every caller rather than each marshal site.
func jsonArray(raw json.RawMessage) []byte {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return []byte("[]")
	}
	return raw
}
