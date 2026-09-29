package perf

import (
	"context"
	"hash/fnv"
	"sort"
	"strings"

	"github.com/yugabyte/amp/test/functional/framework/apps/ybk8s"
	"github.com/yugabyte/amp/test/functional/framework/core"
	"github.com/yugabyte/amp/test/functional/framework/exec/k8s"
)

const zoneLabel = "topology.kubernetes.io/zone"

// inZone pins every pod of one benchmark stack to zone on top of the machine
// pin. default-big spans three zones, and a stack whose tservers, endpoint and
// client straddle them pays cross-zone round trips the build did not cause:
// sysbench's two arms ran one product (0.260926.0 vs 0.260928.0 differ only
// in deploy and docs) and the fixed-rate latency still came out 50% apart.
func (c perfConfig) inZone(zone string) perfConfig {
	if zone == "" {
		return c
	}
	sel := zoneLabel + "=" + zone
	if c.ArmNodeSelector != "" {
		sel = c.ArmNodeSelector + "," + sel
	}
	c.ArmNodeSelector = sel
	return c
}

// armZones is the zones the arm node pool has nodes in, sorted, or nil when
// pinning is off or the nodes cannot be read (the stacks then run unpinned).
func armZones(ctx context.Context, t *core.T, cfg perfConfig, logf func(string, ...any)) []string {
	if !t.Options.GetBool("perf.pin_zone", true) {
		return nil
	}
	yb := ybk8s.FromConfig(t.Options)
	kube := k8s.New(k8s.Config{Context: yb.KubeContext})
	if yb.Kubeconfig != "" {
		kube = kube.WithRunner(k8s.KubeconfigRunner(yb.Kubeconfig))
	}
	sel := cfg.ArmNodeSelector
	if sel == "" {
		sel = zoneLabel
	}
	out, err := kube.GetSelector(ctx, "nodes", sel, `{range .items[*]}{.metadata.labels.topology\.kubernetes\.io/zone}{"\n"}{end}`)
	if err != nil {
		logf("perf: WARN could not read the arm pool's zones, stacks run unpinned: %v", err)
		return nil
	}
	seen := map[string]bool{}
	var zones []string
	for _, z := range strings.Fields(out) {
		if !seen[z] {
			seen[z] = true
			zones = append(zones, z)
		}
	}
	sort.Strings(zones)
	return zones
}

// zoneFor spreads stacks over the zones by index, so provision i of both arms
// shares a zone and a pair's delta carries no zone difference.
func zoneFor(zones []string, i int) string {
	if len(zones) == 0 {
		return ""
	}
	return zones[i%len(zones)]
}

// testZoneIndex spreads single-pair tests over the zones by test name, so they
// do not all land in the first one.
func testZoneIndex(name string) int {
	h := fnv.New32a()
	_, _ = h.Write([]byte(name))
	return int(h.Sum32() & 0x7fffffff)
}
