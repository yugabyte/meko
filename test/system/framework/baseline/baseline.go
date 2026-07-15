// Package baseline is the perf tier's Go-native baseline store and regression
// gate. It models the stress framework's baseline concept — a per-test,
// per-metric reference value a run compares against — as a small JSON file and
// a pure Compare over core.Datapoint slices.
//
// The store is version-keyed: an entry lives under a product version (so a
// newer build compares against its own line) with a version-agnostic ""
// fallback used when no version-specific entry exists. Compare never touches
// the filesystem; Load/Save do, and a missing file loads as an empty store (a
// first run then emits datapoints and WARNs rather than failing).
package baseline

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/yugabyte/yb-amp/test/system/framework/core"
)

// File is the on-disk baseline store: version → test → datapoint → Entry.
type File struct {
	Baselines map[string]map[string]map[string]Entry `json:"baselines"`
}

// Entry is one recorded baseline datapoint. The identity fields (load_signature,
// yb_image, commit) stamp WHAT this baseline was established from, so a later run
// can decide whether it is comparable (same load) before gating against it.
type Entry struct {
	Value     float64 `json:"value"`
	Unit      string  `json:"unit,omitempty"`
	Direction string  `json:"direction,omitempty"`
	UpdatedAt string  `json:"updated_at,omitempty"`

	LoadSignature string `json:"load_signature,omitempty"`
	YBImage       string `json:"yb_image,omitempty"`
	Commit        string `json:"commit,omitempty"`
}

// Meta is the run identity stamped onto a test's baseline entries: the
// load_signature that gates comparability plus the image/commit under test (for
// the mismatch WARN). A run is comparable to a stored baseline iff their
// LoadSignature matches.
type Meta struct {
	LoadSignature string
	YBImage       string
	Commit        string
}

// Thresholds is the allowed regression, per direction: a higher-is-better
// datapoint may drop up to DropPct, a lower-is-better one may rise up to
// RisePct, before Compare marks it regressed.
type Thresholds struct {
	DropPct float64 // higher-is-better tolerance (throughput may fall this far)
	RisePct float64 // lower-is-better tolerance (latency may rise this far)
}

// Report is the outcome of comparing a test's datapoints against the store.
type Report struct {
	// Datapoints is the input list with each element's Baseline comparison
	// filled in where a baseline existed (nil where none did).
	Datapoints []core.Datapoint
	// HadBaseline is true if at least one datapoint matched a stored baseline.
	HadBaseline bool
	// Regressed is true if any datapoint crossed its threshold in the worse
	// direction.
	Regressed bool
}

// Empty returns an initialized, empty store.
func Empty() *File {
	return &File{Baselines: map[string]map[string]map[string]Entry{}}
}

// Load reads the baseline store at path. A missing file is not an error: it
// loads as an empty store so a first run can establish a baseline.
func Load(path string) (*File, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return Empty(), nil
		}
		return nil, fmt.Errorf("read baseline %s: %w", path, err)
	}
	f := Empty()
	if err := json.Unmarshal(data, f); err != nil {
		return nil, fmt.Errorf("parse baseline %s: %w", path, err)
	}
	if f.Baselines == nil {
		f.Baselines = map[string]map[string]map[string]Entry{}
	}
	return f, nil
}

// Save writes the store to path as indented JSON, creating parent dirs.
func Save(path string, f *File) error {
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("mkdir baseline dir: %w", err)
		}
	}
	data, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(data, '\n'), 0o644)
}

// lookup resolves a baseline for (version, test, name), preferring the exact
// version and falling back to the version-agnostic "" bucket.
func (f *File) lookup(version, test, name string) (Entry, bool) {
	buckets := []string{version}
	if version != "" {
		buckets = append(buckets, "")
	}
	for _, v := range buckets {
		byTest, ok := f.Baselines[v]
		if !ok {
			continue
		}
		if e, ok := byTest[test][name]; ok {
			return e, true
		}
	}
	return Entry{}, false
}

// StoredMeta returns the run identity stamped on test's baseline entries at
// version (with the version-agnostic "" fallback lookup preferred exactly as
// Compare's), and whether any entry existed. All of a test's entries share one
// run identity, so the first entry found carries it.
func (f *File) StoredMeta(version, test string) (Meta, bool) {
	buckets := []string{version}
	if version != "" {
		buckets = append(buckets, "")
	}
	for _, v := range buckets {
		byTest, ok := f.Baselines[v]
		if !ok {
			continue
		}
		byName, ok := byTest[test]
		if !ok || len(byName) == 0 {
			continue
		}
		for _, e := range byName {
			return Meta{LoadSignature: e.LoadSignature, YBImage: e.YBImage, Commit: e.Commit}, true
		}
	}
	return Meta{}, false
}

// StampMeta writes the run identity onto every entry for test at version. Call
// it right after Update establishes/refreshes a baseline, so the stored
// baseline records the load it was measured at (for a later comparability gate).
func (f *File) StampMeta(version, test string, m Meta) {
	byTest, ok := f.Baselines[version]
	if !ok {
		return
	}
	byName, ok := byTest[test]
	if !ok {
		return
	}
	for name, e := range byName {
		e.LoadSignature = m.LoadSignature
		e.YBImage = m.YBImage
		e.Commit = m.Commit
		byName[name] = e
	}
}

// Compare gates dps for test at version against the store. It returns a copy of
// dps with each Baseline comparison filled where a baseline existed; it never
// mutates the input slice or the store.
func Compare(f *File, version, test string, dps []core.Datapoint, th Thresholds) Report {
	out := make([]core.Datapoint, len(dps))
	copy(out, dps)
	rep := Report{Datapoints: out}
	for i := range out {
		dp := &out[i]
		e, ok := f.lookup(version, test, dp.Name)
		if !ok || e.Value == 0 {
			continue
		}
		rep.HadBaseline = true
		delta := (dp.Value - e.Value) / e.Value * 100
		threshold := th.DropPct
		if dp.Direction == core.LowerIsBetter {
			threshold = th.RisePct
		}
		regressed := false
		switch dp.Direction {
		case core.LowerIsBetter:
			regressed = delta > threshold // rose too far
		default: // HigherIsBetter (and unset)
			regressed = -delta > threshold // dropped too far
		}
		dp.Baseline = &core.BaselineComparison{
			Value:     e.Value,
			DeltaPct:  delta,
			Threshold: threshold,
			Regressed: regressed,
		}
		if regressed {
			rep.Regressed = true
		}
	}
	return rep
}

// Update writes dps' values back into the store under (version, test),
// refreshing UpdatedAt. Use it for --update-baseline. now is the timestamp to
// stamp (pass time.Now()).
func (f *File) Update(version, test string, dps []core.Datapoint, now time.Time) {
	if f.Baselines == nil {
		f.Baselines = map[string]map[string]map[string]Entry{}
	}
	byTest := f.Baselines[version]
	if byTest == nil {
		byTest = map[string]map[string]Entry{}
		f.Baselines[version] = byTest
	}
	byName := byTest[test]
	if byName == nil {
		byName = map[string]Entry{}
		byTest[test] = byName
	}
	stamp := now.UTC().Format(time.RFC3339)
	for _, dp := range dps {
		byName[dp.Name] = Entry{
			Value:     dp.Value,
			Unit:      dp.Unit,
			Direction: string(dp.Direction),
			UpdatedAt: stamp,
		}
	}
}
