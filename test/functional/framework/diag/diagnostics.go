// Package diag collects Kubernetes failure diagnostics before a test's namespace
// is torn down. Teardown deletes the namespace and with it the controller,
// endpoint, and backing pods — destroying the logs a root-cause needs. This
// helper gathers that evidence first (best-effort) and hands it to the test's
// ArtifactSink, which (over an AttachBytes-backed sink) persists it to disk so it
// survives the run.
package diag

import (
	"context"

	"github.com/yugabyte/yb-amp/test/functional/framework/apps"
	"github.com/yugabyte/yb-amp/test/functional/framework/exec/k8s"
)

// logTailLines bounds how many trailing log lines are pulled per pod, so a chatty
// pod can't produce an unbounded blob.
const logTailLines = 5000

// Kube is the k8s surface the collector needs. *k8s.Kubectl satisfies it; a
// scripted fake satisfies it in unit tests (no cluster).
type Kube interface {
	Namespace() string
	ListPods(ctx context.Context) ([]string, error)
	PodStates(ctx context.Context) ([]k8s.PodState, error)
	Logs(ctx context.Context, target, container string, tail int) (string, error)
	LogsPrevious(ctx context.Context, target, container string, tail int) (string, error)
	Describe(ctx context.Context, kind, name string) (string, error)
	Events(ctx context.Context) (string, error)
}

// Collection mode values for the report.collect_diagnostics knob.
const (
	ModeOnFailure = "on_failure"
	ModeAlways    = "always"
	ModeNever     = "never"
)

// ShouldCollect decides whether to collect for a given mode and test outcome.
// Unknown modes fall back to on_failure (the default) rather than silently
// collecting nothing.
func ShouldCollect(mode string, failed bool) bool {
	switch mode {
	case ModeNever:
		return false
	case ModeAlways:
		return true
	default: // on_failure and any unrecognized value
		return failed
	}
}

// CollectK8sDiagnostics gathers, best-effort, the k8s evidence to root-cause a
// failure before namespace teardown destroys it: the namespace event stream,
// every pod's current AND previous-instance (crash-loop) logs, and a describe of
// every pod that is not Running-and-Ready. Each blob is Add-ed to sink under a
// namespace-prefixed name. A failure to collect ONE item is logged and skipped —
// collection never aborts, so the caller's teardown always proceeds.
func CollectK8sDiagnostics(ctx context.Context, kube Kube, namespace string, sink apps.ArtifactSink, logf func(string, ...any)) {
	if logf == nil {
		logf = func(string, ...any) {}
	}

	if events, err := kube.Events(ctx); err != nil {
		logf("diagnostics: get events in %s (best-effort): %v", namespace, err)
	} else {
		add(sink, logf, namespace+"-events.txt", "txt", events)
	}

	pods, err := kube.ListPods(ctx)
	if err != nil {
		logf("diagnostics: list pods in %s (best-effort): %v", namespace, err)
	}
	for _, pod := range pods {
		if out, err := kube.Logs(ctx, pod, "", logTailLines); err != nil {
			logf("diagnostics: logs %s/%s (best-effort): %v", namespace, pod, err)
		} else {
			add(sink, logf, namespace+"-"+pod+".log", "log", out)
		}
		// --previous errors when the pod never restarted (no prior instance);
		// that is the common case, so treat the error as a quiet skip.
		if out, err := kube.LogsPrevious(ctx, pod, "", logTailLines); err != nil {
			logf("diagnostics: previous logs %s/%s (best-effort, likely no prior instance): %v", namespace, pod, err)
		} else {
			add(sink, logf, namespace+"-"+pod+".previous.log", "log", out)
		}
	}

	states, err := kube.PodStates(ctx)
	if err != nil {
		logf("diagnostics: pod states in %s (best-effort): %v", namespace, err)
	}
	for _, st := range states {
		if st.Phase == "Running" && st.Ready {
			continue
		}
		if out, err := kube.Describe(ctx, "pod", st.Name); err != nil {
			logf("diagnostics: describe %s/%s (best-effort): %v", namespace, st.Name, err)
		} else {
			add(sink, logf, namespace+"-"+st.Name+".describe.txt", "txt", out)
		}
	}
}

// add records one non-empty blob, logging (never propagating) a sink error so a
// single failed Add can't abort the collection.
func add(sink apps.ArtifactSink, logf func(string, ...any), name, kind, content string) {
	if content == "" {
		return
	}
	if err := sink.Add(name, kind, []byte(content)); err != nil {
		logf("diagnostics: record %s (best-effort): %v", name, err)
	}
}
