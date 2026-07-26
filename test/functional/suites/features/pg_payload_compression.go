// Pageserver payload-compression regression pack. The pageserver wraps every
// `BYTEA` payload it writes to YugabyteDB in a self-describing envelope (byte 0
// is the codec tag: 0x00 raw, 0x01 LZ4) and compresses payloads of at least
// MIN_COMPRESS_BYTES=256 with LZ4 when that shrinks them. These tests assert the
// three things that can break: SQL sees plaintext regardless of codec, the
// envelope in storage matches what the codec promises, and the win is real.
package features

import (
	"context"
	"crypto/md5"
	"encoding/hex"
	"fmt"
	"math/rand/v2"
	"time"

	"github.com/yugabyte/amp/test/functional/framework/apps/pageserver"
	"github.com/yugabyte/amp/test/functional/framework/apps/pg"
	"github.com/yugabyte/amp/test/functional/framework/core"
)

// storageEnv is the capability the storage-side assertions depend on: reach the
// backing YugabyteDB's amp storage database (where the `pageserver` schema
// lives) and run a command inside the container hosting the pageserver, whose
// management HTTP port is never published. The compute endpoint's own Postgres
// cannot see either.
type storageEnv interface {
	pgEnv
	YBStorage() pg.ConnInfo
	ExecInEndpoint(ctx context.Context, script string) (string, error)
}

const (
	// minCompressBytes mirrors utils::payload_codec::MIN_COMPRESS_BYTES: the
	// pageserver stores anything shorter verbatim whatever the configured codec.
	minCompressBytes = 256
	// blckSz is the Postgres page size, hence the logical length of every
	// `pageserver.pages` row with is_image = true.
	blckSz = 8192
	// rawImageStored is what a raw-stored image occupies: tag byte + page.
	rawImageStored = blckSz + 1

	logicalBytesMetric = "pageserver_yb_payload_logical_bytes_total"
	storedBytesMetric  = "pageserver_yb_payload_stored_bytes_total"
	payloadRowsMetric  = "pageserver_yb_payload_rows_total"

	// pagesRows is enough ~1KB rows to span several dozen heap pages, so the
	// flushed chain covers both the image and the delta path.
	pagesRows    = 400
	pagesRowSize = 1000
)

func init() {
	core.Register(core.Test{
		Name:        "features/pg_payload_compression_roundtrip",
		Suite:       "features",
		Category:    core.Functional,
		Env:         core.EnvSpec{Sharing: core.Shared, Shape: core.SingleContainer},
		Tags:        []string{"regression", "nightly"},
		OwningTeam:  core.TeamCompute,
		Description: "Write compressible and random values whose WAL payloads straddle the pageserver's 256-byte compression threshold, then verify per-row md5 and length on a cold reconnect and again after a TRUNCATE + re-insert cycle.",
		Run:         runPayloadCompressionRoundtrip,
	})

	core.Register(core.Test{
		Name:        "features/pg_payload_compression_envelope",
		Suite:       "features",
		Category:    core.Functional,
		Env:         core.EnvSpec{Sharing: core.Shared, Shape: core.SingleContainer},
		Tags:        []string{"regression", "nightly"},
		OwningTeam:  core.TeamCompute,
		Description: "Inspect this branch's own rows in pageserver.pages and assert the compression envelope: only known codec tags, LZ4 only above the 256-byte threshold, LZ4 only when it shrinks the row, and 8192-byte images either compressed or stored at exactly 8193 bytes.",
		Run:         runPayloadCompressionEnvelope,
	})

	core.Register(core.Test{
		Name:        "features/pg_payload_compression_saves_space",
		Suite:       "features",
		Category:    core.Functional,
		Env:         core.EnvSpec{Sharing: core.Exclusive, Shape: core.SingleContainer},
		Tags:        []string{"regression", "nightly"},
		OwningTeam:  core.TeamCompute,
		Description: "Run a compressible write workload and assert the pageserver's payload byte counters show fewer stored than logical bytes for table=pages, with the LZ4 row-outcome counter advancing.",
		Run:         runPayloadCompressionSavesSpace,
	})

	core.Register(core.Test{
		Name:        "features/pg_payload_compression_survives_restart",
		Suite:       "features",
		Category:    core.Functional,
		Env:         core.EnvSpec{Sharing: core.Exclusive, Shape: core.SingleContainer},
		Tags:        []string{"regression", "nightly"},
		OwningTeam:  core.TeamCompute,
		Description: "Write compressible and random values, restart the compute endpoint so every read comes back through the pageserver, and verify per-row md5 and length survived the decode path.",
		Run:         runPayloadCompressionSurvivesRestart,
	})
}

