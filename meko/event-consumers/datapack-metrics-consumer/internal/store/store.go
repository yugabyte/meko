// Package store holds every SQL statement the consumer runs.
//
// Table names are built from a schema (validated by config) so the tests can point the consumer at a
// throwaway one. Parameters carry explicit casts because lib/pq sends them untyped.
package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/lib/pq"
)

// Querier is satisfied by *sql.DB and *sql.Tx, so a statement runs in a transaction or on its own.
type Querier interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// Event is one row of events.
type Event struct {
	CreatedAt time.Time
	ID        string
	Type      string
	// SchemaVersion is the version of the payload's JSON shape for this event type (event_schema_version).
	// Handlers read unknown fields and newer versions the same way unless they branch on it.
	SchemaVersion int
	Payload       json.RawMessage
}

// Position is a point in a shard's log: events are ordered by (created_at, id).
type Position struct {
	TS time.Time
	ID string
}

// Pos is the position of e.
func (e Event) Pos() Position { return Position{TS: e.CreatedAt, ID: e.ID} }

// Tables are the quoted, schema-qualified table names.
type Tables struct {
	Events, Default, Consumers, DeadLetters, Applied string
	eventsRegclass                                   string
}

// NewTables builds the names for schema.
func NewTables(schema string) Tables {
	name := func(table string) string { return pq.QuoteIdentifier(schema) + "." + pq.QuoteIdentifier(table) }
	return Tables{
		Events:         name("events"),
		Default:        name("events_default"),
		Consumers:      name("event_consumers"),
		DeadLetters:    name("event_dead_letters"),
		Applied:        name("event_applied"),
		eventsRegclass: name("events"),
	}
}

// Lease is who holds a shard and whether that lease is free or expired, by the database's clock.
type Lease struct {
	Owner sql.NullString
	Free  bool
}

// LeaseSnapshot reads the lease on each listed shard.
func LeaseSnapshot(ctx context.Context, q Querier, t Tables, consumer string, shards []int) (map[int]Lease, error) {
	rows, err := q.QueryContext(ctx, fmt.Sprintf(
		`SELECT shard, lease_owner, lease_owner IS NULL OR lease_expires_at < now() `+
			`FROM %s WHERE consumer = $1 AND shard = ANY($2::int[])`, t.Consumers),
		consumer, pq.Array(shards))
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := map[int]Lease{}
	for rows.Next() {
		var shard int
		var l Lease
		if err := rows.Scan(&shard, &l.Owner, &l.Free); err != nil {
			return nil, err
		}
		out[shard] = l
	}
	return out, rows.Err()
}

