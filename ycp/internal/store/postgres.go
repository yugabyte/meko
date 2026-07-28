package store

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"meko/ycp/internal/api"
)

// Postgres is the durable Store backed by a vanilla Postgres database.
type Postgres struct {
	pool *pgxpool.Pool
}

const schemaSQL = `
CREATE TABLE IF NOT EXISTS environments (
	id                text PRIMARY KEY,
	account_id        text,
	placement         text,
	region            text,
	state             text,
	k8s_version       text,
	node_count        int,
	provider          text,
	datapacks         int,
	max_datapacks     int,
	agent_version     text,
	last_heartbeat_at timestamptz,
	mcp_base_url      text,
	cert_fingerprint  text,
	components        jsonb,
	live              boolean NOT NULL DEFAULT false,
	created_at        timestamptz NOT NULL DEFAULT now()
);
ALTER TABLE environments ADD COLUMN IF NOT EXISTS enroll_token_hash       text NOT NULL DEFAULT '';
ALTER TABLE environments ADD COLUMN IF NOT EXISTS enroll_token_expires_at timestamptz;
ALTER TABLE environments ADD COLUMN IF NOT EXISTS status_token_hash       text NOT NULL DEFAULT '';
-- Every column below is scanned into a non-nullable Go field (string / int), so a
-- NULL is not "empty", it is a scan error that surfaces as a 500. SeedEnrollment
-- inserts a partial row (it only knows the id, placement and token), which left
-- the rest NULL and made the very first agent enroll against a fresh database
-- fail. Defaults make any partial insert safe; only last_heartbeat_at (*time.Time)
-- and components ([]byte) may legitimately be NULL.
ALTER TABLE environments
	ALTER COLUMN account_id       SET DEFAULT '',
	ALTER COLUMN placement        SET DEFAULT '',
	ALTER COLUMN region           SET DEFAULT '',
	ALTER COLUMN state            SET DEFAULT '',
	ALTER COLUMN k8s_version      SET DEFAULT '',
	ALTER COLUMN node_count       SET DEFAULT 0,
	ALTER COLUMN provider         SET DEFAULT '',
	ALTER COLUMN datapacks        SET DEFAULT 0,
	ALTER COLUMN max_datapacks    SET DEFAULT 0,
	ALTER COLUMN agent_version    SET DEFAULT '',
	ALTER COLUMN mcp_base_url     SET DEFAULT '',
	ALTER COLUMN cert_fingerprint SET DEFAULT '';
-- Backfill rows written before those defaults existed; without this an already
-- deployed control plane keeps failing enroll on its seeded row. Idempotent.
UPDATE environments SET
	account_id       = COALESCE(account_id, ''),
	placement        = COALESCE(placement, ''),
	region           = COALESCE(region, ''),
	state            = COALESCE(state, ''),
	k8s_version      = COALESCE(k8s_version, ''),
	node_count       = COALESCE(node_count, 0),
	provider         = COALESCE(provider, ''),
	datapacks        = COALESCE(datapacks, 0),
	max_datapacks    = COALESCE(max_datapacks, 0),
	agent_version    = COALESCE(agent_version, ''),
	mcp_base_url     = COALESCE(mcp_base_url, ''),
	cert_fingerprint = COALESCE(cert_fingerprint, '')
WHERE account_id IS NULL OR placement IS NULL OR region IS NULL OR state IS NULL
	OR k8s_version IS NULL OR node_count IS NULL OR provider IS NULL OR datapacks IS NULL
	OR max_datapacks IS NULL OR agent_version IS NULL OR mcp_base_url IS NULL
	OR cert_fingerprint IS NULL;
CREATE TABLE IF NOT EXISTS datapacks (
	env_id text NOT NULL,
	id     text NOT NULL,
	name   text NOT NULL,
	phase  text NOT NULL,
	PRIMARY KEY (env_id, id)
);
ALTER TABLE datapacks ADD COLUMN IF NOT EXISTS db       text NOT NULL DEFAULT '';
ALTER TABLE datapacks ADD COLUMN IF NOT EXISTS endpoint text NOT NULL DEFAULT '';
ALTER TABLE datapacks ADD COLUMN IF NOT EXISTS err      text NOT NULL DEFAULT '';
CREATE TABLE IF NOT EXISTS desired_resources (
	env_id text NOT NULL,
	kind   text NOT NULL,
	name   text NOT NULL,
	spec   jsonb,
	PRIMARY KEY (env_id, kind, name)
);`

// NewPostgres connects, applies the schema, and returns the store.
func NewPostgres(ctx context.Context, dsn string) (*Postgres, error) {
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return nil, fmt.Errorf("connect: %w", err)
	}
	cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := pool.Ping(cctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping: %w", err)
	}
	if _, err := pool.Exec(ctx, schemaSQL); err != nil {
		pool.Close()
		return nil, fmt.Errorf("schema: %w", err)
	}
	return &Postgres{pool: pool}, nil
}