// payloadValue is one test row: a value plus its Go-computed digest, so the
// verification never trusts the database for its own expectation.
type payloadValue struct {
	id     int
	kind   string
	bytes  []byte
	digest string
}

// payloadLadder builds rows whose values walk the codec's decision boundary from
// well under minCompressBytes to well over it, in both a highly compressible and
// an incompressible flavour. The WAL record a single-row INSERT emits is
// dominated by the tuple, so the ladder walks the raw/compress decision the
// pageserver makes on the delta path.
func payloadLadder(t *core.T) []payloadValue {
	r := t.Seed.Fork("payload_ladder").Rand()
	sizes := []int{8, 64, 200, minCompressBytes - 1, minCompressBytes, minCompressBytes + 1, 512, 2000, blckSz, 3 * blckSz}
	out := make([]payloadValue, 0, 2*len(sizes))
	for _, size := range sizes {
		out = append(out,
			newPayloadValue(len(out), "compressible", compressibleBytes(size)),
			newPayloadValue(len(out)+1, "random", randomBytes(r, size)),
		)
	}
	return out
}

func newPayloadValue(id int, kind string, b []byte) payloadValue {
	sum := md5.Sum(b)
	return payloadValue{id: id, kind: kind, bytes: b, digest: hex.EncodeToString(sum[:])}
}

// compressibleBytes returns n bytes of a repeating multi-byte pattern — LZ4
// crushes it, but unlike a single repeated byte it still looks like real tuple
// data to the page layout.
func compressibleBytes(n int) []byte {
	pattern := []byte("amp_payload_compression_")
	b := make([]byte, n)
	for i := range b {
		b[i] = pattern[i%len(pattern)]
	}
	return b
}

func randomBytes(r *rand.Rand, n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(r.Uint64() >> 24)
	}
	return b
}

func runPayloadCompressionRoundtrip(t *core.T) error {
	env, ok := t.Env.(pgEnv)
	if !ok {
		t.Skip("requires an env that serves an endpoint's Postgres")
		return nil
	}
	client, err := connect(t, env)
	if err != nil {
		return err
	}
	defer client.Close()

	table := "pcomp_" + suffix(t)
	values := payloadLadder(t)

	if err := t.Step("write", func(s *core.StepHandle) error {
		if _, err := client.Exec(t, fmt.Sprintf("CREATE TABLE %s (id int PRIMARY KEY, kind text NOT NULL, v bytea NOT NULL)", table)); err != nil {
			return err
		}
		if err := insertPayloads(t, client, table, values); err != nil {
			return err
		}
		s.Set("rows", len(values))
		_, err := client.Exec(t, "CHECKPOINT")
		return err
	}); err != nil {
		_, _ = client.Exec(t, "DROP TABLE IF EXISTS "+table)
		return err
	}
	client.Close()

	if err := t.Step("verify_cold", func(s *core.StepHandle) error {
		cold, err := connect(t, env)
		if err != nil {
			return err
		}
		defer cold.Close()
		return verifyPayloads(t, cold, table, values, "cold reconnect")
	}); err != nil {
		return err
	}

	return t.Step("verify_after_truncate_cycle", func(s *core.StepHandle) error {
		client, err := connect(t, env)
		if err != nil {
			return err
		}
		defer client.Close()
		defer func() { _, _ = client.Exec(t, "DROP TABLE IF EXISTS "+table) }()

		if _, err := client.Exec(t, "TRUNCATE TABLE "+table); err != nil {
			return err
		}
		left, err := client.QueryInt(t, "SELECT count(*) FROM "+table)
		if err != nil {
			return err
		}
		if left != 0 {
			return fmt.Errorf("after TRUNCATE, table %s has %d rows, want 0", table, left)
		}
		if err := insertPayloads(t, client, table, values); err != nil {
			return err
		}
		if _, err := client.Exec(t, "CHECKPOINT"); err != nil {
			return err
		}
		return verifyPayloads(t, client, table, values, "TRUNCATE + re-insert cycle")
	})
}