// Acquire takes or renews the lease on every listed shard that is free, expired or already mine.
func Acquire(
	ctx context.Context, q Querier, t Tables, consumer string, shards []int, me string, lease time.Duration,
) ([]int, error) {
	rows, err := q.QueryContext(ctx, fmt.Sprintf(
		`UPDATE %s SET lease_owner = $1::uuid, lease_expires_at = now() + make_interval(secs => $2::float8), `+
			`updated_at = now() WHERE consumer = $3 AND shard = ANY($4::int[]) `+
			`AND (lease_owner IS NULL OR lease_expires_at < now() OR lease_owner = $1::uuid) `+
			`RETURNING shard`, t.Consumers),
		me, lease.Seconds(), consumer, pq.Array(shards))
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var got []int
	for rows.Next() {
		var s int
		if err := rows.Scan(&s); err != nil {
			return nil, err
		}
		got = append(got, s)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	slices.Sort(got)
	return got, nil
}

// Release gives up every lease held by me.
func Release(ctx context.Context, q Querier, t Tables, consumer, me string) error {
	_, err := q.ExecContext(ctx, fmt.Sprintf(
		`UPDATE %s SET lease_owner = NULL, lease_expires_at = NULL `+
			`WHERE consumer = $1 AND lease_owner = $2::uuid`, t.Consumers),
		consumer, me)
	return err
}

// ReadOffset returns the saved position of a shard; ok is false when the shard has no row.
func ReadOffset(
	ctx context.Context, q Querier, t Tables, consumer string, shard int,
) (pos Position, ok bool, err error) {
	err = q.QueryRowContext(ctx, fmt.Sprintf(
		`SELECT offset_ts, offset_id FROM %s WHERE consumer = $1 AND shard = $2`, t.Consumers), consumer, shard).
		Scan(&pos.TS, &pos.ID)
	if errors.Is(err, sql.ErrNoRows) {
		return Position{}, false, nil
	}
	return pos, err == nil, err
}

// ReadBatch returns up to limit events after the offset, oldest first, leaving out the newest safetyLag.
//
// VERIFY on YugabyteDB: the row comparison is used as an index range bound on the primary key. The
// equivalent `created_at > $1 OR (created_at = $1 AND id > $2)` is the fallback.
func ReadBatch(
	ctx context.Context, q Querier, t Tables, shard int, off Position, safetyLag time.Duration, limit int,
) ([]Event, error) {
	rows, err := q.QueryContext(ctx, fmt.Sprintf(
		`SELECT created_at, id, type, event_schema_version, payload FROM %s `+
			`WHERE shard = $1 AND (created_at, id) > ($2::timestamptz, $3::uuid) `+
			`AND created_at < now() - make_interval(secs => $4::float8) `+
			`ORDER BY created_at, id LIMIT $5`, t.Events),
		shard, off.TS, off.ID, safetyLag.Seconds(), limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []Event
	for rows.Next() {
		var e Event
		var payload []byte
		if err := rows.Scan(&e.CreatedAt, &e.ID, &e.Type, &e.SchemaVersion, &payload); err != nil {
			return nil, err
		}
		e.Payload = payload
		out = append(out, e)
	}
	return out, rows.Err()
}

// AdvanceOffset is a compare-and-swap. False means another instance moved the offset first, and the
// caller must roll back.
func AdvanceOffset(
	ctx context.Context, q Querier, t Tables, consumer string, shard int, oldPos, newPos Position,
) (bool, error) {
	res, err := q.ExecContext(ctx, fmt.Sprintf(
		`UPDATE %s SET offset_ts = $1::timestamptz, offset_id = $2::uuid, updated_at = now() `+
			`WHERE consumer = $3 AND shard = $4 AND offset_ts = $5::timestamptz AND offset_id = $6::uuid`, t.Consumers),
		newPos.TS, newPos.ID, consumer, shard, oldPos.TS, oldPos.ID)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

// DeadLetter records an event the consumer could not apply. It keeps the event's own created_at and
// event_schema_version, so a replay counts toward the day the event happened and is read with the payload
// version it was written with; the row's created_at is when it was dead-lettered.
func DeadLetter(ctx context.Context, q Querier, t Tables, consumer string, shard int, e Event, errText string) error {
	_, err := q.ExecContext(ctx, fmt.Sprintf(
		`INSERT INTO %s (consumer, shard, event_id, event_created_at, type, event_schema_version, payload, error) `+
			`VALUES ($1, $2, $3::uuid, $4::timestamptz, $5, $6, $7::jsonb, $8)`,
		t.DeadLetters), consumer, shard, e.ID, e.CreatedAt, e.Type, e.SchemaVersion, string(e.Payload), errText)
	return err
}

// ---- the applied-event record, and the look-back that uses it
//
// created_at is stamped at INSERT but a row is visible only at COMMIT, so a row that commits late can
// appear below an offset that has already moved past it. Every event the consumer handles is recorded
// in event_applied in the same transaction as its effects. The look-back reads the window below the
// offset and handles whatever is missing from that record.

func appliedArrays(events []Event) (at, ids []string) {
	for _, e := range events {
		at, ids = append(at, e.CreatedAt.Format(time.RFC3339Nano)), append(ids, e.ID)
	}
	return at, ids
}

// MarkApplied records events as handled. Call it in the transaction that applies them.
func MarkApplied(ctx context.Context, q Querier, t Tables, consumer string, shard int, events []Event) error {
	if len(events) == 0 {
		return nil
	}
	at, ids := appliedArrays(events)
	_, err := q.ExecContext(ctx, fmt.Sprintf(
		`INSERT INTO %s (consumer, shard, created_at, event_id) `+
			`SELECT $1, $2, x.c, x.i FROM unnest($3::timestamptz[], $4::uuid[]) AS x(c, i) `+
			`ON CONFLICT DO NOTHING`, t.Applied),
		consumer, shard, pq.Array(at), pq.Array(ids))
	return err
}

// ClaimApplied records events as handled and returns the ids that were not recorded before. Two
// instances that find the same late event cannot both apply it: the second one's insert waits for the
// first and then conflicts, so it claims nothing.
func ClaimApplied(
	ctx context.Context, q Querier, t Tables, consumer string, shard int, events []Event,
) (map[string]bool, error) {
	if len(events) == 0 {
		return nil, nil
	}
	at, ids := appliedArrays(events)
	rows, err := q.QueryContext(ctx, fmt.Sprintf(
		`INSERT INTO %s (consumer, shard, created_at, event_id) `+
			`SELECT $1, $2, x.c, x.i FROM unnest($3::timestamptz[], $4::uuid[]) AS x(c, i) `+
			`ON CONFLICT DO NOTHING RETURNING event_id::text`, t.Applied),
		consumer, shard, pq.Array(at), pq.Array(ids))
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	claimed := map[string]bool{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		claimed[id] = true
	}
	return claimed, rows.Err()
}

// EventKeysBelow returns the positions of the newest events at or below off that are not older than
// from, newest first, up to limit. Keys only: the rows themselves are fetched for the few that are missing.
func EventKeysBelow(
	ctx context.Context, q Querier, t Tables, shard int, from time.Time, off Position, limit int,
) ([]Position, error) {
	rows, err := q.QueryContext(ctx, fmt.Sprintf(
		`SELECT created_at, id::text FROM %s WHERE shard = $1 AND created_at >= $2::timestamptz `+
			`AND (created_at, id) <= ($3::timestamptz, $4::uuid) ORDER BY created_at DESC, id DESC LIMIT $5`, t.Events),
		shard, from, off.TS, off.ID, limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []Position
	for rows.Next() {
		var p Position
		if err := rows.Scan(&p.TS, &p.ID); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// AppliedIDs returns the ids recorded as handled for events created in [from, to].
func AppliedIDs(
	ctx context.Context, q Querier, t Tables, consumer string, shard int, from, to time.Time,
) (map[string]bool, error) {
	rows, err := q.QueryContext(ctx, fmt.Sprintf(
		`SELECT event_id::text FROM %s WHERE consumer = $1 AND shard = $2 `+
			`AND created_at >= $3::timestamptz AND created_at <= $4::timestamptz`, t.Applied),
		consumer, shard, from, to)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := map[string]bool{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out[id] = true
	}
	return out, rows.Err()
}

// EventsByKey fetches whole events by position, oldest first. keys must not be empty.
func EventsByKey(ctx context.Context, q Querier, t Tables, shard int, keys []Position) ([]Event, error) {
	lo, hi := keys[0].TS, keys[0].TS
	ids := make([]string, 0, len(keys))
	for _, k := range keys {
		lo, hi = minTime(lo, k.TS), maxTime(hi, k.TS)
		ids = append(ids, k.ID)
	}
	rows, err := q.QueryContext(ctx, fmt.Sprintf(
		`SELECT created_at, id::text, type, event_schema_version, payload FROM %s WHERE shard = $1 `+
			`AND created_at >= $2::timestamptz AND created_at <= $3::timestamptz AND id = ANY($4::uuid[]) `+
			`ORDER BY created_at, id`, t.Events),
		shard, lo, hi, pq.Array(ids))
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []Event
	for rows.Next() {
		var e Event
		var payload []byte
		if err := rows.Scan(&e.CreatedAt, &e.ID, &e.Type, &e.SchemaVersion, &payload); err != nil {
			return nil, err
		}
		e.Payload = payload
		out = append(out, e)
	}
	return out, rows.Err()
}

func minTime(a, b time.Time) time.Time {
	if b.Before(a) {
		return b
	}
	return a
}

func maxTime(a, b time.Time) time.Time {
	if b.After(a) {
		return b
	}
	return a
}

// TrimApplied deletes a shard's applied records older than before and returns how many. Pass the
// shard's own look-back lower bound (its offset minus the window), never a wall-clock cutoff: a shard
// that is behind still needs the records near its offset, and deleting them would make the look-back
// apply those events a second time.
func TrimApplied(
	ctx context.Context, q Querier, t Tables, consumer string, shard int, before time.Time,
) (int64, error) {
	res, err := q.ExecContext(ctx, fmt.Sprintf(
		`DELETE FROM %s WHERE consumer = $1 AND shard = $2 AND created_at < $3::timestamptz`, t.Applied),
		consumer, shard, before)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// ServerTimeZone is the database's session time zone. The consumer buckets by UTC date; the MCP server's
// inline writes and the readers of datapack_daily_activity use CURRENT_DATE, which follows this setting.
func ServerTimeZone(ctx context.Context, q Querier) (string, error) {
	var tz string
	err := q.QueryRowContext(ctx, `SHOW TimeZone`).Scan(&tz)
	return tz, err
}

// ---- read-only measurements for the janitor

// OldestUnreadAge is the age in seconds of the first event after off, or 0 if there is none.
func OldestUnreadAge(ctx context.Context, q Querier, t Tables, shard int, off Position) (float64, error) {
	var age float64
	err := q.QueryRowContext(ctx, fmt.Sprintf(
		`SELECT extract(epoch FROM now() - created_at)::float8 FROM %s `+
			`WHERE shard = $1 AND (created_at, id) > ($2::timestamptz, $3::uuid) `+
			`ORDER BY created_at, id LIMIT 1`, t.Events),
		shard, off.TS, off.ID).Scan(&age)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	return max(age, 0), err
}

// AllOffsets returns every shard's saved position for a consumer group.
func AllOffsets(ctx context.Context, q Querier, t Tables, consumer string) (map[int]Position, error) {
	rows, err := q.QueryContext(ctx, fmt.Sprintf(
		`SELECT shard, offset_ts, offset_id FROM %s WHERE consumer = $1`, t.Consumers), consumer)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := map[int]Position{}
	for rows.Next() {
		var shard int
		var p Position
		if err := rows.Scan(&shard, &p.TS, &p.ID); err != nil {
			return nil, err
		}
		out[shard] = p
	}
	return out, rows.Err()
}

// The default partition has no upper bound, so its relpartbound does not match and max() ignores it.
// VERIFY on YugabyteDB: pg_get_expr on relpartbound.
const runwaySQL = `
SELECT (extract(epoch FROM max(((regexp_match(pg_get_expr(c.relpartbound, c.oid),
        'TO \(''([^'']+)''\)'))[1])::timestamptz) - now())
        / (86400 * 30.4375))::float8
FROM pg_inherits i JOIN pg_class c ON c.oid = i.inhrelid
WHERE i.inhparent = $1::regclass`

// PartitionRunwayMonths is the number of months from now to the last range partition's upper bound; ok
// is false when the table has no range partitions.
func PartitionRunwayMonths(ctx context.Context, q Querier, t Tables) (months float64, ok bool, err error) {
	var v sql.NullFloat64
	if err = q.QueryRowContext(ctx, runwaySQL, t.eventsRegclass).Scan(&v); err != nil {
		return 0, false, err
	}
	return v.Float64, v.Valid, nil
}

// DefaultPartitionRows counts rows in events_default, up to 1000.
func DefaultPartitionRows(ctx context.Context, q Querier, t Tables) (n int, err error) {
	err = q.QueryRowContext(ctx,
		fmt.Sprintf(`SELECT count(*) FROM (SELECT 1 FROM %s LIMIT 1000) s`, t.Default)).Scan(&n)
	return n, err
}

// TrimDeadLetters deletes up to limit dead letters older than days, and returns how many.
func TrimDeadLetters(ctx context.Context, q Querier, t Tables, days, limit int) (int64, error) {
	res, err := q.ExecContext(ctx, fmt.Sprintf(
		`DELETE FROM %[1]s WHERE id IN (`+
			`SELECT id FROM %[1]s WHERE created_at < now() - make_interval(days => $1) LIMIT $2)`,
		t.DeadLetters), days, limit)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}
