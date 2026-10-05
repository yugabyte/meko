package mekostack

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/yugabyte/meko/meko/test/functional/framework/apps"
	"github.com/yugabyte/meko/meko/test/functional/framework/diag"
	"github.com/yugabyte/meko/meko/test/functional/framework/exec/k8s"
	"github.com/yugabyte/meko/meko/test/functional/framework/kubemeta"
)

const logTail = 5000

// Collect hands the stack's evidence to into: events, every pod's logs current
// and previous, a describe of each pod that is not ready, the init containers by
// name (a pod still in Init: has no log kubectl will print for the rest), and the
// values with their secrets masked. Best effort throughout.
func (a *App) Collect(ctx context.Context, into apps.ArtifactSink) error {
	diag.CollectK8sDiagnostics(ctx, a.kube, a.cfg.Namespace, into, a.logf)
	a.collectInitLogs(ctx, into)
	if b, err := a.llmCalls(ctx); err == nil {
		_ = into.Add("Stack llmmock calls.json", "json", b)
	}
	a.mu.Lock()
	values := a.values
	a.mu.Unlock()
	if values != nil {
		if b, err := MarshalValues(MaskedValues(values)); err == nil {
			_ = into.Add("Stack values.yaml", "yaml", b)
		}
	}
	return nil
}

func (a *App) collectInitLogs(ctx context.Context, into apps.ArtifactSink) {
	for _, st := range initContainers {
		target := "deployment/" + st.deployment
		if out, err := a.kube.Logs(ctx, target, st.container, logTail); err == nil && strings.TrimSpace(out) != "" {
			_ = into.Add("Stack logs/init/"+st.deployment+"/"+st.container+".log", "log", []byte(out))
		}
		if out, err := a.kube.LogsPrevious(ctx, target, st.container, logTail); err == nil && strings.TrimSpace(out) != "" {
			_ = into.Add("Stack logs/init/"+st.deployment+"/"+st.container+".previous.log", "log", []byte(out))
		}
	}
}

func NewFileSink(dir string) apps.ArtifactSink { return fileSink{dir} }

type fileSink struct{ dir string }

func (s fileSink) Add(name, _ string, content []byte) error {
	clean := filepath.Clean("/" + name)
	path := filepath.Join(s.dir, clean)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, content, 0o644)
}

// failureReport records diagnostics to DiagDir and returns the short summary an
// error message carries: what is not ready, why, and where the rest went.
func (a *App) failureReport(ctx context.Context) string {
	dctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Minute)
	defer cancel()
	var b strings.Builder
	if states, err := a.kube.PodStates(dctx); err == nil {
		for _, st := range states {
			if st.Phase == "Running" && st.Ready {
				continue
			}
			fmt.Fprintf(&b, "  pod %s: %s ready=%v restarts=%d", st.Name, st.Phase, st.Ready, st.Restarts)
			if st.Unschedulable != "" {
				fmt.Fprintf(&b, " unschedulable: %s", st.Unschedulable)
			} else if why := a.kube.PodTrouble(dctx, st.Name); why != "" {
				fmt.Fprintf(&b, " %s", why)
			}
			b.WriteString("\n")
		}
	}
	for _, st := range initContainers {
		if out, err := a.kube.Logs(dctx, "deployment/"+st.deployment, st.container, 5); err == nil && strings.TrimSpace(out) != "" {
			fmt.Fprintf(&b, "  %s/%s last lines: %s\n", st.deployment, st.container, strings.ReplaceAll(strings.TrimSpace(out), "\n", " | "))
		}
	}
	if a.cfg.DiagDir != "" {
		dir := filepath.Join(a.cfg.DiagDir, a.cfg.Namespace)
		_ = a.Collect(dctx, fileSink{dir})
		fmt.Fprintf(&b, "  diagnostics: %s", dir)
	}
	if b.Len() == 0 {
		return "  (no pod state could be read)"
	}
	return strings.TrimRight(b.String(), "\n")
}

// Teardown collects the stack's logs, uninstalls the release and deletes the
// namespace. It refuses a namespace that is not labelled as the framework's.
// The release is gone either way once the namespace is, so an uninstall that
// fails is logged and the delete still runs.
func (a *App) Teardown(ctx context.Context) error {
	a.Close()
	owned, err := a.owns(ctx)
	if err != nil {
		return err
	}
	if !owned {
		return nil
	}
	var sink apps.ArtifactSink
	if a.cfg.DiagDir != "" {
		sink = fileSink{filepath.Join(a.cfg.DiagDir, a.cfg.Namespace)}
	}
	collect := func(ctx context.Context, into apps.ArtifactSink) error {
		if into == nil {
			return nil
		}
		return a.Collect(ctx, into)
	}
	return apps.StopWithCollect(ctx, sink, collect, a.stop)
}

// owns reports whether the namespace exists and carries the framework's labels.
// A missing namespace is not an error: there is nothing left to tear down.
func (a *App) owns(ctx context.Context) (bool, error) {
	labels, exists, err := a.namespaceLabels(ctx, a.cfg.Namespace)
	if err != nil || !exists {
		return false, err
	}
	if labels[kubemeta.ManagedByLabel] != kubemeta.ManagedByValue {
		return false, fmt.Errorf("mekostack: namespace %s is not a mekoqa namespace (no %s=%s label), not touching it",
			a.cfg.Namespace, kubemeta.ManagedByLabel, kubemeta.ManagedByValue)
	}
	return true, nil
}

func (a *App) stop(ctx context.Context) error {
	if a.cfg.Release != "" {
		if _, err := a.helm(ctx, "uninstall", a.cfg.Release, "-n", a.cfg.Namespace, "--timeout", "5m"); err != nil {
			a.log("mekostack: helm uninstall in %s: %v (deleting the namespace anyway)", a.cfg.Namespace, err)
		}
	}
	if err := a.cluster.DeleteNamespace(ctx, a.cfg.Namespace); err != nil {
		return fmt.Errorf("delete namespace %s: %w", a.cfg.Namespace, err)
	}
	return nil
}

// Reap deletes a stack namespace from a process that holds nothing of it. It
// does what Teardown does, minus the log collection.
func Reap(ctx context.Context, kubeContext, kubeconfig, namespace, release string) error {
	run := k8s.KubeconfigRunner(kubeconfig)
	a := New(Config{Context: kubeContext, Namespace: namespace, Release: release}).WithRunner(run).WithLogf(nil)
	owned, err := a.owns(ctx)
	if err != nil {
		return err
	}
	if !owned {
		return nil
	}
	return a.stop(ctx)
}

func (a *App) NamespaceGone(ctx context.Context) (bool, error) {
	_, exists, err := a.namespaceLabels(ctx, a.cfg.Namespace)
	return !exists, err
}
