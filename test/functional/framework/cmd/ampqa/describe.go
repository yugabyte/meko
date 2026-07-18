package main

import (
	"fmt"
	"sort"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	"github.com/yugabyte/amp/test/functional/framework/core"
)

// paramGroup is one Params prefix of a test resolved against the embedded
// defaults: every flattened config key the prefix covers, with its default value
// and the `#`-comment doc from default.yaml.
type paramGroup struct {
	Prefix string     `json:"prefix"`
	Keys   []paramKey `json:"keys"`
}

type paramKey struct {
	Key         string `json:"key"`
	Default     string `json:"default"`
	Description string `json:"description,omitempty"`
}

// describeData is the JSON payload for `ampqa describe <test>`.
type describeData struct {
	Test         testNode     `json:"test"`
	Description  string       `json:"description,omitempty"`
	GlobalParams []flagNode   `json:"global_params"`
	GlobalNote   string       `json:"global_note"`
	TestParams   []paramGroup `json:"test_params"`
	TestNote     string       `json:"test_note,omitempty"`
}

func newDescribeCmd() *cobra.Command {
	var (
		suiteRe   string
		testName  string
		testRegex string
		tags      []string
	)
	cmd := &cobra.Command{
		Use:   "describe [test]",
		Short: "Describe tests for discovery: a rich per-suite list, or one test's full parameters",
		Long: `Describe registered tests.

With NO argument: every test (respecting --suite/--test/--tag), grouped by
suite, each with its one-line description and attributes (category, env sharing/
shape, tags, and whether it exposes a run and/or setup phase).

With a TEST NAME: that test's full block — its description, the GLOBAL
parameters (the flags of ` + "`ampqa run`" + `, plus the note that any config key
is overridable via -o key=value), and its TEST-SPECIFIC parameters (for each
config-key prefix the test consumes, the matching keys with their default and
the doc comment from default.yaml).`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			if len(args) == 1 {
				return describeOne(c, args[0])
			}
			testRe, terr := resolveTestFilter(testName, testRegex)
			if terr != nil {
				return &exitError{code: 2, err: terr}
			}
			return describeList(c, suiteRe, testRe, tags)
		},
	}
	cmd.Flags().StringVar(&suiteRe, "suite", "", "suite name regex (list mode)")
	cmd.Flags().StringVar(&testName, "test", "", "exact test name filter (list mode); use --testr for a regex")
	cmd.Flags().StringVar(&testRegex, "testr", "", "test name regex (list mode; mutually exclusive with --test)")
	cmd.Flags().StringArrayVar(&tags, "tag", nil, "require tag, repeatable (list mode)")
	return cmd
}

// describeList prints every selected test grouped by suite, each with its
// description and attributes.
func describeList(c *cobra.Command, suiteRe, testRe string, tags []string) error {
	tests, err := core.Default().Filter(suiteRe, testRe, tags)
	if err != nil {
		return &exitError{code: 2, err: err}
	}
	if jsonOutput {
		rows := make([]map[string]any, 0, len(tests))
		for _, t := range tests {
			rows = append(rows, testRow(t))
		}
		return printEnvelope(c.OutOrStdout(), "ampqa.describe.list", "describe", map[string]any{
			"count": len(tests),
			"tests": rows,
		})
	}

	// Group by suite, preserving the name-sorted order Filter returns.
	bySuite := map[string][]core.Test{}
	var suites []string
	for _, t := range tests {
		if _, seen := bySuite[t.Suite]; !seen {
			suites = append(suites, t.Suite)
		}
		bySuite[t.Suite] = append(bySuite[t.Suite], t)
	}
	sort.Strings(suites)

	out := c.OutOrStdout()
	for _, suite := range suites {
		group := bySuite[suite]
		fmt.Fprintf(out, "%s (%d test%s)\n", suite, len(group), plural(len(group)))
		tw := tabwriter.NewWriter(out, 0, 2, 2, ' ', 0)
		for _, t := range group {
			fmt.Fprintf(tw, "  %s\t%s\n", t.Name, orDash(t.Description))
			fmt.Fprintf(tw, "  \t%s\n", attrLine(t))
		}
		_ = tw.Flush()
		fmt.Fprintln(out)
	}
	fmt.Fprintf(out, "%d test%s\n", len(tests), plural(len(tests)))
	return nil
}