const envCols = `id, account_id, placement, region, state, k8s_version, node_count, provider,
	datapacks, max_datapacks, agent_version, last_heartbeat_at, mcp_base_url, cert_fingerprint,
	components, live, created_at, enroll_token_hash, enroll_token_expires_at, status_token_hash`

func scanEnv(row pgx.Row) (*api.Environment, error) {
	var e api.Environment
	var comps []byte
	if err := row.Scan(&e.ID, &e.AccountID, &e.Placement, &e.Region, &e.State, &e.K8sVersion,
		&e.NodeCount, &e.Provider, &e.Datapacks, &e.MaxDatapacks, &e.AgentVersion, &e.LastHeartbeatAt,
		&e.MCPBaseURL, &e.CertFingerprint, &comps, &e.Live, &e.CreatedAt,
		&e.EnrollTokenHash, &e.EnrollExpiresAt, &e.StatusTokenHash); err != nil {
		return nil, err
	}
	if len(comps) > 0 {
		_ = json.Unmarshal(comps, &e.Components)
	}
	return &e, nil
}

func (p *Postgres) save(ctx context.Context, e *api.Environment) error {
	comps, _ := json.Marshal(e.Components)
	_, err := p.pool.Exec(ctx, `
		INSERT INTO environments (`+envCols+`)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20)
		ON CONFLICT (id) DO UPDATE SET
			account_id=$2, placement=$3, region=$4, state=$5, k8s_version=$6, node_count=$7,
			provider=$8, datapacks=$9, max_datapacks=$10, agent_version=$11, last_heartbeat_at=$12,
			mcp_base_url=$13, cert_fingerprint=$14, components=$15, live=$16,
			enroll_token_hash=$18, enroll_token_expires_at=$19, status_token_hash=$20`,
		e.ID, e.AccountID, e.Placement, e.Region, e.State, e.K8sVersion, e.NodeCount, e.Provider,
		e.Datapacks, e.MaxDatapacks, e.AgentVersion, e.LastHeartbeatAt, e.MCPBaseURL, e.CertFingerprint,
		comps, e.Live, e.CreatedAt, e.EnrollTokenHash, e.EnrollExpiresAt, e.StatusTokenHash)
	return err
}

