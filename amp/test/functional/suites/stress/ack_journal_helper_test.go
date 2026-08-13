package stress

import (
	"fmt"
	"slices"
	"sync"
	"testing"

	"github.com/yugabyte/amp/test/functional/framework/core"
)

func presentSet(ids ...int64) map[int64]int64 {
	m := make(map[int64]int64, len(ids))
	for _, id := range ids {
		m[id] = id * 7
	}
	return m
}

func TestAckJournalReportsLostAckedID(t *testing.T) {
	j := NewAckJournal()
	j.RecordAcked(1, 7)
	j.RecordAcked(2, 14)

	v := j.Verify(presentSet(1))
	if len(v.Lost) != 1 || v.Lost[0] != 2 {
		t.Fatalf("Lost = %v, want [2]", v.Lost)
	}
	if v.OK() {
		t.Fatal("verification reports OK though an acked id is absent")
	}
	if v.AckedPresent != 1 || v.Present != 1 {
		t.Errorf("AckedPresent = %d, Present = %d, want 1 and 1", v.AckedPresent, v.Present)
	}
}

func TestAckJournalTolerantOfUnknownAndCancelled(t *testing.T) {
	j := NewAckJournal()
	j.RecordUnknown(1, 7)
	j.RecordCancelled(2, 14)
	j.RecordUnknown(3, 21)
	j.RecordCancelled(4, 28)

	// Present and absent are both legal for these, in either combination.
	v := j.Verify(presentSet(1, 2))
	if !v.OK() {
		t.Fatalf("unknown/cancelled ids must never fail verification: %s", v.Summary())
	}
	if len(v.Lost) != 0 || len(v.Unexpected) != 0 {
		t.Errorf("Lost = %v, Unexpected = %v, want both empty", v.Lost, v.Unexpected)
	}
}

func TestAckJournalReportsNeverIssuedID(t *testing.T) {
	j := NewAckJournal()
	j.RecordAcked(1, 7)

	v := j.Verify(presentSet(1, 99))
	if len(v.Unexpected) != 1 || v.Unexpected[0] != 99 {
		t.Fatalf("Unexpected = %v, want [99]", v.Unexpected)
	}
	if len(v.Lost) != 0 {
		t.Errorf("Lost = %v, want empty", v.Lost)
	}
}

func TestAckJournalReportsValueMismatch(t *testing.T) {
	j := NewAckJournal()
	j.RecordAcked(1, 7)
	j.RecordUnknown(2, 14)

	v := j.Verify(map[int64]int64{1: 8, 2: 14})
	if len(v.Mismatched) != 1 {
		t.Fatalf("Mismatched = %v, want one entry for id 1", v.Mismatched)
	}
	got := v.Mismatched[0]
	if got.ID != 1 || got.Want != 7 || got.Got != 8 || got.State != OutcomeAcked {
		t.Errorf("Mismatched[0] = %+v, want id 1 want 7 got 8 acked", got)
	}
}

func TestAckJournalVerifyIDsSkipsValues(t *testing.T) {
	j := NewAckJournal()
	j.RecordAcked(1, 7)

	v := j.VerifyIDs([]int64{1})
	if !v.OK() {
		t.Fatalf("presence-only verification failed: %s", v.Summary())
	}
}

func TestAckJournalAckSurvivesLaterRecords(t *testing.T) {
	j := NewAckJournal()
	j.RecordAcked(1, 7)
	j.RecordUnknown(1, 7)
	j.RecordCancelled(1, 7)

	if o, ok := j.Outcome(1); !ok || o != OutcomeAcked {
		t.Fatalf("Outcome(1) = %v (%v), want acked: a promise to the client cannot be retracted", o, ok)
	}
	if c := j.Counts(); c.Issued != 1 || c.Acked != 1 || c.Unknown != 0 || c.Cancelled != 0 {
		t.Errorf("Counts = %+v, want issued 1 acked 1", c)
	}
	if v := j.Verify(presentSet()); len(v.Lost) != 1 {
		t.Errorf("Lost = %v, want [1]", v.Lost)
	}
}