// describeOne prints the full parameter block for a single test.
func describeOne(c *cobra.Command, name string) error {
	t, ok := core.Default().Get(name)
	if !ok {
		return &exitError{code: 3, err: fmt.Errorf("test %q not found", name)}
	}
	global := runFlags(c.Root())
	const globalNote = "Any config key is also overridable via -o key=value (typed prefixes: int:/float:/bool:/list:/json_text:/json_file:)."
	groups := resolveParams(t)

	if jsonOutput {
		data := describeData{
			Test:         toTestNode(t),
			Description:  t.Description,
			GlobalParams: global,
			GlobalNote:   globalNote,
			TestParams:   groups,
		}
		if len(groups) == 0 {
			data.TestNote = "no test-specific parameters (uses global config only)"
		}
		return printEnvelope(c.OutOrStdout(), "ampqa.describe.test", "describe", data)
	}

	out := c.OutOrStdout()
	fmt.Fprintf(out, "%s\n", t.Name)
	if t.Description != "" {
		fmt.Fprintf(out, "  %s\n", t.Description)
	}
	fmt.Fprintln(out)

	fmt.Fprintln(out, "Attributes:")
	atw := tabwriter.NewWriter(out, 0, 2, 2, ' ', 0)
	fmt.Fprintf(atw, "  suite\t%s\n", t.Suite)
	fmt.Fprintf(atw, "  category\t%s\n", t.Category)
	fmt.Fprintf(atw, "  sharing\t%s\n", t.Env.Sharing)
	fmt.Fprintf(atw, "  shape\t%s\n", t.Env.Shape)
	if len(t.Tags) > 0 {
		fmt.Fprintf(atw, "  tags\t%s\n", strings.Join(t.Tags, ", "))
	}
	if t.OwningTeam != "" {
		fmt.Fprintf(atw, "  owning team\t%s\n", t.OwningTeam)
	}
	fmt.Fprintf(atw, "  phases\t%s\n", phases(t))
	if t.Versions.MinYB != "" || t.Versions.MaxYB != "" {
		fmt.Fprintf(atw, "  yb versions\t%s\n", versionRange(t.Versions.MinYB, t.Versions.MaxYB))
	}
	_ = atw.Flush()
	fmt.Fprintln(out)

	fmt.Fprintln(out, "Global parameters (flags of `ampqa run`):")
	gtw := tabwriter.NewWriter(out, 0, 2, 2, ' ', 0)
	for _, f := range global {
		fmt.Fprintf(gtw, "  --%s\t%s\t%s\t%s\n", f.Name, f.Type, orDash(f.Default), f.Usage)
	}
	_ = gtw.Flush()
	fmt.Fprintf(out, "  %s\n\n", globalNote)

	fmt.Fprintln(out, "Test-specific parameters:")
	if len(groups) == 0 {
		fmt.Fprintln(out, "  (none — uses global config only)")
		return nil
	}
	for _, g := range groups {
		fmt.Fprintf(out, "  %s\n", g.Prefix)
		if len(g.Keys) == 0 {
			fmt.Fprintln(out, "    (no matching config keys)")
			continue
		}
		ptw := tabwriter.NewWriter(out, 0, 2, 2, ' ', 0)
		for _, k := range g.Keys {
			fmt.Fprintf(ptw, "    %s\t%s\t%s\n", k.Key, orDash(k.Default), docFirstLine(k.Description))
		}
		_ = ptw.Flush()
	}
	return nil
}

// resolveParams maps each of the test's Params prefixes to the matching
// flattened config keys (default + comment doc) from the embedded defaults.
func resolveParams(t core.Test) []paramGroup {
	if len(t.Params) == 0 {
		return nil
	}
	cfg, err := core.LoadConfig(nil, nil, nil)
	if err != nil {
		return nil
	}
	flat := map[string]any{}
	flattenConfig("", cfg.Resolved(), flat)
	docs := parseConfigDocs()

	groups := make([]paramGroup, 0, len(t.Params))
	for _, prefix := range t.Params {
		var keys []string
		for k := range flat {
			if k == prefix || strings.HasPrefix(k, prefix+".") {
				keys = append(keys, k)
			}
		}
		sort.Strings(keys)
		pk := make([]paramKey, 0, len(keys))
		for _, k := range keys {
			pk = append(pk, paramKey{
				Key:         k,
				Default:     fmt.Sprint(flat[k]),
				Description: docs[k],
			})
		}
		groups = append(groups, paramGroup{Prefix: prefix, Keys: pk})
	}
	return groups
}

// runFlags returns the local flags of the `ampqa run` subcommand — the global
// parameters every test run accepts.
func runFlags(root *cobra.Command) []flagNode {
	var out []flagNode
	for _, sub := range root.Commands() {
		if sub.Name() != "run" {
			continue
		}
		sub.Flags().VisitAll(func(f *pflag.Flag) {
			if f.Hidden {
				return
			}
			out = append(out, flagNode{
				Name:      f.Name,
				Shorthand: f.Shorthand,
				Type:      f.Value.Type(),
				Default:   f.DefValue,
				Usage:     f.Usage,
			})
		})
		break
	}
	return out
}

func toTestNode(t core.Test) testNode {
	return testNode{
		Name:       t.Name,
		Suite:      t.Suite,
		Category:   t.Category.String(),
		Tags:       t.Tags,
		OwningTeam: t.OwningTeam,
		Sharing:    t.Env.Sharing.String(),
		Shape:      t.Env.Shape.String(),
		HasRun:     t.Run != nil,
		HasSetup:   t.Setup != nil,
		MinYB:      t.Versions.MinYB,
		MaxYB:      t.Versions.MaxYB,
	}
}

// attrLine is the one-line attribute summary under a test in list mode.
func attrLine(t core.Test) string {
	parts := []string{
		t.Category.String(),
		t.Env.Sharing.String(),
		t.Env.Shape.String(),
		phases(t),
	}
	line := strings.Join(parts, " · ")
	if len(t.Tags) > 0 {
		line += "  tags: " + strings.Join(t.Tags, ", ")
	}
	if t.OwningTeam != "" {
		line += "  team: " + t.OwningTeam
	}
	return line
}

// phases reports which lifecycle phases a test exposes.
func phases(t core.Test) string {
	switch {
	case t.Run != nil && t.Setup != nil:
		return "setup+run"
	case t.Setup != nil:
		return "setup-only"
	case t.Run != nil:
		return "run"
	default:
		return "none"
	}
}

func versionRange(min, max string) string {
	switch {
	case min != "" && max != "":
		return min + " – " + max
	case min != "":
		return ">= " + min
	default:
		return "<= " + max
	}
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}
