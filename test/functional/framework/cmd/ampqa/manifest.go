package main

import (
	"encoding/json"
	"fmt"
	"sort"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	"github.com/yugabyte/amp/test/functional/framework/core"
)

type flagNode struct {
	Name      string `json:"name"`
	Shorthand string `json:"shorthand,omitempty"`
	Type      string `json:"type"`
	Default   string `json:"default,omitempty"`
	Usage     string `json:"usage,omitempty"`
}

type cmdNode struct {
	Name     string     `json:"name"`
	Use      string     `json:"use"`
	Short    string     `json:"short,omitempty"`
	Flags    []flagNode `json:"flags,omitempty"`
	Commands []cmdNode  `json:"commands,omitempty"`
}

// testNode is one registered test, so an agent can discover WHAT it can run.
type testNode struct {
	Name        string   `json:"name"`
	Suite       string   `json:"suite"`
	Category    string   `json:"category"`
	Description string   `json:"description,omitempty"`
	Tags        []string `json:"tags,omitempty"`
	OwningTeam  string   `json:"owning_team,omitempty"`
	Sharing     string   `json:"sharing"`
	Shape       string   `json:"shape"`
	HasRun      bool     `json:"has_run"`   // false → setup-only (run with `run --setup-only`)
	HasSetup    bool     `json:"has_setup"` // true → exposes a Setup phase
	MinYB       string   `json:"min_yb,omitempty"`
	MaxYB       string   `json:"max_yb,omitempty"`
}

// manifest is the full self-discovery surface: the command tree (how to invoke),
// the registered tests/suites/tags (what to run), and the flattened config keys
// with their defaults (what to configure).
type manifest struct {
	Command cmdNode        `json:"command"`
	Suites  []string       `json:"suites"`
	Tags    []string       `json:"tags"`
	Tests   []testNode     `json:"tests"`
	Config  map[string]any `json:"config"`
}

func newManifestCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "manifest",
		Short: "Emit the full command tree, registered tests, and config keys as JSON",
		RunE: func(c *cobra.Command, _ []string) error {
			m, err := buildFullManifest(c.Root())
			if err != nil {
				return &exitError{code: 1, err: err}
			}
			out := c.OutOrStdout()
			if jsonOutput {
				return printEnvelope(out, "ampqa.manifest", "manifest", m)
			}
			enc := json.NewEncoder(out)
			enc.SetIndent("", "  ")
			return enc.Encode(m)
		},
	}
}

// buildFullManifest assembles the command tree plus the registered-test and
// config-key discovery surfaces.
func buildFullManifest(root *cobra.Command) (manifest, error) {
	tests := core.Default().List()
	nodes := make([]testNode, 0, len(tests))
	suiteSet := map[string]struct{}{}
	tagSet := map[string]struct{}{}
	for _, t := range tests {
		nodes = append(nodes, testNode{
			Name:        t.Name,
			Suite:       t.Suite,
			Category:    t.Category.String(),
			Description: t.Description,
			Tags:        t.Tags,
			OwningTeam:  t.OwningTeam,
			Sharing:     t.Env.Sharing.String(),
			Shape:       t.Env.Shape.String(),
			HasRun:      t.Run != nil,
			HasSetup:    t.Setup != nil,
			MinYB:       t.Versions.MinYB,
			MaxYB:       t.Versions.MaxYB,
		})
		suiteSet[t.Suite] = struct{}{}
		for _, tag := range t.Tags {
			tagSet[tag] = struct{}{}
		}
	}

	cfg, err := core.LoadConfig(nil, nil, nil)
	if err != nil {
		return manifest{}, fmt.Errorf("load embedded defaults for manifest: %w", err)
	}
	flat := map[string]any{}
	flattenConfig("", cfg.Resolved(), flat)

	return manifest{
		Command: buildManifest(root),
		Suites:  sortedKeys(suiteSet),
		Tags:    sortedKeys(tagSet),
		Tests:   nodes,
		Config:  flat,
	}, nil
}

// flattenConfig turns the nested default config into dotted-key → default-value
// leaves (env.docker.yb_image → "yugabytedb/yugabyte:latest"), so an agent can
// discover every configurable knob and its default. Empty maps are kept as leaf
// values so a documented-but-empty section (e.g. secrets) still surfaces.
func flattenConfig(prefix string, node map[string]any, out map[string]any) {
	for k, v := range node {
		key := k
		if prefix != "" {
			key = prefix + "." + k
		}
		if child, ok := v.(map[string]any); ok && len(child) > 0 {
			flattenConfig(key, child, out)
			continue
		}
		out[key] = v
	}
}

func sortedKeys(set map[string]struct{}) []string {
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func buildManifest(c *cobra.Command) cmdNode {
	node := cmdNode{Name: c.Name(), Use: c.Use, Short: c.Short}
	c.Flags().VisitAll(func(f *pflag.Flag) {
		node.Flags = append(node.Flags, flagNode{
			Name:      f.Name,
			Shorthand: f.Shorthand,
			Type:      f.Value.Type(),
			Default:   f.DefValue,
			Usage:     f.Usage,
		})
	})
	for _, sub := range c.Commands() {
		if sub.Hidden || sub.Name() == "help" {
			continue
		}
		node.Commands = append(node.Commands, buildManifest(sub))
	}
	return node
}