func TestAckJournalUnknownPromotesToAcked(t *testing.T) {
	j := NewAckJournal()
	j.RecordUnknown(1, 7)
	j.RecordAcked(1, 7)

	if o, _ := j.Outcome(1); o != OutcomeAcked {
		t.Fatalf("Outcome(1) = %v, want acked after a retry succeeded", o)
	}
	if c := j.Counts(); c.Issued != 1 || c.Acked != 1 {
		t.Errorf("Counts = %+v, want issued 1 acked 1", c)
	}
}

func TestAckJournalKeepsFirstIssueOrder(t *testing.T) {
	j := NewAckJournal()
	for _, id := range []int64{5, 3, 9, 3, 5} {
		j.RecordUnknown(id, id*7)
	}
	ids := j.IDs()
	want := []int64{5, 3, 9}
	if len(ids) != len(want) {
		t.Fatalf("IDs = %v, want %v", ids, want)
	}
	for i := range want {
		if ids[i] != want[i] {
			t.Fatalf("IDs = %v, want %v", ids, want)
		}
	}
}

func TestAckJournalAckedIDsAscending(t *testing.T) {
	j := NewAckJournal()
	j.RecordAcked(9, 63)
	j.RecordAcked(2, 14)
	j.RecordUnknown(5, 35)

	ids := j.AckedIDs()
	if len(ids) != 2 || ids[0] != 2 || ids[1] != 9 {
		t.Fatalf("AckedIDs = %v, want [2 9]", ids)
	}
}

func TestAckJournalConcurrentRecording(t *testing.T) {
	j := NewAckJournal()
	const writers, perWriter = 8, 500

	var wg sync.WaitGroup
	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			base := int64(w) * perWriter
			for i := int64(0); i < perWriter; i++ {
				id := base + i
				switch i % 3 {
				case 0:
					j.RecordAcked(id, id*7)
				case 1:
					j.RecordUnknown(id, id*7)
				default:
					j.RecordCancelled(id, id*7)
				}
			}
		}(w)
	}
	// Readers run alongside the writers: -race must see no unguarded access.
	for r := 0; r < 2; r++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 200; i++ {
				j.Counts()
				j.IDs()
			}
		}()
	}
	wg.Wait()

	c := j.Counts()
	if c.Issued != writers*perWriter {
		t.Fatalf("Issued = %d, want %d", c.Issued, writers*perWriter)
	}
	if c.Acked+c.Unknown+c.Cancelled != c.Issued {
		t.Fatalf("outcomes %+v do not sum to issued %d", c, c.Issued)
	}

	present := make(map[int64]int64, c.Issued)
	for _, id := range j.AckedIDs() {
		present[id] = id * 7
	}
	if v := j.Verify(present); !v.OK() {
		t.Fatalf("every acked id present, yet verification failed: %s", v.Summary())
	}
}

func TestClassifyCancelledInsert(t *testing.T) {
	boom := fmt.Errorf("ERROR: canceling statement due to user request")
	cases := []struct {
		name      string
		execErr   error
		delivered bool
		want      cancelVerdict
	}{
		{"acked despite a delivered cancel", nil, true, cancelVerdict{Outcome: OutcomeAcked, AckedDespiteCancel: true}},
		{"acked with the cancel never delivered", nil, false, cancelVerdict{Outcome: OutcomeAcked}},
		{"error after a delivered cancel", boom, true, cancelVerdict{Outcome: OutcomeCancelled}},
		{"error with the cancel never delivered", boom, false, cancelVerdict{Outcome: OutcomeUnknown}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := classifyCancelledInsert(tc.execErr, tc.delivered); got != tc.want {
				t.Fatalf("classifyCancelledInsert = %+v, want %+v", got, tc.want)
			}
		})
	}
}

// An insert the server acked while a cancel was in flight is a promise like any
// other, so its later absence must be a hard failure.
func TestAckedDespiteCancelIsADurabilityObligation(t *testing.T) {
	j := NewAckJournal()
	v := classifyCancelledInsert(nil, true)
	if v.Outcome != OutcomeAcked || !v.AckedDespiteCancel {
		t.Fatalf("verdict = %+v, want acked and flagged", v)
	}
	j.RecordAcked(1, 7)

	got := j.Verify(presentSet())
	if len(got.Lost) != 1 || got.Lost[0] != 1 {
		t.Fatalf("Lost = %v, want [1]: %s", got.Lost, got.Summary())
	}
}

