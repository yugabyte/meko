// Package handlers holds the consumer's event handlers: what each event type does to the database.
package handlers

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"math"
	"sort"
	"time"

	"github.com/google/uuid"
	"github.com/lib/pq"

	"meko/event-consumers/datapack-metrics-consumer/internal/consumer"
	"meko/event-consumers/datapack-metrics-consumer/internal/store"
)

// SearchCompleted is the type of the event context_search emits when it finishes.
const SearchCompleted = "search.completed"

// Register adds every handler to reg. The bookkeeping tables are in schema.
func Register(reg *consumer.Registry, schema string) error {
	return reg.Register(SearchCompleted, NewSearchCompleted(schema))
}

// searchPayload is the search.completed v1 payload, as the MCP server writes it. Unknown fields are ignored
// and a newer version is read the same way, because evolution is additive.
type searchPayload struct {
	SearchType     string `json:"search_type"`
	DatapackID     string `json:"datapack_id"`
	ConversationID string `json:"conversation_id"`
	UserID         string `json:"user_id"`
	TokensSent     int64  `json:"tokens_sent"`
	TokensIfRaw    int64  `json:"tokens_if_raw"`
	HitsHigh       int64  `json:"hits_high"`
	HitsMid        int64  `json:"hits_mid"`
	HitsLow        int64  `json:"hits_low"`
	QuotaMetric    string `json:"quota_metric"`
	// QuotaAudited is true when the inline code would have written a free_tier_audit_log row for this
	// call: the quota gate let it through and the tenant is known.
	QuotaAudited bool `json:"quota_audited"`
	// OccurredAt is when the search finished (RFC 3339). The producer queues events and inserts them in
	// batches, so created_at is the insert time and can be seconds later; a search just before midnight
	// UTC must still count for its own day.
	// Any type is accepted so a malformed value cannot fail the decode and dead-letter the whole event;
	// eventTime uses it only when it is a valid time string.
	OccurredAt any `json:"occurred_at"`
}

// eventTime is when the search happened: occurred_at if the producer sent a valid one, otherwise the row's
// created_at. An occurred_at after created_at is clock skew between the producer and the database, and the
// database's time wins, so a skewed pod cannot put counts on a future day.
func eventTime(e store.Event, occurredAt any) time.Time {
	at := e.CreatedAt.UTC()
	text, ok := occurredAt.(string)
	if !ok || text == "" {
		return at
	}
	t, err := time.Parse(time.RFC3339Nano, text)
	if err != nil || t.After(e.CreatedAt) {
		return at
	}
	return t.UTC()
}

type dailyKey struct{ datapack, date, searchType string }

type summaryKey struct{ datapack, source string }

type summaryDelta struct{ requests, saved, ifRaw, high, mid, low int64 }

type logRow struct {
	datapack, user, conversation, source string
	sent, ifRaw                          int64
	at                                   time.Time
}

type auditRow struct {
	user, metric string
	at           time.Time
}

// plan is what a batch of events does to the tables, with counters already coalesced per key: a multi-row
// upsert cannot touch the same key twice.
type plan struct {
	daily   map[dailyKey]int64
	summary map[summaryKey]*summaryDelta
	log     []logRow
	audit   []auditRow
	// tokenRowsSkipped counts searches whose token-savings rows were left out because a value did not fit.
	tokenRowsSkipped int
}

// fitsInt4 reports whether v fits the INT columns of token_savings_log.
func fitsInt4(v int64) bool { return v >= math.MinInt32 && v <= math.MaxInt32 }

// addTokenRows adds the token_savings_log row and the token_savings_summary delta for one search.
func (p *plan) addTokenRows(datapack, user, conversation string, s searchPayload, at time.Time) {
	p.log = append(p.log, logRow{datapack, user, conversation, s.SearchType, s.TokensSent, s.TokensIfRaw, at})
	d := p.summary[summaryKey{datapack, s.SearchType}]
	if d == nil {
		d = &summaryDelta{}
		p.summary[summaryKey{datapack, s.SearchType}] = d
	}
	d.requests++
	d.saved += max(0, s.TokensIfRaw-s.TokensSent) // the log keeps the raw numbers; the summary never goes negative
	d.ifRaw += s.TokensIfRaw
	d.high += s.HitsHigh
	d.mid += s.HitsMid
	d.low += s.HitsLow
}

// canonicalUUID returns s in canonical form, or false when it is not a UUID, the same test the inline
// code makes before it writes.
func canonicalUUID(s string) (string, bool) {
	u, err := uuid.Parse(s)
	if err != nil {
		return "", false
	}
	return u.String(), true
}

