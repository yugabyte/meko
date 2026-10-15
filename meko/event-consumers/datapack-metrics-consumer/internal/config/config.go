// Package config reads the consumer's settings from environment variables, so a Deployment can tune
// them without a rebuild. Out-of-range values are clamped and unparsable ones fall back to the default,
// both with a warning.
package config

import (
	"fmt"
	"log/slog"
	"regexp"
	"strconv"
	"strings"
	"time"

	"meko/event-consumers/datapack-metrics-consumer/internal/contract"
)

// Prefix is the start of every setting this package reads.
const Prefix = "MEKO_EVENTS_CONSUMER_"

var identifier = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// Config is the consumer's settings.
type Config struct {
	DSN      string
	Schema   string
	Consumer string // the consumer group: which event_consumers rows to use
	Shards   []int
	// MaxShards is the most shards one instance holds at once; 0 means no limit.
	MaxShards int
	BatchSize int
	// SafetyLag leaves the newest events unread. created_at is stamped at INSERT but a row is visible only
	// at COMMIT, so most rows that commit a little late are still read in order. A row that commits later
	// than the lag lands below an offset that has passed it; the look-back recovers those.
	SafetyLag time.Duration
	// LookBackWindow is how far below its offset a shard is re-read for events that committed late, and
	// so the longest commit stall that is recovered. It also sets how long event_applied rows are kept.
	LookBackWindow time.Duration
	// LookBackEvery is how often each shard is re-read. The look-back is a safety net and is nearly
	// always empty, so it runs far less often than the main read.
	LookBackEvery time.Duration
	Lease         time.Duration
	Renew         time.Duration
	// BatchTimeout cuts off one batch's database work. It is derived from the lease, because leases are
	// renewed between shards and a batch that outlived its lease would be fought over by another instance.
	BatchTimeout time.Duration
	PollMin      time.Duration
	PollMax      time.Duration
	Janitor      time.Duration
	// DeadLetterDays is how long dead letters are kept.
	DeadLetterDays int
	// MaxDeadLettersPerPass stops isolating a batch after this many dead letters in one pass: a handler
	// bug should pause the shard, not empty the queue into the dead-letter table.
	MaxDeadLettersPerPass int
	HTTPPort              int
}

// FromEnv builds a Config from getenv (os.Getenv in production).
func FromEnv(getenv func(string) string) (Config, error) {
	r := reader{getenv: getenv}
	dsn := strings.TrimSpace(getenv("YUGABYTEDB_SYSTEM_URL"))
	if dsn == "" {
		dsn = strings.TrimSpace(getenv("YUGABYTEDB_URL"))
	}
	if dsn == "" {
		return Config{}, fmt.Errorf("YUGABYTEDB_SYSTEM_URL or YUGABYTEDB_URL must be set")
	}
	schema := r.text("SCHEMA", "meko_system")
	if !identifier.MatchString(schema) {
		return Config{}, fmt.Errorf("%sSCHEMA %q is not a plain identifier", Prefix, schema)
	}

	lease := r.seconds("LEASE_SECONDS", 30, 5, 600)
	renew := min(r.seconds("RENEW_SECONDS", 10, 1, 300), lease/2)
	// Leases are renewed inside Tick, and an idle loop sleeps up to PollMax between ticks, so a PollMax longer
	// than the renewal interval would let an idle holder's leases lapse every cycle.
	pollMin := time.Duration(r.integer("POLL_MIN_MS", 500, 10, 60000)) * time.Millisecond
	pollMax := time.Duration(r.integer("POLL_MAX_MS", 2000, 10, 60000)) * time.Millisecond
	if pollMax > renew {
		slog.Warn("POLL_MAX_MS is longer than the lease renewal interval; using the renewal interval",
			"poll_max", pollMax, "renew", renew)
		pollMax = renew
	}
	pollMin = min(pollMin, pollMax)
	shards := make([]int, contract.ShardCount)
	for i := range shards {
		shards[i] = i
	}
	return Config{
		DSN:                   dsn,
		Schema:                schema,
		Consumer:              r.text("CONSUMER", "analytics"),
		Shards:                shards,
		MaxShards:             r.integer("MAX_SHARDS", 0, 0, contract.ShardCount),
		BatchSize:             r.integer("BATCH_SIZE", 500, 1, 5000),
		SafetyLag:             r.seconds("SAFETY_LAG_SECONDS", 3, 0, 60),
		LookBackWindow:        r.seconds("LOOKBACK_WINDOW_SECONDS", 300, 30, 86400),
		LookBackEvery:         r.seconds("LOOKBACK_INTERVAL_SECONDS", 30, 5, 3600),
		Lease:                 lease,
		Renew:                 renew,
		BatchTimeout:          max(2*time.Second, lease-renew-lease/6),
		PollMin:               pollMin,
		PollMax:               pollMax,
		Janitor:               r.seconds("JANITOR_SECONDS", 60, 5, 3600),
		DeadLetterDays:        r.integer("DEAD_LETTER_DAYS", 14, 1, 365),
		MaxDeadLettersPerPass: r.integer("MAX_DEAD_LETTERS_PER_PASS", 20, 1, 1000),
		HTTPPort:              r.integer("HTTP_PORT", 8080, 0, 65535),
	}, nil
}

type reader struct{ getenv func(string) string }

func (r reader) raw(name string) string { return strings.TrimSpace(r.getenv(Prefix + name)) }

func (r reader) text(name, def string) string {
	if v := r.raw(name); v != "" {
		return v
	}
	return def
}

func (r reader) integer(name string, def, lo, hi int) int {
	raw := r.raw(name)
	if raw == "" {
		return def
	}
	v, err := strconv.Atoi(raw)
	if err != nil {
		slog.Warn("setting is not a number; using the default", "name", Prefix+name, "default", def)
		return def
	}
	return clamp(name, v, lo, hi)
}

func (r reader) seconds(name string, def, lo, hi float64) time.Duration {
	raw := r.raw(name)
	v := def
	if raw != "" {
		f, err := strconv.ParseFloat(raw, 64)
		if err != nil {
			slog.Warn("setting is not a number; using the default", "name", Prefix+name, "default", def)
		} else {
			v = clamp(name, f, lo, hi)
		}
	}
	return time.Duration(v * float64(time.Second))
}

func clamp[T int | float64](name string, v, lo, hi T) T {
	if v < lo || v > hi {
		c := min(max(v, lo), hi)
		slog.Warn("setting is outside its range; clamped", "name", Prefix+name, "value", v, "using", c)
		return c
	}
	return v
}