func TestCancelStatsRecordDelivery(t *testing.T) {
	var c cancelStats
	c.recordDelivery(true)
	c.recordDelivery(false)
	c.recordDelivery(false)
	if c.Attempted.Load() != 3 || c.Delivered.Load() != 1 || c.Undelivered.Load() != 2 {
		t.Fatalf("attempted %d delivered %d undelivered %d, want 3 1 2",
			c.Attempted.Load(), c.Delivered.Load(), c.Undelivered.Load())
	}
}

// The run that exposed this reported "0 attempted": a plain 0.1 draw per insert
// enters the cancel leg with poor odds once a fault window leaves a writer with
// only a few completed inserts, so the leg must have a floor.
func TestCancelDeciderAlwaysEntersTheLeg(t *testing.T) {
	seed := core.NewRunSeed(1, true)
	for _, inserts := range []int{1, 3, 200} {
		for w := 0; w < 8; w++ {
			rnd := seed.Fork(fmt.Sprintf("writer-%d", w)).Rand()
			d := newCancelDecider(rnd, 0.1, cancelFloorPerWriter, cancelFloorWithin)
			taken := 0
			for i := 0; i < inserts; i++ {
				if d.want(i) {
					taken++
				}
			}
			if taken == 0 {
				t.Errorf("writer %d took the cancel branch 0 times over %d inserts", w, inserts)
			}
		}
	}
}

// A writer that completes exactly one insert in the whole window must still have
// aimed a cancel at it, and the floor must land inside its window.
func TestCancelDeciderFloorIsReachable(t *testing.T) {
	rnd := core.NewRunSeed(7, true).Fork("writer-0").Rand()
	d := newCancelDecider(rnd, 0.1, cancelFloorPerWriter, cancelFloorWithin)
	if !d.want(0) {
		t.Error("the first insert must take the cancel branch")
	}
	if len(d.forced) != cancelFloorPerWriter {
		t.Errorf("forced inserts = %v, want %d of them", d.forced, cancelFloorPerWriter)
	}
	for i := range d.forced {
		if i < 0 || i >= cancelFloorWithin {
			t.Errorf("forced insert %d sits outside the first %d", i, cancelFloorWithin)
		}
	}
}

// --seed replays exactly, and a zero probability turns the leg off entirely.
func TestCancelDeciderIsSeedDerived(t *testing.T) {
	decisions := func(seedValue int64, probability float64) []bool {
		rnd := core.NewRunSeed(seedValue, true).Fork("chaos-writers").Fork("writer-1").Rand()
		d := newCancelDecider(rnd, probability, cancelFloorPerWriter, cancelFloorWithin)
		out := make([]bool, 40)
		for i := range out {
			out[i] = d.want(i)
		}
		return out
	}
	if !slices.Equal(decisions(42, 0.1), decisions(42, 0.1)) {
		t.Error("the same seed produced different cancel decisions")
	}
	if slices.Equal(decisions(42, 0.1), decisions(43, 0.1)) {
		t.Error("two seeds produced identical cancel decisions")
	}
	for _, taken := range decisions(42, 0) {
		if taken {
			t.Fatal("cancel_probability 0 must never take the cancel branch")
		}
	}
}

func TestFormatIDsTruncates(t *testing.T) {
	if got := formatIDs(nil, 3); got != "none" {
		t.Errorf("formatIDs(nil) = %q, want %q", got, "none")
	}
	if got := formatIDs([]int64{1, 2}, 3); got != "1, 2" {
		t.Errorf("formatIDs = %q, want %q", got, "1, 2")
	}
	got := formatIDs([]int64{1, 2, 3, 4, 5}, 2)
	want := fmt.Sprintf("1, 2, and %d more", 3)
	if got != want {
		t.Errorf("formatIDs = %q, want %q", got, want)
	}
}
