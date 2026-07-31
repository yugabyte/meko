package features

import (
	"context"

	"github.com/yugabyte/amp/test/functional/framework/core"
	"github.com/yugabyte/amp/test/functional/framework/diag"
	"github.com/yugabyte/amp/test/functional/framework/env/reap"
	"github.com/yugabyte/amp/test/functional/framework/exec/k8s"
)

// attachSink records every collected blob on the result AND persists its bytes
// (via T.AttachBytes), so failure diagnostics — and any other collected
// artifact — land on disk under the run's artifacts dir.
type attachSink struct {
	t    *core.T
	logf func(string, ...any)
}

func (s attachSink) Add(name, kind string, content []byte) error {
	s.t.AttachBytes(name, kind, content)
	if s.logf != nil {
		s.logf("collected %s (%s, %d bytes)", name, kind, len(content))
	}
	return nil
}

// collectDiagnostics is the runLifecycle pre-teardown hook: before any cleanup
// deletes the amp namespace, gather the k8s failure evidence (controller +
// endpoint + co-located YB pod logs, events, describe of unhealthy pods) into the
// test's persisting sink. Gated by report.collect_diagnostics (on_failure by
// default) against the run's outcome — runErr is the lifecycle's phase error, and
// a body may also have marked t failed directly. Best-effort throughout: any
// collection error is logged and swallowed so teardown still runs.
func (f *flow) collectDiagnostics(ctx context.Context, runErr error) {
	sink := attachSink{t: f.t, logf: f.logf}
	// The framework log (every step + kubectl/helm command and its output) is
	// attached on every run, independent of the k8s-diagnostics gate below.
	if f.rlog != nil {
		_ = sink.Add("Framework log/framework.log", "log", f.rlog.bytes())
	}
	mode := f.t.Options.GetString("report.collect_diagnostics", core.DefaultCollectDiagnostics)
	failed := runErr != nil || f.t.Failed()
	if !diag.ShouldCollect(mode, failed) {
		return
	}
	var (
		kube diag.Kube
		ns   string
	)
	switch {
	case f.ampApp != nil:
		// Self-provision: the amp stack we deployed owns the namespace.
		kube, ns = f.ampApp.Kube(), f.ampApp.Namespace()
	case f.ampEnv != nil:
		// Attach: collect from the standing control plane's namespace (where the
		// controller and yb-ep-* compute pods live). AmpKube is nil for a
		// direct-URL control plane — no kube to reach.
		if k := f.ampEnv.AmpKube(); k != nil {
			kube, ns = k, k.Namespace()
		}
	}
	if kube == nil {
		f.logf("diagnostics: no amp kube handle (amp never deployed, or direct-URL control plane); skipping amp evidence collection")
	} else {
		diag.CollectK8sDiagnostics(ctx, kube, ns, sink, f.logf)
	}
	f.collectBackingDiagnostics(ctx, sink)
}

// collectBackingDiagnostics captures the backing cluster's k8s evidence via the
// reap plan's namespace handles (recorded by the provisioner): a co-located
// ybk8s namespace or a YBA-created k8s universe namespace lands under
// "Universe logs/", a ybastack YBA control plane under "YBA logs/". VM/SSH
// backings (yugabyted / raw yugabyte on AWS, attached YBA universes) carry no
// k8s handle and are skipped with a note.
// TODO: SSH-based log collection for VM/universe-attached backings.
func (f *flow) collectBackingDiagnostics(ctx context.Context, sink attachSink) {
	if f.reap == nil || len(f.reap.Namespaces) == 0 {
		if f.store != nil {
			f.logf("diagnostics: backing logs NOT collected (VM/external backing — no k8s namespace handle)")
		}
		return
	}
	for _, n := range f.reap.Namespaces {
		kube := k8s.New(k8s.Config{Context: n.KubeContext, Namespace: n.Namespace}).
			WithRunner(k8s.KubeconfigRunner(n.Kubeconfig))
		switch n.Kind {
		case reap.KindYBK8s, reap.KindDiscover:
			diag.CollectUniverseK8sDiagnostics(ctx, kube, n.Namespace, sink, f.logf)
		case reap.KindYBAStack:
			diag.CollectYBADiagnostics(ctx, kube, n.Namespace, sink, f.logf)
		}
	}
	if f.reap.Cloud != nil {
		f.logf("diagnostics: cloud-node backing logs NOT collected (VM backing — no k8s handle)")
	}
}
