package baseline

import (
	"github.com/yugabyte/meko/meko/test/functional/framework/core"
)

// Resolve answers the question a perf gate has to answer before it can compare
// anything: WHICH stored baseline is this run allowed to be judged against.
//
// Two green paths used to open here. A run on a brand new product version found
// nothing under its own version key and self-established a baseline, so a
// regression landing exactly at a version bump was never gated. And a run whose
// load signature differed from the stored one was recorded ungated, so a run
// that changed shape reported green whatever the numbers did. Resolve closes
// both: it searches every version bucket, keeps only the baselines whose LOAD is
// comparable, and picks one deterministically.

// LoadMatch says how comparable a stored baseline's load is to the run's.
type LoadMatch string

const (
	// LoadExact is the same load_signature: every tracked load knob is identical.
	LoadExact LoadMatch = "exact"
	// LoadShape is the same load SHAPE (rows, clients, warehouses, RF, script,
	// ...) measured on a different harness cadence (iterations, warmup length,
	// measurement duration). The numbers are rates over the same work, so the
	// comparison still means something, but the cadence difference is real noise
	// and the caller must WARN.
	LoadShape LoadMatch = "shape"
)

// Selection names the stored baseline a run gates against, so the verdict can
// say which numbers it came from.
type Selection struct {
	Version  string // the version bucket gated against; "" is the version-agnostic one
	Meta     Meta
	Match    LoadMatch
	Fallback bool // the bucket is not the run's own version
}

// bucket ranks, nearest first.
const (
	rankOwn = iota
	rankAgnostic
	rankPreceding
	rankFollowing
)

// Resolve picks the stored baseline a run at version gates against. It prefers
// the stronger LOAD match first (an identical load beats a same-shape one, the
// store's comparability rule is the load signature, not the version), then the
// nearest version bucket: the run's own, the version-agnostic "", the nearest
// preceding version, the nearest following one. A bucket whose version cannot be
// ordered against the run's is skipped rather than guessed at. It reports false
// when no bucket holds a load-comparable baseline for test.
func Resolve(f *File, version, test string, want Meta) (Selection, bool) {
	var best candidate
	found := false
	for v := range f.Baselines {
		m, ok := f.metaIn(v, test)
		if !ok {
			continue
		}
		match, ok := classifyLoad(want, m)
		if !ok {
			continue
		}
		rank, ok := bucketRank(version, v)
		if !ok {
			continue
		}
		c := candidate{version: v, meta: m, match: match, rank: rank}
		if !found || c.betterThan(best) {
			best, found = c, true
		}
	}
	if !found {
		return Selection{}, false
	}
	return Selection{Version: best.version, Meta: best.meta, Match: best.match, Fallback: best.version != version}, true
}

// candidate is one version bucket that holds a load-comparable baseline.
type candidate struct {
	version string
	meta    Meta
	match   LoadMatch
	rank    int
}

// betterThan reports whether a is the better pick than b: the stronger load
// match first, then the nearer version bucket, with the version string breaking
// every remaining tie so the pick never depends on map iteration order.
func (a candidate) betterThan(b candidate) bool {
	if ar, br := matchRank(a.match), matchRank(b.match); ar != br {
		return ar < br
	}
	if a.rank != b.rank {
		return a.rank < b.rank
	}
	if c := compareVersions(a.version, b.version); c != 0 {
		// Nearest preceding is the greatest version below the run's; nearest
		// following is the smallest above it.
		if a.rank == rankPreceding {
			return c > 0
		}
		if a.rank == rankFollowing {
			return c < 0
		}
	}
	return a.version < b.version
}

func matchRank(m LoadMatch) int {
	if m == LoadExact {
		return 0
	}
	return 1
}

// classifyLoad decides whether a stored baseline's load is comparable to the
// run's at all. An empty stored load_shape (an entry written before shapes were
// recorded) can only ever match exactly.
func classifyLoad(want, stored Meta) (LoadMatch, bool) {
	if stored.LoadSignature == want.LoadSignature {
		return LoadExact, true
	}
	if want.LoadShape != "" && stored.LoadShape == want.LoadShape {
		return LoadShape, true
	}
	return "", false
}

// bucketRank places a stored version bucket relative to the run's version.
// Anything that is neither the run's own bucket nor the version-agnostic one and
// cannot be parsed as a version is unorderable, so it is refused.
func bucketRank(runVersion, v string) (int, bool) {
	switch {
	case v == runVersion:
		return rankOwn, true
	case v == "":
		return rankAgnostic, true
	}
	if !core.LooksLikeVersion(runVersion) || !core.LooksLikeVersion(v) {
		return 0, false
	}
	c, err := core.Compare(v, runVersion)
	if err != nil {
		return 0, false
	}
	if c < 0 {
		return rankPreceding, true
	}
	return rankFollowing, true
}

// compareVersions orders two version strings, treating an unparseable one as
// equal so the caller falls back to its string tie-break.
func compareVersions(a, b string) int {
	c, err := core.Compare(a, b)
	if err != nil {
		return 0
	}
	return c
}
