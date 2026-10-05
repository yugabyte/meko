// Package diag collects Kubernetes failure diagnostics before a test's namespace
// is torn down. Teardown deletes the namespace and with it the stack and backing
// pods, destroying the logs a root-cause needs. This
// helper gathers that evidence first (best-effort) and hands it to the test's
// ArtifactSink, which (over an AttachBytes-backed sink) persists it to disk so it
// survives the run.
//
// Attachment names are '/'-separated paths, the report frontend renders them as
// a folder tree: "Stack logs/<pod>/<container>.log" for a meko stack,
// "Universe logs/<pod>/<role>/<container>.log" for a backing YugabyteDB
// namespace, "YBA logs/<pod>/<container>.log" for a YBA control plane.
package diag

import (
	"context"
	"fmt"
	"strings"

	"github.com/yugabyte/meko/meko/test/functional/framework/apps"
	"github.com/yugabyte/meko/meko/test/functional/framework/exec/k8s"
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
	LogsAll(ctx context.Context, target string, tail int) (string, error)
	LogsAllPrevious(ctx context.Context, target string, tail int) (string, error)
	Describe(ctx context.Context, kind, name string) (string, error)
	Events(ctx context.Context) (string, error)
	Wide(ctx context.Context, kind string) (string, error)
	Nodes(ctx context.Context) (string, error)
	NodeUsage(ctx context.Context) (string, error)
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

// scheme maps one namespace's evidence onto the report's attachment tree.
type scheme struct {
	logDir       func(pod string) string // folder for the pod's per-container logs
	describeName func(pod string) string
	events       string
	resourcesDir string // "" → no wide listings
	describeAll  bool   // describe every pod, not just unhealthy
	treePrefix   string // where this scheme files cluster-scoped evidence
}

// nodesName / nodeUsageName file the cluster-scoped node evidence in whichever
// tree this scheme owns, using its own prefix, the stack tree separates with a
// space ("Stack events.txt") and the others with a folder ("Universe logs/"), so
// this cannot be derived from the events path.
func (s scheme) nodesName() string     { return s.treePrefix + "nodes.txt" }
func (s scheme) nodeUsageName() string { return s.treePrefix + "node-usage.txt" }

// stackScheme files stack pods under "Stack ...", but routes any co-located
// backing-YugabyteDB pod (yugabyted / yb-master / yb-tserver, which share the
// stack namespace in a ybk8s env) into the "Universe logs/" tree so the backing
// shows up as its own subtree rather than buried under Stack.
func stackScheme() scheme {
	uni := universeScheme()
	return scheme{
		logDir: func(pod string) string {
			if isUniversePod(pod) {
				return uni.logDir(pod)
			}
			return "Stack logs/" + pod
		},
		describeName: func(pod string) string {
			if isUniversePod(pod) {
				return uni.describeName(pod)
			}
			return "Stack describe/" + pod + ".txt"
		},
		events:       "Stack events.txt",
		resourcesDir: "Stack resources",
		treePrefix:   "Stack ",
	}
}

func universeScheme() scheme {
	return scheme{
		logDir: func(pod string) string {
			if r := universeRole(pod); r != "" {
				return "Universe logs/" + pod + "/" + r
			}
			return "Universe logs/" + pod
		},
		describeName: func(pod string) string {
			if r := universeRole(pod); r != "" {
				return "Universe logs/" + pod + "/" + r + ".describe.txt"
			}
			return "Universe logs/" + pod + "/describe.txt"
		},
		events:      "Universe logs/events.txt",
		describeAll: true,
		treePrefix:  "Universe logs/",
	}
}

func ybaScheme() scheme {
	return scheme{
		logDir:       func(pod string) string { return "YBA logs/" + pod },
		describeName: func(pod string) string { return "YBA logs/" + pod + "/describe.txt" },
		events:       "YBA logs/events.txt",
		describeAll:  true,
		treePrefix:   "YBA logs/",
	}
}

// universeRole classifies a backing-namespace pod by its StatefulSet name.
// yugabyted is a single all-in-one node (master+tserver in one process), so it
// carries no role subdir.
func universeRole(pod string) string {
	switch {
	case strings.HasPrefix(pod, "yb-master"):
		return "master"
	case strings.HasPrefix(pod, "yb-tserver"):
		return "tserver"
	}
	return ""
}

// isUniversePod reports whether a pod is a backing-YugabyteDB node, a full
// yb-master/yb-tserver StatefulSet member or a yugabyted single-node, that may
// be co-located in the stack namespace and belongs under "Universe logs/".
func isUniversePod(pod string) bool {
	return strings.HasPrefix(pod, "yugabyted") || universeRole(pod) != ""
}

const yugabytedContainer = "yugabyted"

// A yugabyted pod's stdout is only the launcher's. The master and tserver write
// their own logs under the base dir, and a stalled database shows only there.
var yugabytedServerLogs = map[string]string{
	"master.INFO":  "/data/logs/master/yb-master.INFO",
	"tserver.INFO": "/data/logs/tserver/yb-tserver.INFO",
}

type podShell interface {
	ForPod(pod, container string) *k8s.Kubectl
}

func collectServerLogs(ctx context.Context, kube Kube, pod, dir string, sink apps.ArtifactSink, logf func(string, ...any)) {
	sh, ok := kube.(podShell)
	if !ok {
		return
	}
	for name, path := range yugabytedServerLogs {
		res, err := sh.ForPod(pod, yugabytedContainer).Run(ctx, fmt.Sprintf("tail -n %d %s", logTailLines*4, path))
		if err != nil || res.ExitCode != 0 {
			logf("diagnostics: %s of %s (best-effort): %v %s", name, pod, err, strings.TrimSpace(res.Stderr))
			continue
		}
		add(sink, logf, dir+"/"+name+".log", "log", res.Stdout)
	}
}

// CollectK8sDiagnostics gathers, best-effort, the stack namespace's
// evidence under the "Stack ..." tree: the event stream ("Stack events.txt"),
// pods/deployments -o wide listings ("Stack resources/"), every pod's current AND
// previous-instance (crash-loop) logs split per container ("Stack
// logs/<pod>/<container>.log"), and a describe of every pod that is not
// Running-and-Ready ("Stack describe/<pod>.txt").
// A failure to collect ONE item is logged and skipped, collection never aborts,
// so the caller's teardown always proceeds.
func CollectK8sDiagnostics(ctx context.Context, kube Kube, namespace string, sink apps.ArtifactSink, logf func(string, ...any)) {
	collect(ctx, kube, namespace, sink, logf, stackScheme())
}

// CollectUniverseK8sDiagnostics gathers a backing YugabyteDB namespace's
// evidence under "Universe logs/": every pod's current + previous per-container
// logs ("Universe logs/<pod>/<master|tserver>/<container>.log"), a describe of
// every pod ("Universe logs/<pod>/<role>.describe.txt"), and the namespace
// events. Best-effort throughout, same as CollectK8sDiagnostics.
func CollectUniverseK8sDiagnostics(ctx context.Context, kube Kube, namespace string, sink apps.ArtifactSink, logf func(string, ...any)) {
	collect(ctx, kube, namespace, sink, logf, universeScheme())
}

// CollectYBADiagnostics gathers a YBA control-plane namespace's evidence under
// "YBA logs/": every pod's current + previous per-container logs (the yugaware
// pod's yugaware/prometheus/postgres containers land as separate files), a
// describe of every pod, and the namespace events. Best-effort throughout.
func CollectYBADiagnostics(ctx context.Context, kube Kube, namespace string, sink apps.ArtifactSink, logf func(string, ...any)) {
	collect(ctx, kube, namespace, sink, logf, ybaScheme())
}

func collect(ctx context.Context, kube Kube, namespace string, sink apps.ArtifactSink, logf func(string, ...any), sc scheme) {
	if logf == nil {
		logf = func(string, ...any) {}
	}

	if events, err := kube.Events(ctx); err != nil {
		logf("diagnostics: get events in %s (best-effort): %v", namespace, err)
	} else {
		add(sink, logf, sc.events, "txt", events)
	}

	// Node-level state. A pod can be Running and Ready with zero restarts while
	// its node is starved, and from inside the pod that is indistinguishable from
	// the product hanging: in run 6a26ee65 all three tservers of an RF=3 backing
	// stopped making progress in the same second, stayed Ready, and the client's
	// TCP connects timed out, with nothing collected about the nodes, whether
	// that was YB or the cluster could not be settled afterwards.
	//
	// Cluster-scoped, so it is gathered once per collection rather than per
	// namespace, and `top` needs metrics-server, hence best-effort like the rest.
	if out, err := kube.Nodes(ctx); err != nil {
		logf("diagnostics: describe nodes (best-effort): %v", err)
	} else {
		add(sink, logf, sc.nodesName(), "txt", out)
	}
	if out, err := kube.NodeUsage(ctx); err != nil {
		logf("diagnostics: top nodes (best-effort, needs metrics-server): %v", err)
	} else {
		add(sink, logf, sc.nodeUsageName(), "txt", out)
	}

	if sc.resourcesDir != "" {
		// A missing kind is a quiet best-effort skip.
		for _, kind := range []string{"pods", "deployments", "replicasets"} {
			if out, err := kube.Wide(ctx, kind); err != nil {
				logf("diagnostics: get %s -o wide in %s (best-effort): %v", kind, namespace, err)
			} else {
				add(sink, logf, sc.resourcesDir+"/"+kind+".txt", "txt", out)
			}
		}
	}

	pods, err := kube.ListPods(ctx)
	if err != nil {
		logf("diagnostics: list pods in %s (best-effort): %v", namespace, err)
	}
	for _, pod := range pods {
		dir := sc.logDir(pod)
		if out, err := kube.LogsAll(ctx, pod, logTailLines); err != nil {
			logf("diagnostics: logs %s/%s (best-effort): %v", namespace, pod, err)
		} else {
			byContainer := splitByContainer(out)
			for container, blob := range byContainer {
				add(sink, logf, dir+"/"+container+".log", "log", blob)
			}
			if _, ok := byContainer[yugabytedContainer]; ok {
				collectServerLogs(ctx, kube, pod, dir, sink, logf)
			}
		}
		// --previous errors when no container of the pod ever restarted (no
		// prior instance); that is the common case, so treat it as a quiet skip.
		if out, err := kube.LogsAllPrevious(ctx, pod, logTailLines); err != nil {
			if !strings.Contains(err.Error(), "previous terminated container") {
				logf("diagnostics: previous logs %s/%s (best-effort): %v", namespace, pod, err)
			}
		} else {
			for container, blob := range splitByContainer(out) {
				add(sink, logf, dir+"/"+container+".previous.log", "log", blob)
			}
		}
	}

	states, err := kube.PodStates(ctx)
	if err != nil {
		logf("diagnostics: pod states in %s (best-effort): %v", namespace, err)
	}
	for _, st := range states {
		if !sc.describeAll && st.Phase == "Running" && st.Ready && !isUniversePod(st.Name) {
			continue
		}
		if out, err := kube.Describe(ctx, "pod", st.Name); err != nil {
			logf("diagnostics: describe %s/%s (best-effort): %v", namespace, st.Name, err)
		} else {
			add(sink, logf, sc.describeName(st.Name), "txt", out)
		}
	}
}

// splitByContainer splits a `kubectl logs --all-containers --prefix` blob
// ("[pod/<pod>/<container>] line") into per-container logs. Unprefixed output
// (single-stream fallback) lands under "pod".
func splitByContainer(out string) map[string]string {
	parts := map[string]*strings.Builder{}
	last := "pod"
	for _, line := range strings.Split(out, "\n") {
		name, rest := last, line
		if strings.HasPrefix(line, "[pod/") {
			if i := strings.Index(line, "]"); i > 0 {
				if segs := strings.Split(line[1:i], "/"); len(segs) == 3 && segs[2] != "" {
					name = segs[2]
					rest = strings.TrimPrefix(line[i+1:], " ")
				}
			}
		}
		b := parts[name]
		if b == nil {
			b = &strings.Builder{}
			parts[name] = b
		}
		b.WriteString(rest)
		b.WriteByte('\n')
		last = name
	}
	blobs := make(map[string]string, len(parts))
	for container, b := range parts {
		if s := strings.TrimRight(b.String(), "\n"); s != "" {
			blobs[container] = s
		}
	}
	return blobs
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