func (p *Postgres) get(ctx context.Context, id string) (*api.Environment, bool, error) {
	e, err := scanEnv(p.pool.QueryRow(ctx, `SELECT `+envCols+` FROM environments WHERE id=$1`, id))
	if err == pgx.ErrNoRows {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	return e, true, nil
}

func (p *Postgres) List() ([]*api.Environment, error) {
	ctx := context.Background()
	rows, err := p.pool.Query(ctx, `SELECT `+envCols+` FROM environments`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*api.Environment
	for rows.Next() {
		e, err := scanEnv(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	sortEnvs(out)
	return out, rows.Err()
}

func (p *Postgres) Get(id string) (*api.Environment, bool, error) {
	return p.get(context.Background(), id)
}

func (p *Postgres) Create(e *api.Environment) error {
	return p.save(context.Background(), e)
}

func (p *Postgres) Action(id, action string) (bool, error) {
	ctx := context.Background()
	e, ok, err := p.get(ctx, id)
	if err != nil || !ok {
		return false, err
	}
	applyAction(e, action)
	return true, p.save(ctx, e)
}

func (p *Postgres) SetState(id string, state api.EnvState) (bool, error) {
	tag, err := p.pool.Exec(context.Background(), `UPDATE environments SET state=$2 WHERE id=$1`, id, state)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() > 0, nil
}

func (p *Postgres) EnrollWithToken(tokenPlain, placement string, f api.ClusterFacts) (*api.Environment, string, bool, error) {
	ctx := context.Background()
	var id string
	err := p.pool.QueryRow(ctx, `SELECT id FROM environments WHERE enroll_token_hash=$1 AND enroll_token_expires_at > now() LIMIT 1`, HashToken(tokenPlain)).Scan(&id)
	if err == pgx.ErrNoRows {
		return nil, "", false, nil
	}
	if err != nil {
		return nil, "", false, err
	}
	e, ok, err := p.get(ctx, id)
	if err != nil || !ok {
		return nil, "", false, err
	}
	applyEnroll(e, placement, f)
	statusPlain, statusHash := NewToken("mko_sts_")
	e.StatusTokenHash = statusHash
	if err := p.save(ctx, e); err != nil {
		return nil, "", false, err
	}
	return e, statusPlain, true, nil
}

func (p *Postgres) ValidateStatusToken(envID, tokenPlain string) (bool, error) {
	var h string
	err := p.pool.QueryRow(context.Background(), `SELECT status_token_hash FROM environments WHERE id=$1`, envID).Scan(&h)
	if err == pgx.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return h != "" && h == HashToken(tokenPlain), nil
}

func (p *Postgres) SeedEnrollment(envID, placement, tokenPlain string) error {
	// Insert if absent; otherwise (re)arm the seeded env's enrollment token so an
	// agent restart can re-enroll (its in-memory status token is lost on restart).
	// This reset applies ONLY to seeded envs — registered envs never call this.
	exp := time.Now().Add(EnrollTokenTTL)
	_, err := p.pool.Exec(context.Background(),
		`INSERT INTO environments (id, placement, state, max_datapacks, created_at, enroll_token_hash, enroll_token_expires_at)
		 VALUES ($1,$2,$3,$4,now(),$5,$6)
		 ON CONFLICT (id) DO UPDATE SET enroll_token_hash = EXCLUDED.enroll_token_hash, enroll_token_expires_at = EXCLUDED.enroll_token_expires_at`,
		envID, placement, api.StatePending, 400, HashToken(tokenPlain), exp)
	return err
}

func (p *Postgres) SetDesiredResources(envID string, res []api.DesiredResource) (bool, error) {
	ctx := context.Background()
	_, ok, err := p.get(ctx, envID)
	if err != nil || !ok {
		return false, err
	}
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `DELETE FROM desired_resources WHERE env_id=$1`, envID); err != nil {
		return false, err
	}
	for _, r := range res {
		spec := r.Spec
		if len(spec) == 0 {
			spec = []byte("{}")
		}
		if _, err := tx.Exec(ctx, `INSERT INTO desired_resources (env_id, kind, name, spec) VALUES ($1,$2,$3,$4) ON CONFLICT DO NOTHING`, envID, r.Kind, r.Name, []byte(spec)); err != nil {
			return false, err
		}
	}
	return true, tx.Commit(ctx)
}

func (p *Postgres) DesiredResources(envID string) ([]api.DesiredResource, error) {
	rows, err := p.pool.Query(context.Background(), `SELECT kind, name, spec FROM desired_resources WHERE env_id=$1 ORDER BY kind, name`, envID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []api.DesiredResource
	for rows.Next() {
		var r api.DesiredResource
		var spec []byte
		if err := rows.Scan(&r.Kind, &r.Name, &spec); err != nil {
			return nil, err
		}
		r.Spec = spec
		out = append(out, r)
	}
	return out, rows.Err()
}

func (p *Postgres) ApplyStatus(id string, r api.StatusReport) (bool, error) {
	ctx := context.Background()
	e, ok, err := p.get(ctx, id)
	if err != nil || !ok {
		return false, err
	}
	applyStatus(e, r)
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer tx.Rollback(ctx)
	comps, _ := json.Marshal(e.Components)
	if _, err := tx.Exec(ctx, `UPDATE environments SET state=$2, agent_version=$3, last_heartbeat_at=$4,
		components=$5, region=$6, k8s_version=$7, node_count=$8, provider=$9, datapacks=$10 WHERE id=$1`,
		e.ID, e.State, e.AgentVersion, e.LastHeartbeatAt, comps, e.Region, e.K8sVersion, e.NodeCount, e.Provider, e.Datapacks); err != nil {
		return false, err
	}
	if _, err := tx.Exec(ctx, `DELETE FROM datapacks WHERE env_id=$1`, id); err != nil {
		return false, err
	}
	for _, d := range r.DatapackPhases {
		if _, err := tx.Exec(ctx, `INSERT INTO datapacks (env_id, id, name, phase, db, endpoint, err) VALUES ($1,$2,$3,$4,$5,$6,$7)`,
			id, d.ID, d.Name, d.Phase, d.DB, d.Endpoint, d.Error); err != nil {
			return false, err
		}
	}
	return true, tx.Commit(ctx)
}

func (p *Postgres) Datapacks(envFilter string) ([]map[string]any, error) {
	ctx := context.Background()
	q := `SELECT env_id, id, name, phase, db, endpoint, err FROM datapacks`
	args := []any{}
	if envFilter != "" {
		q += ` WHERE env_id=$1`
		args = append(args, envFilter)
	}
	q += ` ORDER BY name`
	rows, err := p.pool.Query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		var envID, id, name, phase, db, endpoint, errStr string
		if err := rows.Scan(&envID, &id, &name, &phase, &db, &endpoint, &errStr); err != nil {
			return nil, err
		}
		out = append(out, map[string]any{"environment_id": envID, "id": id, "name": name, "phase": phase, "db": db, "endpoint": endpoint, "error": errStr})
	}
	return out, rows.Err()
}

func (p *Postgres) Revision() (int, error) { return 7, nil }

func (p *Postgres) SeedIfEmpty() error {
	ctx := context.Background()
	var n int
	if err := p.pool.QueryRow(ctx, `SELECT count(*) FROM environments`).Scan(&n); err != nil {
		return err
	}
	if n > 0 {
		return nil
	}
	for _, e := range seedEnvs(time.Now().UTC()) {
		if err := p.save(ctx, e); err != nil {
			return err
		}
	}
	return nil
}

func (p *Postgres) Close() error {
	p.pool.Close()
	return nil
}
