package main

import (
	"fmt"
	"sort"
	"strings"

	"github.com/spf13/cobra"

	"github.com/yugabyte/amp/test/functional/framework/core"
)

func newCollectCmd() *cobra.Command {
	var (
		suiteRe   string
		testName  string
		testRegex string
		tags      []string
		team      string
	)
	cmd := &cobra.Command{
		Use:   "collect",
		Short: "List registered tests",
		RunE: func(c *cobra.Command, _ []string) error {
			testRe, terr := resolveTestFilter(testName, testRegex)
			if terr != nil {
				return &exitError{code: 2, err: terr}
			}
			tests, err := core.Default().Filter(suiteRe, testRe, tags)
			if err != nil {
				return &exitError{code: 2, err: err}
			}
			tests = filterByTeam(tests, team)
			if jsonOutput {
				rows := make([]map[string]any, 0, len(tests))
				for _, t := range tests {
					rows = append(rows, testRow(t))
				}
				return printEnvelope(c.OutOrStdout(), "ampqa.tests", "collect", map[string]any{
					"count": len(tests),
					"tests": rows,
				})
			}
			out := c.OutOrStdout()
			// Group by suite (headed), then each test: short name + shape + tags on
			// one line, the description on the next — no key=value soup.
			bySuite := map[string][]core.Test{}
			var order []string
			for _, t := range tests {
				if _, seen := bySuite[t.Suite]; !seen {
					order = append(order, t.Suite)
				}
				bySuite[t.Suite] = append(bySuite[t.Suite], t)
			}
			sort.Strings(order)
			for _, s := range order {
				grp := bySuite[s]
				sort.Slice(grp, func(i, j int) bool { return grp[i].Name < grp[j].Name })
				fmt.Fprintf(out, "\n%s  (%d)\n", s, len(grp))
				for _, t := range grp {
					short := strings.TrimPrefix(t.Name, s+"/")
					line := fmt.Sprintf("  %-30s %-16s", short, t.Env.Shape)
					if len(t.Tags) > 0 {
						line += "  " + strings.Join(t.Tags, ", ")
					}
					if t.OwningTeam != "" {
						line += "  [" + t.OwningTeam + "]"
					}
					fmt.Fprintln(out, strings.TrimRight(line, " "))
					if t.Description != "" {
						fmt.Fprintf(out, "      %s\n", t.Description)
					}
				}
			}
			plural := "tests"
			if len(tests) == 1 {
				plural = "test"
			}
			fmt.Fprintf(out, "\n%d %s\n", len(tests), plural)
			return nil
		},
	}
	cmd.Flags().StringVar(&suiteRe, "suite", "", "suite name regex")
	cmd.Flags().StringVar(&testName, "test", "", "exact test name (e.g. perf/pgbench); use --testr for a regex")
	cmd.Flags().StringVar(&testRegex, "testr", "", "test name regex (mutually exclusive with --test)")
	cmd.Flags().StringArrayVar(&tags, "tag", nil, "require tag (repeatable)")
	cmd.Flags().StringVar(&team, "team", "", "require owning team (e.g. control_plane, compute)")
	return cmd
}

// filterByTeam keeps only tests whose OwningTeam matches team; empty team
// keeps every test.
func filterByTeam(tests []core.Test, team string) []core.Test {
	if team == "" {
		return tests
	}
	out := make([]core.Test, 0, len(tests))
	for _, t := range tests {
		if t.OwningTeam == team {
			out = append(out, t)
		}
	}
	return out
}

func testRow(t core.Test) map[string]any {
	return map[string]any{
		"name":        t.Name,
		"suite":       t.Suite,
		"category":    t.Category.String(),
		"description": t.Description,
		"tags":        t.Tags,
		"owning_team": t.OwningTeam,
		"env": map[string]any{
			"sharing": t.Env.Sharing.String(),
			"shape":   t.Env.Shape.String(),
			"weight":  t.Env.Weight,
		},
	}
}
