package branchgc

import (
	"os"
	"regexp"
	"slices"
	"strconv"
	"testing"
)

// The reaper fans two tables out over a column whose cardinality is owned by
// Rust — `GC_BUCKETS` for `pending_gc`, `SlruKind` for `slru_pages`. Go cannot
// import either, so it hard-codes a copy of each (gcBuckets, slruKinds) and the
// tests here are the only thing that notices when a copy stops matching.
//
// Where each authority lives:
const (
	gcBucketsRustSource = "../../../compute/pageserver/src/page_store/gc.rs"
	// The authority for slru_pages.kind is the map that WRITES it, not the enum.
	// `SlruKind`'s discriminants and the stored values are separate hand-written
	// sequences, free to diverge. It stayed in page_store.rs when GC's storage
	// layer moved to the submodule, so the two authorities are separate files.
	slruKindsRustSource = "../../../compute/pageserver/src/page_store.rs"
)

var gcBucketsDecl = regexp.MustCompile(`(?m)^pub const GC_BUCKETS: i32 = (\d+);`)

// TestGCBucketsMatchesRust fails when `GC_BUCKETS` moves in the pageserver and
// the reaper's copy does not.
//
// Drifting LOW is the dangerous direction and it is SILENT: the reap drains
// buckets 0..gcBuckets-1 of `pending_gc`, finds them empty, reports the branch
// finished and stamps `gc_completed_at` — so the branch leaves the due set with
// rows still in the higher buckets and nothing ever revisits it. No error, no
// log, just rows that are never reclaimed. Drifting high only wastes a few
// empty queries per branch.
//
// This is the last such constant. `pages` and `wals` used to need one each, but
// their `bucket_id` is a leading RANGE column, so one anchored cursor walks every
// bucket and no count is needed. `pending_gc` keeps it because its `bucket_id` is
// inside the HASH key, which admits only equality — see the fan-out in specs.
//
// A missing source file is a FAILURE, not a skip: a guard that quietly stops
// running is the same silent drift it exists to catch.
func TestGCBucketsMatchesRust(t *testing.T) {
	src, err := os.ReadFile(gcBucketsRustSource)
	if err != nil {
		t.Fatalf("read the GC_BUCKETS authority: %v\n"+
			"If gc.rs moved, update gcBucketsRustSource — do not delete this test.", err)
	}
	m := gcBucketsDecl.FindSubmatch(src)
	if m == nil {
		t.Fatalf("no `pub const GC_BUCKETS: i32 = N;` in %s\n"+
			"If the declaration was reshaped, update the pattern — do not delete this test.",
			gcBucketsRustSource)
	}
	want, err := strconv.Atoi(string(m[1]))
	if err != nil {
		t.Fatalf("parse GC_BUCKETS %q: %v", m[1], err)
	}
	if gcBuckets != want {
		t.Errorf("gcBuckets = %d, but %s says GC_BUCKETS = %d.\n"+
			"The reaper fans out over buckets 0..gcBuckets-1 of amp_storage.pending_gc; "+
			"too low and a reaped branch keeps the rows in the buckets above it.",
			gcBuckets, gcBucketsRustSource, want)
	}
}

// slruKindsDecl matches the body of `slru_kind_to_i16`, and slruKindArm each of
// its `SlruKind::Name => N,` arms.
var (
	slruKindsDecl = regexp.MustCompile(`(?s)fn slru_kind_to_i16\(kind: SlruKind\) -> i16 \{(.*?)\n\}`)
	slruKindArm   = regexp.MustCompile(`=>\s*(\d+)\s*,`)
)

// TestSlruKindsMatchesRust fails when the SLRU kinds the pageserver stores stop
// being exactly 0..slruKinds-1.
//
// Same silent failure as gcBuckets: the reaper fans `slru_pages` out over
// `kind` 0..slruKinds-1, so a kind outside that range is never selected, never
// deleted, and the branch is still stamped `gc_completed_at` — nothing revisits
// it. Postgres has more SLRUs than the three the pageserver models (subtrans,
// commit_ts, serial, notify), so a fourth kind is a live possibility.
//
// The authority is `slru_kind_to_i16`, not `enum SlruKind`. The enum's
// discriminants never reach the database: every write of the column goes through
// this map, whose arms are a hand-written second sequence. Counting enum variants
// would pass while the map assigned, say, 0/1/2/10 — and kind 10 would be
// stranded on every reap. Asserting the VALUES also covers the count, so this
// checks the property the fan-out actually rests on rather than a proxy for it.
//
// A missing source file or unmatched function is a FAILURE, not a skip: a guard
// that quietly stops running is the same silent drift it exists to catch.
func TestSlruKindsMatchesRust(t *testing.T) {
	src, err := os.ReadFile(slruKindsRustSource)
	if err != nil {
		t.Fatalf("read the slru_kind_to_i16 authority: %v\n"+
			"If page_store.rs moved, update slruKindsRustSource — do not delete this test.", err)
	}
	m := slruKindsDecl.FindSubmatch(src)
	if m == nil {
		t.Fatalf("no `fn slru_kind_to_i16(kind: SlruKind) -> i16 { ... }` in %s\n"+
			"If it was renamed or reshaped, update the pattern — do not delete this test.",
			slruKindsRustSource)
	}
	var got []int
	for _, arm := range slruKindArm.FindAllSubmatch(m[1], -1) {
		v, err := strconv.Atoi(string(arm[1]))
		if err != nil {
			t.Fatalf("unparsable kind value %q in slru_kind_to_i16", arm[1])
		}
		got = append(got, v)
	}
	if len(got) == 0 {
		t.Fatalf("no `SlruKind::Name => N,` arms in slru_kind_to_i16 (%s)\n"+
			"If the arms were reshaped, update the pattern — do not delete this test.",
			slruKindsRustSource)
	}
	slices.Sort(got)
	want := make([]int, slruKinds)
	for i := range want {
		want[i] = i
	}
	if !slices.Equal(got, want) {
		t.Errorf("slru_pages.kind values are %v, but the reaper fans out over %v "+
			"(slruKinds = %d) in %s.\n"+
			"The fan-out binds kind = 0..slruKinds-1 by equality, so any stored value "+
			"outside that range is never deleted and the branch is still marked reclaimed. "+
			"Adjust slruKinds if a kind was added; if the values are no longer contiguous "+
			"from 0, the fan-out must enumerate them instead of counting.",
			got, want, slruKinds, slruKindsRustSource)
	}
}
