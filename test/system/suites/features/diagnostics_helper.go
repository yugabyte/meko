package features

import (
	"context"

	"github.com/yugabyte/yb-amp/test/system/framework/core"
	"github.com/yugabyte/yb-amp/test/system/framework/diag"
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
	mode := f.t.Options.GetString("report.collect_diagnostics", core.DefaultCollectDiagnostics)
	failed := runErr != nil || f.t.Failed()
	if !diag.ShouldCollect(mode, failed) {
		return
	}
	if f.ampApp == nil {
		// Amp never deployed (e.g. backing provisioning failed); nothing amp-side
		// to collect and no namespace client to reach.
		return
	}
	diag.CollectK8sDiagnostics(ctx, f.ampApp.Kube(), f.ampApp.Namespace(), attachSink{t: f.t, logf: f.logf}, f.logf)
}
