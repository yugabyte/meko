package core

import (
	"sync"
	"time"
)

// Step is a named, timed, status-bearing span inside a test. Steps may nest;
// each step is recorded into the result and streamed to the reporter as events.
type Step struct {
	Name       string
	Status     Status
	Depth      int
	Properties map[string]any
	StartedAt  time.Time
	EndedAt    time.Time
	Steps      []*Step

	// Guards Properties. Events carry this *Step and sinks drain them on their own
	// goroutines, so a sink can read the map while the body is still setting it.
	propMu sync.Mutex
}

// Duration is the wall-clock span of the step.
func (s *Step) Duration() time.Duration { return s.EndedAt.Sub(s.StartedAt) }

// PropsSnapshot returns a copy of the step's properties. Every event consumer
// must read properties through this, never the live Properties map.
func (s *Step) PropsSnapshot() map[string]any {
	s.propMu.Lock()
	defer s.propMu.Unlock()
	out := make(map[string]any, len(s.Properties))
	for k, v := range s.Properties {
		out[k] = v
	}
	return out
}

// maskProps rewrites string-valued properties in place under the step's lock.
func (s *Step) maskProps(masker *Masker) {
	s.propMu.Lock()
	defer s.propMu.Unlock()
	for k, v := range s.Properties {
		if str, ok := v.(string); ok {
			s.Properties[k] = masker.Mask(str)
		}
	}
}

// StepHandle is passed to a step body; it carries properties, allows nesting,
// and mediates recording under the test's lock.
type StepHandle struct {
	step    *Step
	emit    emitter
	mu      *sync.Mutex // shared with T; guards result/step trees
	appendf func(child *Step)
}

// Set records a property on the step.
func (h *StepHandle) Set(key string, val any) {
	h.step.propMu.Lock()
	defer h.step.propMu.Unlock()
	if h.step.Properties == nil {
		h.step.Properties = map[string]any{}
	}
	h.step.Properties[key] = val
}

// Step runs fn inside a nested child step.
func (h *StepHandle) Step(name string, fn func(*StepHandle) error) error {
	return runStep(name, h.step.Depth+1, h.emit, h.mu, func(child *Step) {
		h.mu.Lock()
		h.step.Steps = append(h.step.Steps, child)
		h.mu.Unlock()
	}, fn)
}

// runStep is the shared span machinery for T.Step and StepHandle.Step. It emits
// StepStarted/StepFinished, times the body, and derives status. A panic marks
// the step broken, emits, and re-panics for the lifecycle to classify.
func runStep(name string, depth int, emit emitter, mu *sync.Mutex, attach func(*Step), fn func(*StepHandle) error) (err error) {
	step := &Step{
		Name:      name,
		Depth:     depth,
		Status:    StatusPassed,
		StartedAt: time.Now(),
	}
	attach(step)
	if emit != nil {
		emit.emit(ResultEvent{Kind: EventStepStarted, Step: step})
	}

	handle := &StepHandle{step: step, emit: emit, mu: mu}

	defer func() {
		step.EndedAt = time.Now()
		if r := recover(); r != nil {
			step.Status = StatusBroken
			if emit != nil {
				emit.emit(ResultEvent{Kind: EventStepFinished, Step: step})
			}
			panic(r)
		}
		if err != nil {
			step.Status = StatusFailed
		}
		if emit != nil {
			emit.emit(ResultEvent{Kind: EventStepFinished, Step: step})
		}
	}()

	err = fn(handle)
	return
}
