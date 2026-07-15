package core

// Direction says which way a Datapoint moving counts as a regression: a
// throughput number is HigherIsBetter (a drop is bad), a latency number is
// LowerIsBetter (a rise is bad). The baseline gate reads it to decide which
// side of a change to penalize.
type Direction string

const (
	HigherIsBetter Direction = "higher_is_better"
	LowerIsBetter  Direction = "lower_is_better"
)

// Datapoint is a named performance measurement recorded on a result and gated
// against a baseline. Unlike a Metric (freeform observability), a Datapoint is
// an SLO-comparable number: Direction says which way is worse, and Baseline —
// once a perf test has gated it — carries the comparison against the recorded
// baseline value. It is deliberately small and typed (no free-form maps).
type Datapoint struct {
	Name      string
	Value     float64
	Unit      string
	Direction Direction
	// Baseline is the gate's comparison against the stored baseline, or nil when
	// no baseline existed (a first run) or the datapoint was emitted ungated.
	Baseline *BaselineComparison
}

// BaselineComparison is a Datapoint's comparison against its baseline value:
// the signed percent change, the allowed regression threshold, and whether the
// change crossed it in the worse direction.
type BaselineComparison struct {
	Value     float64 // the baseline value compared against
	DeltaPct  float64 // signed percent change of Value vs baseline ((v-b)/b*100)
	Threshold float64 // allowed regression percent in the worse direction (>=0)
	Regressed bool    // the change exceeded Threshold in Direction's worse side
}

// PerfIdentity is the self-describing provenance of a perf result: what was
// under test (YB version/image, amp images, framework commit) and the
// load_signature — a deterministic hash of the load config. Two perf results
// are comparable iff their LoadSignature matches (identical load); the fields
// above then say which thing-under-test differed. It is deliberately small and
// typed (no free-form maps) like Datapoint/Chart, rendered as a small "variant"
// block per perf result and persisted so `ampqa report --from` re-renders it.
type PerfIdentity struct {
	// Variant is the comparison variant label (comparison mode), or a short
	// summary of the comparison; empty for a plain single run.
	Variant            string `json:"variant,omitempty"`
	YBVersion          string `json:"yb_version,omitempty"`
	YBImage            string `json:"yb_image,omitempty"`
	AmpControllerImage string `json:"amp_controller_image,omitempty"`
	AmpEndpointImage   string `json:"amp_endpoint_image,omitempty"`
	Commit             string `json:"commit,omitempty"`
	LoadSignature      string `json:"load_signature,omitempty"`
}

// SetPerfIdentity records this result's self-describing identity (the YB/amp
// images, framework commit, and load_signature). The report renders it as a
// small variant block; the jsonfile sink persists it.
func (t *T) SetPerfIdentity(id PerfIdentity) {
	t.mu.Lock()
	t.result.PerfIdentity = &id
	t.mu.Unlock()
}

// Datapoint records a performance datapoint on the result and emits it. The
// caller builds the fully-formed Datapoint (including any baseline comparison)
// so every sink renders the same gated value.
func (t *T) Datapoint(dp Datapoint) {
	t.mu.Lock()
	t.result.Perf = append(t.result.Perf, dp)
	t.mu.Unlock()
	if t.emit != nil {
		t.emit.emit(ResultEvent{Kind: EventDatapoint, Datapoint: &dp})
	}
}