func runPayloadCompressionSurvivesRestart(t *core.T) error {
	env, ok := t.Env.(computeRestarter)
	if !ok {
		t.Skip("requires an env with compute-restart capability (docker or sharedk8s)")
		return nil
	}
	client, err := connect(t, env)
	if err != nil {
		return err
	}

	table := "pcompr_" + suffix(t)
	values := payloadLadder(t)

	if err := t.Step("write", func(s *core.StepHandle) error {
		if _, err := client.Exec(t, fmt.Sprintf("CREATE TABLE %s (id int PRIMARY KEY, kind text NOT NULL, v bytea NOT NULL)", table)); err != nil {
			return err
		}
		if err := insertPayloads(t, client, table, values); err != nil {
			return err
		}
		s.Set("rows", len(values))
		_, err := client.Exec(t, "CHECKPOINT")
		return err
	}); err != nil {
		client.Close()
		return err
	}
	client.Close()

	if err := t.Step("restart_endpoint", func(s *core.StepHandle) error {
		return env.RestartCompute(t.Context)
	}); err != nil {
		return err
	}

	// The restart empties shared_buffers, so every value below is reassembled
	// from the pageserver's stored chain — the codec's decode path.
	return t.Step("verify_after_restart", func(s *core.StepHandle) error {
		client, err := connect(t, env)
		if err != nil {
			return err
		}
		defer client.Close()
		defer func() { _, _ = client.Exec(t, "DROP TABLE IF EXISTS "+table) }()
		return verifyPayloads(t, client, table, values, "compute-endpoint restart")
	})
}

func insertPayloads(t *core.T, client *pg.Client, table string, values []payloadValue) error {
	for _, v := range values {
		if _, err := client.Exec(t, fmt.Sprintf("INSERT INTO %s (id, kind, v) VALUES ($1, $2, $3)", table), v.id, v.kind, v.bytes); err != nil {
			return err
		}
	}
	return nil
}

// verifyPayloads asserts every value's md5 and length match what Go wrote. what
// names the boundary just crossed, so a failure says which read path lost bytes.
func verifyPayloads(t *core.T, client *pg.Client, table string, values []payloadValue, what string) error {
	count, err := client.QueryInt(t, "SELECT count(*) FROM "+table)
	if err != nil {
		return err
	}
	if int(count) != len(values) {
		return fmt.Errorf("after %s, table %s has %d rows, want %d", what, table, count, len(values))
	}
	for _, v := range values {
		got, err := client.QueryString(t, fmt.Sprintf("SELECT md5(v) FROM %s WHERE id = $1", table), v.id)
		if err != nil {
			return err
		}
		if got != v.digest {
			return fmt.Errorf("after %s, row %d (%s, %d bytes) md5(v) = %s, want %s (the payload did not survive the pageserver's compression envelope intact)",
				what, v.id, v.kind, len(v.bytes), got, v.digest)
		}
		gotLen, err := client.QueryInt(t, fmt.Sprintf("SELECT octet_length(v) FROM %s WHERE id = $1", table), v.id)
		if err != nil {
			return err
		}
		if int(gotLen) != len(v.bytes) {
			return fmt.Errorf("after %s, row %d (%s) octet_length(v) = %d, want %d (compression must be invisible to SQL)",
				what, v.id, v.kind, gotLen, len(v.bytes))
		}
	}
	return nil
}

// envelopeStats is the per-relation shape of the compression envelope as stored,
// read straight out of pageserver.pages.
type envelopeStats struct {
	rows            int64
	unknownTag      int64
	lz4Rows         int64
	rawRows         int64
	imageRows       int64
	minLz4Logical   int64 // -1 when the relation has no LZ4 row
	lz4NotSmaller   int64
	lz4ImageBadSize int64
	rawImageBadSize int64
}