// planBatch decodes events and works out the writes. It does the same checks the inline code makes: nothing
// is counted for a call whose datapack is not a UUID; the token-savings rows need a UUID conversation as
// well, and only calls that sent tokens; an invalid user is stored as NULL; an audit row needs a valid user.
// The activity date is the UTC date of when the search happened (occurred_at, else created_at), so a
// late-processed event lands on the day it happened.
func planBatch(events []store.Event) (*plan, error) {
	p := &plan{daily: map[dailyKey]int64{}, summary: map[summaryKey]*summaryDelta{}}
	for _, e := range events {
		var s searchPayload
		if err := json.Unmarshal(e.Payload, &s); err != nil {
			return nil, fmt.Errorf("event %s: payload is not a search.completed payload: %w", e.ID, err)
		}
		at := eventTime(e, s.OccurredAt)
		datapack, datapackOK := canonicalUUID(s.DatapackID)

		if datapackOK {
			p.daily[dailyKey{datapack, at.Format("2006-01-02"), s.SearchType}]++
		}

		conversation, conversationOK := canonicalUUID(s.ConversationID)
		user, _ := canonicalUUID(s.UserID) // "" when absent or invalid, stored as NULL
		if datapackOK && conversationOK && s.TokensSent > 0 {
			if fitsInt4(s.TokensSent) && fitsInt4(s.TokensIfRaw) {
				p.addTokenRows(datapack, user, conversation, s, at)
			} else {
				// token_savings_log keeps these as INT. The inline code writes the log row and the summary
				// upsert in one transaction, so a value that does not fit loses both rows there, and the
				// daily count and the audit row (separate writes) survive. Do the same: failing the batch
				// would dead-letter the whole event, count and audit row included, on every search against
				// a datapack with more than about 2.1 million chunks.
				p.tokenRowsSkipped++
			}
		}

		if s.QuotaAudited && user != "" {
			p.audit = append(p.audit, auditRow{user, s.QuotaMetric, at})
		}
	}
	return p, nil
}

// NewSearchCompleted returns the handler for search.completed events. It writes the four bookkeeping tables
// in schema, in the batch's transaction, so a rolled-back batch leaves nothing behind.
func NewSearchCompleted(schema string) consumer.Handler {
	t := tables{
		daily:   pq.QuoteIdentifier(schema) + ".datapack_daily_activity",
		log:     pq.QuoteIdentifier(schema) + ".token_savings_log",
		summary: pq.QuoteIdentifier(schema) + ".token_savings_summary",
		audit:   pq.QuoteIdentifier(schema) + ".free_tier_audit_log",
	}
	return func(ctx context.Context, tx *sql.Tx, events []store.Event) error {
		p, err := planBatch(events)
		if p != nil && p.tokenRowsSkipped > 0 {
			slog.Warn("token-savings rows skipped: a token count does not fit the INT columns",
				"searches", p.tokenRowsSkipped, "of", len(events))
		}
		if err != nil {
			return err
		}
		return p.apply(ctx, tx, t)
	}
}

type tables struct{ daily, log, summary, audit string }

