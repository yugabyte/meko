package foundation

import (
	"context"
	"fmt"
	"strings"
)

// applyCRDs installs the operator's CustomResourceDefinitions.
//
// Server-side apply, not create: the CRD bundle is large and shared, and a
// second foundation on the same cluster re-applies the same objects. A plain
// create would 409 and a plain replace would fight whoever applied it first;
// apply merges field-by-field and is idempotent, which is what a converging
// deployer wants.
//
// The CRDs are CLUSTER-SCOPED and shared by every foundation on the cluster, so
// this is a once-per-cluster step that happens to run on every converge. That
// also means Down leaves them alone: deleting them would delete every other
// foundation's universe with them.
func (d *Deployer) applyCRDs(ctx context.Context) error {
	source := d.crdSource()
	d.log("applying operator CRDs from %s", source)
	res, err := d.kubectlCluster(ctx, nil, "apply", "--server-side", "-f", source)
	if err != nil {
		return err
	}
	if res.ExitCode != 0 {
		hint := ""
		if !strings.HasPrefix(source, "http") {
			hint = " (crdPath is a local file; check the path)"
		} else {
			hint = " (no cluster egress to raw.githubusercontent.com? set crdPath to a local copy)"
		}
		return fmt.Errorf("apply CRDs from %s%s: %s", source, hint, strings.TrimSpace(res.Stderr))
	}
	return nil
}

// crdSource is the local override when set, else the pinned URL.
func (d *Deployer) crdSource() string {
	if d.spec.CRDPath != "" {
		return d.spec.CRDPath
	}
	return CRDURL(d.spec.ChartVersion)
}