// readEnvelopeStats aggregates one relation's rows. `declared` is the LZ4
// envelope's own 4-byte LE logical length (bytes 1..5), which is what lets a
// SQL-side check reason about the *pre-compression* size; it is only computed
// for LZ4 rows, since get_byte past the end of a short raw payload errors.
func readEnvelopeStats(t *core.T, storage *pg.Client, branchID string, dbnode, relnode int64) (envelopeStats, error) {
	const q = `
WITH p AS (
    SELECT is_image,
           get_byte(payload, 0) AS tag,
           octet_length(payload) AS stored,
           CASE WHEN get_byte(payload, 0) = 1 AND octet_length(payload) >= 5
                THEN get_byte(payload, 1) + get_byte(payload, 2) * 256
                     + get_byte(payload, 3) * 65536 + get_byte(payload, 4) * 16777216
           END AS declared
      FROM pageserver.pages
     WHERE branch_id = $1 AND dbnode = $2 AND relnode = $3 AND forknum = 0
)
SELECT count(*),
       count(*) FILTER (WHERE tag NOT IN (0, 1)),
       count(*) FILTER (WHERE tag = 1),
       count(*) FILTER (WHERE tag = 0),
       count(*) FILTER (WHERE is_image),
       coalesce(min(declared), -1),
       count(*) FILTER (WHERE tag = 1 AND stored >= declared + 1),
       count(*) FILTER (WHERE tag = 1 AND is_image AND declared <> $4),
       count(*) FILTER (WHERE tag = 0 AND is_image AND stored <> $5)
  FROM p`
	var st envelopeStats
	err := storage.Pool().QueryRow(t, q, branchID, dbnode, relnode, blckSz, rawImageStored).Scan(
		&st.rows, &st.unknownTag, &st.lz4Rows, &st.rawRows, &st.imageRows,
		&st.minLz4Logical, &st.lz4NotSmaller, &st.lz4ImageBadSize, &st.rawImageBadSize)
	if err != nil {
		return envelopeStats{}, fmt.Errorf("read envelope stats for relnode %d: %w", relnode, err)
	}
	return st, nil
}

// checkEnvelope asserts the invariants utils::payload_codec guarantees for every
// stored row, whatever the payload happened to be.
func checkEnvelope(st envelopeStats, rel string) error {
	if st.unknownTag != 0 {
		return fmt.Errorf("%s: %d of %d rows in pageserver.pages carry a codec tag other than 0 (raw) or 1 (lz4) — this build cannot decode them",
			rel, st.unknownTag, st.rows)
	}
	if st.lz4Rows > 0 && st.minLz4Logical < minCompressBytes {
		return fmt.Errorf("%s: smallest LZ4-stored payload declares %d logical bytes, want >= %d (MIN_COMPRESS_BYTES) — a sub-threshold payload was compressed",
			rel, st.minLz4Logical, minCompressBytes)
	}
	if st.lz4NotSmaller != 0 {
		return fmt.Errorf("%s: %d LZ4-stored rows occupy at least as many bytes as storing them raw would — the codec kept a compression that did not pay",
			rel, st.lz4NotSmaller)
	}
	if st.lz4ImageBadSize != 0 {
		return fmt.Errorf("%s: %d LZ4-stored image rows declare a logical length other than %d — an image row must be exactly one Postgres page",
			rel, st.lz4ImageBadSize, blckSz)
	}
	if st.rawImageBadSize != 0 {
		return fmt.Errorf("%s: %d raw-stored image rows are not exactly %d bytes (tag + page) — the raw fallback must never grow a payload",
			rel, st.rawImageBadSize, rawImageStored)
	}
	return nil
}