// apply runs the writes. Keys are sorted so concurrent consumers take row locks in the same order.
func (p *plan) apply(ctx context.Context, tx *sql.Tx, t tables) error {
	if len(p.daily) > 0 {
		keys := make([]dailyKey, 0, len(p.daily))
		for k := range p.daily {
			keys = append(keys, k)
		}
		sort.Slice(keys, func(i, j int) bool {
			a, b := keys[i], keys[j]
			return a.datapack+a.date+a.searchType < b.datapack+b.date+b.searchType
		})
		var dp, date, typ []string
		var n []int64
		for _, k := range keys {
			dp, date, typ, n = append(dp, k.datapack), append(date, k.date), append(typ, k.searchType), append(n, p.daily[k])
		}
		if _, err := tx.ExecContext(ctx, fmt.Sprintf(
			`INSERT INTO %s AS t (datapack_id, activity_date, search_type, query_count, updated_at)
			 SELECT d::uuid, a::date, s, n, now()
			 FROM unnest($1::text[], $2::text[], $3::text[], $4::bigint[]) AS x(d, a, s, n)
			 ON CONFLICT (datapack_id, activity_date, search_type) DO UPDATE
			 SET query_count = t.query_count + EXCLUDED.query_count, updated_at = now()`, t.daily),
			pq.Array(dp), pq.Array(date), pq.Array(typ), pq.Array(n)); err != nil {
			return fmt.Errorf("datapack_daily_activity: %w", err)
		}
	}

	if len(p.log) > 0 {
		var dp, user, conv, src, at []string
		var sent, raw []int64
		for _, r := range p.log {
			dp, user, conv, src = append(dp, r.datapack), append(user, r.user), append(conv, r.conversation), append(src, r.source)
			sent, raw, at = append(sent, r.sent), append(raw, r.ifRaw), append(at, r.at.Format(time.RFC3339Nano))
		}
		if _, err := tx.ExecContext(ctx, fmt.Sprintf(
			`INSERT INTO %s (datapack_id, user_id, conversation_id, source, tokens_sent, tokens_if_raw, created_at)
			 SELECT d::uuid, NULLIF(u, '')::uuid, c::uuid, s, ts, tr, at::timestamptz
			 FROM unnest($1::text[], $2::text[], $3::text[], $4::text[], $5::int[], $6::int[], $7::text[])
			      AS x(d, u, c, s, ts, tr, at)`, t.log),
			pq.Array(dp), pq.Array(user), pq.Array(conv), pq.Array(src), pq.Array(sent), pq.Array(raw), pq.Array(at)); err != nil {
			return fmt.Errorf("token_savings_log: %w", err)
		}
	}

	if len(p.summary) > 0 {
		keys := make([]summaryKey, 0, len(p.summary))
		for k := range p.summary {
			keys = append(keys, k)
		}
		sort.Slice(keys, func(i, j int) bool { return keys[i].datapack+keys[i].source < keys[j].datapack+keys[j].source })
		var dp, src []string
		var req, saved, raw, hi, mid, lo []int64
		for _, k := range keys {
			d := p.summary[k]
			dp, src = append(dp, k.datapack), append(src, k.source)
			req, saved, raw = append(req, d.requests), append(saved, d.saved), append(raw, d.ifRaw)
			hi, mid, lo = append(hi, d.high), append(mid, d.mid), append(lo, d.low)
		}
		// The score columns are nullable (rows from before they existed have none), hence the COALESCE.
		if _, err := tx.ExecContext(ctx, fmt.Sprintf(
			`INSERT INTO %s AS t (datapack_id, source, request_count, tokens_saved_total, tokens_if_raw_total,
			                      score_high_total, score_mid_total, score_low_total, updated_at)
			 SELECT d::uuid, s, n, sv, rw, h, m, l, now()
			 FROM unnest($1::text[], $2::text[], $3::bigint[], $4::bigint[], $5::bigint[], $6::bigint[], $7::bigint[], $8::bigint[])
			      AS x(d, s, n, sv, rw, h, m, l)
			 ON CONFLICT (datapack_id, source) DO UPDATE
			 SET request_count       = t.request_count + EXCLUDED.request_count,
			     tokens_saved_total  = t.tokens_saved_total + EXCLUDED.tokens_saved_total,
			     tokens_if_raw_total = t.tokens_if_raw_total + EXCLUDED.tokens_if_raw_total,
			     score_high_total    = COALESCE(t.score_high_total, 0) + EXCLUDED.score_high_total,
			     score_mid_total     = COALESCE(t.score_mid_total, 0) + EXCLUDED.score_mid_total,
			     score_low_total     = COALESCE(t.score_low_total, 0) + EXCLUDED.score_low_total,
			     updated_at          = now()`, t.summary),
			pq.Array(dp), pq.Array(src), pq.Array(req), pq.Array(saved), pq.Array(raw), pq.Array(hi), pq.Array(mid), pq.Array(lo)); err != nil {
			return fmt.Errorf("token_savings_summary: %w", err)
		}
	}

	if len(p.audit) > 0 {
		var user, metric, at []string
		for _, r := range p.audit {
			user, metric, at = append(user, r.user), append(metric, r.metric), append(at, r.at.Format(time.RFC3339Nano))
		}
		if _, err := tx.ExecContext(ctx, fmt.Sprintf(
			`INSERT INTO %s (user_id, metric_name, created_at)
			 SELECT u::uuid, m, at::timestamptz FROM unnest($1::text[], $2::text[], $3::text[]) AS x(u, m, at)`, t.audit),
			pq.Array(user), pq.Array(metric), pq.Array(at)); err != nil {
			return fmt.Errorf("free_tier_audit_log: %w", err)
		}
	}
	return nil
}
