// Package history is the perf tier's run-history store: a JSON file that APPENDS
// one record per perf run so the report can render a TREND of a metric across
// past runs (and versions). It is a sibling concept to the baseline store but a
// distinct one — the baseline keeps a SINGLE reference value per test/metric to
// gate against, while the history keeps EVERY run's aggregate to plot over time.
//
// The store is a flat, append-ordered list; append order is the run order, which
// the trend reads as chronological (no clock-based sort, so it stays
// deterministic). Load/Save touch the filesystem exactly like the baseline
// store: a missing file loads as an empty store, so a first run appends its one
// record and the report simply has nothing to trend yet. The store is
// ENVIRONMENT-SPECIFIC (hardware, cluster size) like the baseline — it is
// gitignored and never committed.
package history

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// Record is one perf run's aggregate, appended to the store. The timestamp is
// supplied by the caller (the store never reads the wall clock itself), so a
// unit test can inject a deterministic stamp.
type Record struct {
	Test         string  `json:"test"`
	Version      string  `json:"version"`
	Timestamp    string  `json:"timestamp"`
	MedianTPS    float64 `json:"median_tps"`
	LatencyAvgMS float64 `json:"latency_avg_ms"`
	LatencyP95MS float64 `json:"latency_p95_ms,omitempty"`
	LatencyP99MS float64 `json:"latency_p99_ms,omitempty"`
	CVTPS        float64 `json:"cv_tps"`
	Seed         int64   `json:"seed"`

	// Result identity — what this run measured and at what load. LoadSignature
	// makes two records comparable (same load); the images/commit say which
	// thing-under-test differed. Self-describing so a trend across records is
	// interpretable without external context.
	YBImage            string `json:"yb_image,omitempty"`
	AmpControllerImage string `json:"amp_controller_image,omitempty"`
	AmpEndpointImage   string `json:"amp_endpoint_image,omitempty"`
	Commit             string `json:"commit,omitempty"`
	LoadSignature      string `json:"load_signature,omitempty"`
}

// File is the on-disk history store: a flat list of records in append order.
type File struct {
	Records []Record `json:"records"`
}

// Empty returns an initialized, empty store.
func Empty() *File { return &File{} }

// Load reads the history store at path. A missing file is not an error: it loads
// as an empty store so a first run can append its record.
func Load(path string) (*File, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return Empty(), nil
		}
		return nil, fmt.Errorf("read history %s: %w", path, err)
	}
	f := Empty()
	if err := json.Unmarshal(data, f); err != nil {
		return nil, fmt.Errorf("parse history %s: %w", path, err)
	}
	return f, nil
}

// Save writes the store to path as indented JSON, creating parent dirs.
func Save(path string, f *File) error {
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("mkdir history dir: %w", err)
		}
	}
	data, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(data, '\n'), 0o644)
}

// Append adds rec to the store at path and writes it back (create-or-append). A
// missing file is created; an existing one is read, appended to, and re-saved,
// so append order stays the run order.
func Append(path string, rec Record) error {
	f, err := Load(path)
	if err != nil {
		return err
	}
	f.Records = append(f.Records, rec)
	return Save(path, f)
}

// ForTest returns the records for test in append (chronological) order. A
// non-empty version filters to that version's runs; "" returns every version.
func (f *File) ForTest(test, version string) []Record {
	var out []Record
	for _, r := range f.Records {
		if r.Test != test {
			continue
		}
		if version != "" && r.Version != version {
			continue
		}
		out = append(out, r)
	}
	return out
}

// Recent returns the most-recent n records (the tail), preserving order. n <= 0
// or a slice already at/below n returns recs unchanged — so a caller can bound
// the trend to the last N runs without special-casing the small-history case.
func Recent(recs []Record, n int) []Record {
	if n <= 0 || len(recs) <= n {
		return recs
	}
	return recs[len(recs)-n:]
}