func runPayloadCompressionEnvelope(t *core.T) error {
	env, ok := t.Env.(storageEnv)
	if !ok {
		t.Skip("requires an env exposing the backing amp storage database and the pageserver's container")
		return nil
	}
	client, err := connect(t, env)
	if err != nil {
		return err
	}
	defer client.Close()

	compressible := "pcompc_" + suffix(t)
	random := "pcompn_" + suffix(t)
	r := t.Seed.Fork("envelope").Rand()

	if err := t.Step("write_and_flush", func(s *core.StepHandle) error {
		for _, table := range []string{compressible, random} {
			if _, err := client.Exec(t, fmt.Sprintf("CREATE TABLE %s (id int PRIMARY KEY, v bytea NOT NULL, touched int NOT NULL DEFAULT 0)", table)); err != nil {
				return err
			}
		}
		for id := 0; id < pagesRows; id++ {
			if _, err := client.Exec(t, fmt.Sprintf("INSERT INTO %s (id, v) VALUES ($1, $2)", compressible), id, compressibleBytes(pagesRowSize)); err != nil {
				return err
			}
			if _, err := client.Exec(t, fmt.Sprintf("INSERT INTO %s (id, v) VALUES ($1, $2)", random), id, randomBytes(r, pagesRowSize)); err != nil {
				return err
			}
		}
		// CHECKPOINT then touch every row: the first write to a page after a
		// checkpoint emits a full-page image, which is the row shape the whole
		// compression win rests on.
		if _, err := client.Exec(t, "CHECKPOINT"); err != nil {
			return err
		}
		for _, table := range []string{compressible, random} {
			if _, err := client.Exec(t, fmt.Sprintf("UPDATE %s SET touched = touched + 1", table)); err != nil {
				return err
			}
		}
		s.Set("rows_per_relation", pagesRows)
		return flushToStorage(t, client, env)
	}); err != nil {
		dropTables(t, client, compressible, random)
		return err
	}

	return t.Step("inspect_stored_envelope", func(s *core.StepHandle) error {
		defer dropTables(t, client, compressible, random)

		branchID, dbnode, err := branchAndDatabase(t, client)
		if err != nil {
			return err
		}
		storage, err := connectStorage(t, env)
		if err != nil {
			return err
		}
		defer storage.Close()

		for _, rel := range []struct {
			label       string
			table       string
			wantLZ4Rows bool
		}{
			// Only the compressible relation is guaranteed to produce compressed
			// rows: its ~1KB repeating values put both the images and the deltas
			// above the threshold and well within LZ4's reach. The random one is
			// here to prove the raw fallback never grows a row.
			{"compressible relation", compressible, true},
			{"random relation", random, false},
		} {
			relnode, err := client.QueryInt(t, "SELECT pg_relation_filenode($1)::bigint", rel.table)
			if err != nil {
				return err
			}
			st, err := readEnvelopeStats(t, storage, branchID, dbnode, relnode)
			if err != nil {
				return err
			}
			s.Set(rel.table+"_rows", st.rows)
			s.Set(rel.table+"_lz4_rows", st.lz4Rows)
			s.Set(rel.table+"_raw_rows", st.rawRows)
			s.Set(rel.table+"_image_rows", st.imageRows)
			if st.rows == 0 {
				return fmt.Errorf("%s: pageserver.pages holds no rows for relnode %d on branch %s after a forced flush — the write never reached storage",
					rel.label, relnode, branchID)
			}
			if err := checkEnvelope(st, rel.label); err != nil {
				return err
			}
			if rel.wantLZ4Rows && st.lz4Rows == 0 {
				return fmt.Errorf("%s: all %d rows in pageserver.pages are stored raw, want at least one LZ4 row — compression never engaged", rel.label, st.rows)
			}
		}
		return nil
	})
}

func runPayloadCompressionSavesSpace(t *core.T) error {
	env, ok := t.Env.(storageEnv)
	if !ok {
		t.Skip("requires an env exposing the backing amp storage database and the pageserver's container")
		return nil
	}
	client, err := connect(t, env)
	if err != nil {
		return err
	}
	defer client.Close()

	ps := pageserver.New(env.ExecInEndpoint, pageserver.DefaultHTTPPort)
	pagesLabel := map[string]string{"table": "pages"}
	table := "pcomps_" + suffix(t)

	var beforeLogical, beforeStored, beforeLz4 float64
	if err := t.Step("baseline_metrics", func(s *core.StepHandle) error {
		before, err := ps.Metrics(t)
		if err != nil {
			return err
		}
		// Absent counters are a failure, not a skip: this test exists to prove
		// the pair is exported and moving.
		var ok bool
		if beforeLogical, ok = before.Value(logicalBytesMetric, pagesLabel); !ok {
			return fmt.Errorf("pageserver /metrics exports no %s{table=\"pages\"} — the payload-compression accounting is missing from this build", logicalBytesMetric)
		}
		if beforeStored, ok = before.Value(storedBytesMetric, pagesLabel); !ok {
			return fmt.Errorf("pageserver /metrics exports no %s{table=\"pages\"} — the payload-compression accounting is missing from this build", storedBytesMetric)
		}
		beforeLz4, _ = before.Value(payloadRowsMetric, map[string]string{"table": "pages", "outcome": "lz4"})
		return nil
	}); err != nil {
		return err
	}

	if err := t.Step("compressible_workload", func(s *core.StepHandle) error {
		if _, err := client.Exec(t, fmt.Sprintf("CREATE TABLE %s (id int PRIMARY KEY, v bytea NOT NULL, touched int NOT NULL DEFAULT 0)", table)); err != nil {
			return err
		}
		for id := 0; id < pagesRows; id++ {
			if _, err := client.Exec(t, fmt.Sprintf("INSERT INTO %s (id, v) VALUES ($1, $2)", table), id, compressibleBytes(pagesRowSize)); err != nil {
				return err
			}
		}
		if _, err := client.Exec(t, "CHECKPOINT"); err != nil {
			return err
		}
		if _, err := client.Exec(t, fmt.Sprintf("UPDATE %s SET touched = touched + 1", table)); err != nil {
			return err
		}
		s.Set("rows", pagesRows)
		return flushToStorage(t, client, env)
	}); err != nil {
		dropTables(t, client, table)
		return err
	}

	return t.Step("verify_saving", func(s *core.StepHandle) error {
		defer dropTables(t, client, table)

		after, err := ps.Metrics(t)
		if err != nil {
			return err
		}
		afterLogical, _ := after.Value(logicalBytesMetric, pagesLabel)
		afterStored, _ := after.Value(storedBytesMetric, pagesLabel)
		afterLz4, _ := after.Value(payloadRowsMetric, map[string]string{"table": "pages", "outcome": "lz4"})

		logical := afterLogical - beforeLogical
		stored := afterStored - beforeStored
		s.Set("logical_bytes", logical)
		s.Set("stored_bytes", stored)
		s.Set("lz4_rows", afterLz4-beforeLz4)

		if logical <= 0 {
			return fmt.Errorf("%s{table=\"pages\"} advanced by %.0f bytes over the workload, want > 0 — the pageserver recorded no payload write at all", logicalBytesMetric, logical)
		}
		if stored <= 0 {
			return fmt.Errorf("%s{table=\"pages\"} advanced by %.0f bytes over the workload, want > 0 — every payload write must store at least its envelope", storedBytesMetric, stored)
		}
		if afterLz4 <= beforeLz4 {
			return fmt.Errorf("%s{table=\"pages\",outcome=\"lz4\"} did not advance (%.0f -> %.0f) over a workload of %d compressible ~%d-byte rows — compression never engaged",
				payloadRowsMetric, beforeLz4, afterLz4, pagesRows, pagesRowSize)
		}
		// The floor is deliberately far below what this workload should reach:
		// the least favourable mix measured on the product PR (delta-heavy, only
		// 0.63% of rows compressed) still stored 14% fewer bytes than logical,
		// and this workload is image-heavy on a repeating pattern. Asking for 5%
		// keeps the test a check that compression is on, not a ratio gate.
		const maxStoredFraction = 0.95
		if stored > logical*maxStoredFraction {
			return fmt.Errorf("over the workload the pageserver stored %.0f bytes for %.0f logical bytes (%.2fx), want stored <= %.0f (a %.0f%% saving) — payload compression is not saving space",
				stored, logical, logical/stored, logical*maxStoredFraction, (1-maxStoredFraction)*100)
		}
		t.Metric("pageserver_payload_compression_ratio", logical/stored, "x")
		return nil
	})
}

