package core_test

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/yugabyte/meko/meko/test/functional/framework/core"
	"github.com/yugabyte/meko/meko/test/functional/framework/env/fake"
	"github.com/yugabyte/meko/meko/test/functional/framework/report"
)

// propSpinSink reads a started step's properties on the reporter's goroutine
// until the body signals it is done, the way a real async sink races the body.
type propSpinSink struct {
	stop      chan struct{}
	reading   chan struct{} // closed after the first snapshot
	once      sync.Once
	snapshots atomic.Int64
}

func (p *propSpinSink) RunStarted(core.RunInfo) error     { return nil }
func (p *propSpinSink) RunFinished(core.RunSummary) error { return nil }

func (p *propSpinSink) ResultEvent(ev core.ResultEvent) error {
	if ev.Kind != core.EventStepStarted || ev.Step == nil {
		return nil
	}
	for {
		select {
		case <-p.stop:
			return nil
		default:
		}
		for range ev.Step.PropsSnapshot() {
		}
		p.snapshots.Add(1)
		p.once.Do(func() { close(p.reading) })
	}
}

func TestStepPropertiesSurviveConcurrentSinkReads(t *testing.T) {
	const keys = 32
	sink := &propSpinSink{stop: make(chan struct{}), reading: make(chan struct{})}
	rep := report.NewReporter(sink)

	test := core.Test{Name: "core/step-props", Suite: "core", Run: func(tt *core.T) error {
		return tt.Step("props", func(h *core.StepHandle) error {
			defer close(sink.stop)
			// Wait for the sink to actually be reading before writing, instead
			// of hoping it gets scheduled inside the body. Without this the
			// body could finish first and the race window went unexercised,
			// the test then failed for its own timing, roughly one run in five.
			select {
			case <-sink.reading:
			case <-time.After(30 * time.Second):
				return fmt.Errorf("the sink never started reading the step's properties")
			}
			for i := 0; i < 20000; i++ {
				h.Set(fmt.Sprintf("k%d", i%keys), i)
			}
			return nil
		})
	}}

	_, results := core.RunTests(context.Background(), []core.Test{test}, core.RunOptions{
		Parallel: 1,
		Seed:     core.NewRunSeed(1, true),
		Config:   core.NewConfig(),
		Provider: &fake.Provider{},
		Masker:   core.NewMasker(),
		Sink:     rep,
	})
	rep.Close()

	if len(results) != 1 || results[0].Status != core.StatusPassed {
		t.Fatalf("run did not pass: %+v", results)
	}
	if sink.snapshots.Load() == 0 {
		t.Fatal("sink never read the step properties, the race window was not exercised")
	}
	if got := len(results[0].Steps[0].Properties); got != keys {
		t.Errorf("recorded %d properties, want %d", got, keys)
	}
}

func TestPropsSnapshotIsACopy(t *testing.T) {
	p := &fake.Provider{}
	test := core.Test{Name: "core/step-copy", Suite: "core", Run: func(tt *core.T) error {
		return tt.Step("props", func(h *core.StepHandle) error {
			h.Set("a", 1)
			return nil
		})
	}}
	_, results := core.RunTests(context.Background(), []core.Test{test}, core.RunOptions{
		Parallel: 1, Seed: core.NewRunSeed(1, true), Config: core.NewConfig(), Provider: p,
	})
	step := results[0].Steps[0]

	snap := step.PropsSnapshot()
	snap["a"] = "tampered"
	delete(snap, "a")
	if step.Properties["a"] != 1 {
		t.Errorf("live properties changed through the snapshot: %v", step.Properties["a"])
	}
}
