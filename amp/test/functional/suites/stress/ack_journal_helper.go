// A per-id record of what the product PROMISED a writer, so a later read can be
// held to an exact set rather than a row count.
package stress

import (
	"fmt"
	"slices"
	"sort"
	"strings"
	"sync"
)

// AckOutcome is what the client learned about one write.
type AckOutcome int

const (
	// OutcomeAcked: the write call returned success. The row MUST be present.
	OutcomeAcked AckOutcome = iota
	// OutcomeUnknown: the write call returned an error. Present or absent are
	// both legal — the statement may still have committed.
	OutcomeUnknown
	// OutcomeCancelled: the client cancelled the statement in flight. Same
	// verdict as unknown: both outcomes legal.
	OutcomeCancelled
)

func (o AckOutcome) String() string {
	switch o {
	case OutcomeAcked:
		return "acked"
	case OutcomeUnknown:
		return "unknown"
	case OutcomeCancelled:
		return "cancelled"
	}
	return fmt.Sprintf("outcome(%d)", int(o))
}

// AckCounts is the per-outcome tally, for step properties and reports.
type AckCounts struct {
	Issued    int
	Acked     int
	Unknown   int
	Cancelled int
}

// AckValueMismatch is one row whose stored value differs from what the writer
// recorded for that id.
type AckValueMismatch struct {
	ID    int64
	Want  int64
	Got   int64
	State AckOutcome
}

// AckVerification is the verdict of comparing a journal against storage.
type AckVerification struct {
	// Lost are acked ids absent from storage: a durability violation.
	Lost []int64
	// Unexpected are ids present in storage that were never issued.
	Unexpected []int64
	// Mismatched are present ids whose stored value is not the recorded one.
	Mismatched []AckValueMismatch
	// Present is how many stored ids were compared; AckedPresent how many of
	// them the journal had acked.
	Present      int
	AckedPresent int
	Counts       AckCounts
}

// OK reports a clean verification.
func (v AckVerification) OK() bool {
	return len(v.Lost) == 0 && len(v.Unexpected) == 0 && len(v.Mismatched) == 0
}

// Summary is the counts line to hang on a step or a failure message.
func (v AckVerification) Summary() string {
	return fmt.Sprintf("issued %d, acked %d, unknown %d, cancelled %d; stored %d (%d acked); lost %d, unexpected %d, mismatched %d",
		v.Counts.Issued, v.Counts.Acked, v.Counts.Unknown, v.Counts.Cancelled,
		v.Present, v.AckedPresent, len(v.Lost), len(v.Unexpected), len(v.Mismatched))
}

// AckJournal records the outcome and value of every id a writer issued. Safe for
// concurrent writers.
type AckJournal struct {
	mu      sync.Mutex
	entries map[int64]ackEntry
	order   []int64
}

type ackEntry struct {
	outcome AckOutcome
	value   int64
}

func NewAckJournal() *AckJournal {
	return &AckJournal{entries: map[int64]ackEntry{}}
}

// RecordAcked marks id as acknowledged with the value the writer sent.
func (j *AckJournal) RecordAcked(id, value int64) { j.record(id, value, OutcomeAcked) }

// RecordUnknown marks id as issued with an unknown outcome.
func (j *AckJournal) RecordUnknown(id, value int64) { j.record(id, value, OutcomeUnknown) }

// RecordCancelled marks id as cancelled in flight.
func (j *AckJournal) RecordCancelled(id, value int64) { j.record(id, value, OutcomeCancelled) }

// record applies the promotion rule: an ack cannot be retracted by a later
// record of the same id, because the promise was already made to the client.
func (j *AckJournal) record(id, value int64, outcome AckOutcome) {
	j.mu.Lock()
	defer j.mu.Unlock()
	prev, seen := j.entries[id]
	if !seen {
		j.order = append(j.order, id)
	}
	if seen && prev.outcome == OutcomeAcked {
		return
	}
	j.entries[id] = ackEntry{outcome: outcome, value: value}
}

