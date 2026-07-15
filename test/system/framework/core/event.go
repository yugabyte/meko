package core

import "time"

// EventKind tags a ResultEvent.
type EventKind int

const (
	EventTestStarted EventKind = iota
	EventTestFinished
	EventStepStarted
	EventStepFinished
	EventAttachment
	EventMetric
	EventDatapoint
	EventChart
)

// ResultEvent is a single live event about a test's progress. The Reporter fans
// these out to sinks. Only the field(s) relevant to Kind are populated.
type ResultEvent struct {
	Kind       EventKind
	TestName   string
	Result     *Result // TestStarted / TestFinished
	Step       *Step   // StepStarted / StepFinished
	Attachment *Attachment
	Metric     *Metric
	Datapoint  *Datapoint
	Chart      *Chart
	// Content carries an attachment's raw bytes for EventAttachment events
	// emitted via T.AttachBytes, so a persisting sink (jsonfile) can write them
	// to disk. It is transient: it is NEVER serialized into a recorded Result —
	// only the Attachment metadata is. Nil for metadata-only attaches (T.Attach).
	Content []byte
}

// RunInfo opens a run.
type RunInfo struct {
	Seed      int64
	Total     int
	Parallel  int
	StartedAt time.Time
}

// RunSummary closes a run.
type RunSummary struct {
	Counts    map[Status]int
	Total     int
	StartedAt time.Time
	EndedAt   time.Time
	Dropped   int64 // reporter events dropped on overflow
}

// Sink is a reporter backend. A Reporter fans out to N sinks; sinks must not
// block the test (bounded queues, drop-with-counter on overflow for non-critical
// sinks).
type Sink interface {
	RunStarted(RunInfo) error
	ResultEvent(ResultEvent) error
	RunFinished(RunSummary) error
}

// emitter is the per-test event channel handed to T. It carries the test name
// so a single sink stream can be demultiplexed.
type emitter interface {
	emit(ResultEvent)
}

// sinkEmitter adapts a Sink into a per-test emitter.
type sinkEmitter struct {
	sink     Sink
	testName string
}

func (e sinkEmitter) emit(ev ResultEvent) {
	if e.sink == nil {
		return
	}
	ev.TestName = e.testName
	_ = e.sink.ResultEvent(ev)
}
