package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/spf13/cobra"

	"github.com/yugabyte/yb-amp/test/functional/framework/core"
	rhtml "github.com/yugabyte/yb-amp/test/functional/framework/report/html"
)

// newReportCmd builds `ampqa report`: generate a self-contained HTML report
// from a completed run's output, on the spot, without re-running anything.
func newReportCmd() *cobra.Command {
	var (
		from string
		out  string
	)
	cmd := &cobra.Command{
		Use:   "report",
		Short: "Generate a self-contained HTML report from a completed run's output",
		Long: `Generate a self-contained HTML report from a completed run's output.

--from accepts either a jsonfile run-out-dir (baseDir/<runid> as written by
"ampqa run --out-dir", containing run.json + one <test>.json per result) or a
single JSON file holding a plain array of results (e.g. hand-assembled or
extracted from one run's per-test files). The report is written to --out and
never touches the network or re-executes any test.`,
		RunE: func(c *cobra.Command, _ []string) error {
			if from == "" {
				return &exitError{code: 2, err: fmt.Errorf("--from is required")}
			}
			if out == "" {
				return &exitError{code: 2, err: fmt.Errorf("-o/--out is required")}
			}
			data, err := loadRunData(from)
			if err != nil {
				return &exitError{code: 1, err: fmt.Errorf("load %s: %w", from, err)}
			}
			rendered, err := rhtml.Render(data)
			if err != nil {
				return &exitError{code: 1, err: err}
			}
			if dir := filepath.Dir(out); dir != "" && dir != "." {
				if err := os.MkdirAll(dir, 0o755); err != nil {
					return &exitError{code: 1, err: err}
				}
			}
			if err := os.WriteFile(out, []byte(rendered), 0o644); err != nil {
				return &exitError{code: 1, err: err}
			}

			counts := map[string]int{}
			for _, r := range data.Results {
				if r != nil {
					counts[string(r.Status)]++
				}
			}
			if jsonOutput {
				return printEnvelope(c.OutOrStdout(), "ampqa.report", "report", map[string]any{
					"from":          from,
					"out":           out,
					"total":         len(data.Results),
					"counts":        counts,
					"bytes_written": len(rendered),
				})
			}
			fmt.Fprintf(c.OutOrStdout(), "wrote %s (%d test(s), %d bytes)\n", out, len(data.Results), len(rendered))
			return nil
		},
	}
	cmd.Flags().StringVar(&from, "from", "", "run-out-dir (jsonfile sink output) or a results.json file")
	cmd.Flags().StringVarP(&out, "out", "o", "", "output HTML file path")
	return cmd
}

// runSummaryFile is the subset of jsonfile's run.json this command reads for
// run-level metadata (seed/parallel/time span); per-test detail comes from the
// individual result files, not from this summary.
type runSummaryFile struct {
	Seed      int64     `json:"seed"`
	Parallel  int       `json:"parallel"`
	StartedAt time.Time `json:"started_at"`
	EndedAt   time.Time `json:"ended_at"`
}

// loadRunData resolves --from into RunData: a jsonfile run-out-dir or a single
// JSON file containing a results array (or one bare result object).
func loadRunData(from string) (rhtml.RunData, error) {
	info, err := os.Stat(from)
	if err != nil {
		return rhtml.RunData{}, err
	}
	var data rhtml.RunData
	if info.IsDir() {
		data, err = loadRunDataFromDir(from)
	} else {
		data, err = loadRunDataFromFile(from)
	}
	if err != nil {
		return rhtml.RunData{}, err
	}
	backfillDescriptions(data.Results)
	return data, nil
}

// backfillDescriptions fills each result's Description from the registered test
// of the same name when the persisted result predates the field (older run
// output), so the report shows what each test checks either way.
func backfillDescriptions(results []*core.Result) {
	tests, err := core.Default().Filter("", "", nil)
	if err != nil {
		return
	}
	desc := map[string]string{}
	for _, t := range tests {
		desc[t.Name] = t.Description
	}
	for _, r := range results {
		if r != nil && r.Description == "" {
			r.Description = desc[r.TestName]
		}
	}
}

func loadRunDataFromDir(dir string) (rhtml.RunData, error) {
	var data rhtml.RunData

	if raw, err := os.ReadFile(filepath.Join(dir, "run.json")); err == nil {
		var sum runSummaryFile
		if err := json.Unmarshal(raw, &sum); err != nil {
			return data, fmt.Errorf("parse run.json: %w", err)
		}
		data.Seed, data.Parallel, data.StartedAt, data.EndedAt = sum.Seed, sum.Parallel, sum.StartedAt, sum.EndedAt
	} else if !os.IsNotExist(err) {
		return data, err
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		return data, err
	}
	for _, e := range entries {
		if e.IsDir() || e.Name() == "run.json" || filepath.Ext(e.Name()) != ".json" {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			return data, fmt.Errorf("read %s: %w", e.Name(), err)
		}
		var r core.Result
		if err := json.Unmarshal(raw, &r); err != nil {
			return data, fmt.Errorf("parse %s: %w", e.Name(), err)
		}
		data.Results = append(data.Results, &r)
	}
	return data, nil
}

func loadRunDataFromFile(path string) (rhtml.RunData, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return rhtml.RunData{}, err
	}
	var results []*core.Result
	if err := json.Unmarshal(raw, &results); err == nil {
		return rhtml.RunData{Results: results}, nil
	}
	var single core.Result
	if err := json.Unmarshal(raw, &single); err != nil {
		return rhtml.RunData{}, fmt.Errorf("%s is neither a JSON array of results nor a single result object: %w", path, err)
	}
	return rhtml.RunData{Results: []*core.Result{&single}}, nil
}
