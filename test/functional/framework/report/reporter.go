// Package report provides the Reporter fan-out and its sinks. The Reporter
// implements core.Sink by dispatching every event to N backing sinks over
// bounded, non-blocking per-sink queues (drop-with-counter on overflow for
// non-critical sinks) so a slow sink never blocks a test. Sinks are plain
// core.Sink implementations, not a registry: report/stdout for live progress,
// report/jsonfile for the per-test JSON + manifest, report/junit for CI XML,
// and report/html for a self-contained HTML report (rendered live via its
// Sink, or after the fact from a completed run's output via `ampqa report`).
package report

import (
	"sync"
	"sync/atomic"

	"github.com/yugabyte/yb-amp/test/functional/framework/core"
)

// DefaultQueueSize is the per-sink buffer used by NewReporter.
const DefaultQueueSize = 1024

type eventKind int

const (
	kindRunStarted eventKind = iota
	kindResult
	kindRunFinished
)

type queued struct {
	kind    eventKind
	run     core.RunInfo
	result  core.ResultEvent
	summary core.RunSummary
}

// SinkOption pairs a sink with its overflow policy. Critical sinks block on a
// full queue instead of dropping.
type SinkOption struct {
	Sink     core.Sink
	Critical bool
}

type worker struct {
	sink     core.Sink
	ch       chan queued
	critical bool
	dropped  atomic.Int64
	done     chan struct{}
}

func (w *worker) run() {
	defer close(w.done)
	for e := range w.ch {
		switch e.kind {
		case kindRunStarted:
			_ = w.sink.RunStarted(e.run)
		case kindResult:
			_ = w.sink.ResultEvent(e.result)
		case kindRunFinished:
			_ = w.sink.RunFinished(e.summary)
		}
	}
}

func (w *worker) enqueue(e queued) {
	if w.critical {
		w.ch <- e
		return
	}
	select {
	case w.ch <- e:
	default:
		w.dropped.Add(1)
	}
}

// Reporter fans out to a fixed set of sinks.
type Reporter struct {
	workers []*worker
	closed  atomic.Bool
	once    sync.Once
}

// NewReporter builds a Reporter over non-critical sinks with a default queue.
func NewReporter(sinks ...core.Sink) *Reporter {
	opts := make([]SinkOption, len(sinks))
	for i, s := range sinks {
		opts[i] = SinkOption{Sink: s}
	}
	return NewReporterWithOptions(opts, DefaultQueueSize)
}

// NewReporterWithOptions builds a Reporter with explicit per-sink policies.
func NewReporterWithOptions(opts []SinkOption, queueSize int) *Reporter {
	if queueSize < 1 {
		queueSize = 1
	}
	r := &Reporter{}
	for _, o := range opts {
		w := &worker{
			sink:     o.Sink,
			ch:       make(chan queued, queueSize),
			critical: o.Critical,
			done:     make(chan struct{}),
		}
		go w.run()
		r.workers = append(r.workers, w)
	}
	return r
}

func (r *Reporter) enqueue(e queued) {
	if r.closed.Load() {
		return
	}
	for _, w := range r.workers {
		w.enqueue(e)
	}
}

// RunStarted implements core.Sink.
func (r *Reporter) RunStarted(info core.RunInfo) error {
	r.enqueue(queued{kind: kindRunStarted, run: info})
	return nil
}

// ResultEvent implements core.Sink.
func (r *Reporter) ResultEvent(ev core.ResultEvent) error {
	r.enqueue(queued{kind: kindResult, result: ev})
	return nil
}

// RunFinished implements core.Sink.
func (r *Reporter) RunFinished(s core.RunSummary) error {
	r.enqueue(queued{kind: kindRunFinished, summary: s})
	return nil
}

// Dropped is the total number of events dropped across all sinks.
func (r *Reporter) Dropped() int64 {
	var total int64
	for _, w := range r.workers {
		total += w.dropped.Load()
	}
	return total
}

// Close stops accepting events, drains each sink's buffered events, and waits
// for all sink workers to finish.
func (r *Reporter) Close() {
	r.once.Do(func() {
		r.closed.Store(true)
		for _, w := range r.workers {
			close(w.ch)
		}
		for _, w := range r.workers {
			<-w.done
		}
	})
}
