// Package presets registers the developer-facing SETUP-ONLY suite: a menu of
// ready-made standing environments a developer stands up with `ampqa setup` and
// keeps for manual testing. Each entry has NO assertions and NO Run body — it
// only exposes a Setup that reuses a functional provisioner to stand up a common
// backing + amp configuration, register it, and print access. Blank-imported by
// cmd/ampqa.
//
// Usage: `ampqa setup -t presets/yb_k8s` → a ready environment; inspect with
// `ampqa env ls`; tear down with `ampqa env rm <id>`. Like the functional
// suite's amp-on-backing tests, presets self-provision real infrastructure, so
// they SKIP cleanly unless functional.enabled and the shape's config are
// present (StandUp gates).
package presets

import (
	"github.com/yugabyte/yb-amp/test/functional/framework/core"
	"github.com/yugabyte/yb-amp/test/functional/suites/features"
	"github.com/yugabyte/yb-amp/test/functional/suites/perf"
)

// preset registers one setup-only test: Setup stands up the given shape via the
// shared functional provisioner; there is deliberately no Run (no assertions).
func preset(name, shape, description string, params []string) {
	core.Register(core.Test{
		Name:        "presets/" + name,
		Suite:       "presets",
		Category:    core.LongRunning,
		Env:         core.EnvSpec{Sharing: core.Persistent, Shape: core.Universe},
		Description: description,
		Params:      params,
		Setup: func(t *core.T) (*core.SetupResult, error) {
			return features.StandUp(t, shape)
		},
	})
}

func init() {
	preset("yb_k8s", features.ShapeYBK8s,
		"Setup-only: stand up a YugabyteDB-in-k8s backing co-located with amp and leave it running.",
		[]string{"functional", "env.ampstack", "apps.ybk8s"})
	preset("yba_k8s", features.ShapeYBA,
		"Setup-only: stand up an owned YBA + universe (provider_type drives cloud/k8s) and leave it running.",
		[]string{"functional", "env.ampstack", "apps.ybastack", "apps.yba"})
	preset("yugabyted_gce", features.ShapeYugabyted,
		"Setup-only: stand up yugabyted on cloud VMs (cloud = env.vm.cloud) and leave it running.",
		[]string{"functional", "env.ampstack", "env.vm", "env.awsvm", "apps.cloud.aws"})
	preset("raw_aws", features.ShapeRaw,
		"Setup-only: stand up a raw master + tserver cluster on cloud VMs and leave it running.",
		[]string{"functional", "env.ampstack", "env.vm", "env.awsvm", "apps.cloud.aws"})

	// pgbench_client stands up a YB-in-k8s backing + a long-lived client pod
	// carrying YB's pgbench (ysql_bench), loads the schema, and leaves it up for
	// a developer to drive load by hand. Setup lives in the perf suite (it reuses
	// the perf/pgbench delivery glue), so it is registered directly rather than
	// through preset() (which hardcodes features.StandUp).
	core.Register(core.Test{
		Name:        "presets/pgbench_client",
		Suite:       "presets",
		Category:    core.LongRunning,
		Env:         core.EnvSpec{Sharing: core.Persistent, Shape: core.Universe},
		Description: "Setup-only: stand up a YB-in-k8s backing + long-lived pgbench client pod (schema loaded) and leave it running.",
		Params:      []string{"perf", "apps.ybk8s"},
		Setup:       perf.StandUpPgbenchClient,
	})
}