// flushToStorage pushes the compute's writes all the way into YugabyteDB: a
// CHECKPOINT gets the WAL to the safekeeper, then a forced pageserver flush at
// that LSN lands the branch's buffered pages, so a storage-side query does not
// have to wait out the periodic flush interval.
func flushToStorage(t *core.T, client *pg.Client, env storageEnv) error {
	if _, err := client.Exec(t, "CHECKPOINT"); err != nil {
		return err
	}
	branchID, err := client.QueryString(t, "SELECT current_setting('yb_amp.branch_id')")
	if err != nil {
		return fmt.Errorf("read the endpoint's branch id: %w", err)
	}
	lsn, err := client.QueryString(t, "SELECT pg_current_wal_flush_lsn()::text")
	if err != nil {
		return err
	}
	ps := pageserver.New(env.ExecInEndpoint, pageserver.DefaultHTTPPort)
	return ps.FlushBranch(t, branchID, lsn)
}

func branchAndDatabase(t *core.T, client *pg.Client) (string, int64, error) {
	branchID, err := client.QueryString(t, "SELECT current_setting('yb_amp.branch_id')")
	if err != nil {
		return "", 0, fmt.Errorf("read the endpoint's branch id: %w", err)
	}
	dbnode, err := client.QueryInt(t, "SELECT oid::bigint FROM pg_database WHERE datname = current_database()")
	if err != nil {
		return "", 0, err
	}
	return branchID, dbnode, nil
}

// storageReadyTimeout bounds waiting for the backing store's YSQL. It is the
// same cluster the endpoint is already serving from, so it answers immediately;
// the budget only covers a cold connection.
const storageReadyTimeout = time.Minute

func connectStorage(t *core.T, env storageEnv) (*pg.Client, error) {
	ci := env.YBStorage()
	if err := pg.WaitReady(t, ci, storageReadyTimeout); err != nil {
		return nil, fmt.Errorf("backing amp storage database %s never answered: %w", ci.Addr(), err)
	}
	return pg.Connect(t, ci)
}

func dropTables(t *core.T, client *pg.Client, tables ...string) {
	for _, table := range tables {
		_, _ = client.Exec(t, "DROP TABLE IF EXISTS "+table)
	}
}