// Counts is the per-outcome tally over distinct ids.
func (j *AckJournal) Counts() AckCounts {
	j.mu.Lock()
	defer j.mu.Unlock()
	c := AckCounts{Issued: len(j.entries)}
	for _, e := range j.entries {
		switch e.outcome {
		case OutcomeAcked:
			c.Acked++
		case OutcomeUnknown:
			c.Unknown++
		case OutcomeCancelled:
			c.Cancelled++
		}
	}
	return c
}

// Outcome reports what was recorded for id.
func (j *AckJournal) Outcome(id int64) (AckOutcome, bool) {
	j.mu.Lock()
	defer j.mu.Unlock()
	e, ok := j.entries[id]
	return e.outcome, ok
}

// IDs is every issued id in the order it was first recorded.
func (j *AckJournal) IDs() []int64 {
	j.mu.Lock()
	defer j.mu.Unlock()
	return slices.Clone(j.order)
}

// AckedIDs is every acked id, ascending.
func (j *AckJournal) AckedIDs() []int64 {
	j.mu.Lock()
	defer j.mu.Unlock()
	out := make([]int64, 0, len(j.entries))
	for id, e := range j.entries {
		if e.outcome == OutcomeAcked {
			out = append(out, id)
		}
	}
	sort.Slice(out, func(a, b int) bool { return out[a] < out[b] })
	return out
}

// Verify compares the journal against the ids present in storage and their
// stored values.
func (j *AckJournal) Verify(present map[int64]int64) AckVerification {
	return j.verify(present, true)
}

// VerifyIDs is Verify for a caller that read ids only, so no value is checked.
func (j *AckJournal) VerifyIDs(present []int64) AckVerification {
	m := make(map[int64]int64, len(present))
	for _, id := range present {
		m[id] = 0
	}
	return j.verify(m, false)
}

func (j *AckJournal) verify(present map[int64]int64, checkValues bool) AckVerification {
	j.mu.Lock()
	defer j.mu.Unlock()

	v := AckVerification{Present: len(present), Counts: AckCounts{Issued: len(j.entries)}}
	for id, e := range j.entries {
		switch e.outcome {
		case OutcomeAcked:
			v.Counts.Acked++
		case OutcomeUnknown:
			v.Counts.Unknown++
		case OutcomeCancelled:
			v.Counts.Cancelled++
		}
		if _, ok := present[id]; !ok && e.outcome == OutcomeAcked {
			v.Lost = append(v.Lost, id)
		}
	}
	for id, got := range present {
		e, issued := j.entries[id]
		if !issued {
			v.Unexpected = append(v.Unexpected, id)
			continue
		}
		if e.outcome == OutcomeAcked {
			v.AckedPresent++
		}
		if checkValues && e.value != got {
			v.Mismatched = append(v.Mismatched, AckValueMismatch{ID: id, Want: e.value, Got: got, State: e.outcome})
		}
	}
	sort.Slice(v.Lost, func(a, b int) bool { return v.Lost[a] < v.Lost[b] })
	sort.Slice(v.Unexpected, func(a, b int) bool { return v.Unexpected[a] < v.Unexpected[b] })
	sort.Slice(v.Mismatched, func(a, b int) bool { return v.Mismatched[a].ID < v.Mismatched[b].ID })
	return v
}

// formatIDs renders at most limit ids for a failure message.
func formatIDs(ids []int64, limit int) string {
	if len(ids) == 0 {
		return "none"
	}
	shown := ids
	if len(shown) > limit {
		shown = shown[:limit]
	}
	parts := make([]string, 0, len(shown))
	for _, id := range shown {
		parts = append(parts, fmt.Sprintf("%d", id))
	}
	s := strings.Join(parts, ", ")
	if len(ids) > len(shown) {
		s += fmt.Sprintf(", and %d more", len(ids)-len(shown))
	}
	return s
}
