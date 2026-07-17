package core

import "time"

// Status is the honest outcome of a test, set by where the failure happened.
type Status string

const (
	StatusPassed    Status = "passed"     // assertions + invariants green
	StatusFailed    Status = "failed"     // product defect signal during RUN
	StatusEnvFailed Status = "env_failed" // env could not be provisioned/attached
	StatusBroken    Status = "broken"     // framework bug (panic, impossible state)
	StatusTimedOut  Status = "timed_out"  // deadline exceeded
	StatusSkipped   Status = "skipped"    // not run (filtered, bounds, dependency)
)

// AllStatuses lists every status, in reporting order.
var AllStatuses = []Status{
	StatusPassed, StatusFailed, StatusEnvFailed, StatusBroken, StatusTimedOut, StatusSkipped,
}

// Result is one execution of a test.
type Result struct {
	TestName     string
	Suite        string
	Description  string // the test's registered one-line description (what it checks)
	OwningTeam   string // the test's registered owning team, for regression routing
	Category     Category
	Status       Status
	ErrorSummary string
	StartedAt    time.Time
	EndedAt      time.Time
	Duration     time.Duration
	Seed         int64
	Config       map[string]any // resolved config snapshot
	Versions     DetectedVersions
	SkipReason   string
	Steps        []*Step
	Attachments  []Attachment
	Metrics      []Metric
	// Perf holds the SLO-comparable performance datapoints a perf test emits
	// (throughput, latency), each optionally carrying its baseline comparison.
	// Kept unbounded: a perf test emits a handful, not per-cycle events.
	Perf []Datapoint
	// Charts holds the graphs a test emitted with T.Chart, rendered as inline
	// SVG in the HTML report. Kept unbounded for the same reason as Perf: a test
	// emits a handful, not per-cycle.
	Charts []Chart
	// PerfIdentity is the self-describing provenance of a perf result (YB/amp
	// images, framework commit, load_signature). Nil for non-perf tests. It is
	// what makes two perf results comparable (matching LoadSignature) and is
	// rendered as a small variant block.
	PerfIdentity *PerfIdentity `json:",omitempty"`
	// StepsDropped/MetricsDropped count events dropped from the retained slices
	// once the per-test retention cap was hit (report.max_steps_retained /
	// report.max_metrics_retained). The live sink already received every event;
	// only the in-memory snapshot is bounded, so a long soak can't balloon RAM.
	StepsDropped   int
	MetricsDropped int
}

// Attachment is a pointer to a collected blob (log, core, config, artifact).
type Attachment struct {
	Name     string
	Kind     string
	Size     int64
	StepName string
	// Path is the run-relative location a persisting sink wrote the bytes to
	// (e.g. "artifacts/<test>/<name>"). Empty for metadata-only attaches, or
	// when no sink persisted the bytes.
	Path string
}

// Metric is a single named measurement emitted during a test.
type Metric struct {
	Key   string
	Value float64
	Unit  string
}
