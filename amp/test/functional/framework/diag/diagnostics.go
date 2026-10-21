// Package diag collects Kubernetes failure diagnostics before a test's namespace
// is torn down. Teardown deletes the namespace and with it the controller,
// endpoint, and backing pods — destroying the logs a root-cause needs. This
// helper gathers that evidence first (best-effort) and hands it to the test's
// ArtifactSink, which (over an AttachBytes-backed sink) persists it to disk so it
// survives the run.
//
// Attachment names are '/'-separated paths — the report frontend renders them as
// a folder tree: "Amp logs/<pod>/<container>.log" for the amp control plane,
// "Universe logs/<pod>/<role>/<container>.log" for a backing YugabyteDB
// namespace, "YBA logs/<pod>/<container>.log" for a YBA control plane.
package diag

import (
	"context"
	"strings"
	"sync"

	"github.com/yugabyte/amp/test/functional/framework/apps"
	"github.com/yugabyte/amp/test/functional/framework/exec/k8s"
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

// endpointPodPrefix marks the controller-deployed endpoint compute pods
// (controller EndpointDeployName). They crash-loop when a deploy goes wrong, so
// they are always describe-worthy even when currently Running/Ready.
const endpointPodPrefix = "yb-ep-"

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
	describeAll  bool   // describe every pod, not just unhealthy + yb-ep-*
	treePrefix   string // where this scheme files cluster-scoped evidence
}

// nodesName / nodeUsageName file the cluster-scoped node evidence in whichever
// tree this scheme owns, using its own prefix — the amp tree separates with a
// space ("Amp events.txt") and the others with a folder ("Universe logs/"), so
// this cannot be derived from the events path.
func (s scheme) nodesName() string     { return s.treePrefix + "nodes.txt" }
func (s scheme) nodeUsageName() string { return s.treePrefix + "node-usage.txt" }

// ampScheme files amp control-plane pods under "Amp ...", but routes any
// co-located backing-YugabyteDB pod (yugabyted / yb-master / yb-tserver, which
// share the amp namespace in ampstack+ybk8s envs) into the "Universe logs/"
// tree so the backing shows up as its own subtree rather than buried under Amp.
func ampScheme() scheme {
	uni := universeScheme()
	return scheme{
		logDir: func(pod string) string {
			if isUniversePod(pod) {
				return uni.logDir(pod)
			}
			return "Amp logs/" + pod
		},
		describeName: func(pod string) string {
			if isUniversePod(pod) {
				return uni.describeName(pod)
			}
			return "Amp describe/" + pod + ".txt"
		},
		events:       "Amp events.txt",
		resourcesDir: "Amp resources",
		treePrefix:   "Amp ",
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

// isUniversePod reports whether a pod is a backing-YugabyteDB node — a full
// yb-master/yb-tserver StatefulSet member or a yugabyted single-node — that may
// be co-located in the amp namespace and belongs under "Universe logs/".
func isUniversePod(pod string) bool {
	return strings.HasPrefix(pod, "yugabyted") || universeRole(pod) != ""
}

// CollectK8sDiagnostics gathers, best-effort, the amp control-plane namespace's
// evidence under the "Amp ..." tree: the event stream ("Amp events.txt"),
// pods/deployments -o wide listings ("Amp resources/"), every pod's current AND
// previous-instance (crash-loop) logs split per container ("Amp
// logs/<pod>/<container>.log"), and a describe of every pod that is not
// Running-and-Ready plus every yb-ep-* endpoint pod ("Amp describe/<pod>.txt").
// A failure to collect ONE item is logged and skipped — collection never aborts,
// so the caller's teardown always proceeds.
func CollectK8sDiagnostics(ctx context.Context, kube Kube, namespace string, sink apps.ArtifactSink, logf func(string, ...any)) {
	collect(ctx, kube, namespace, sink, logf, ampScheme())
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

// CollectPods gathers the current and previous logs and a describe of every pod
// whose name starts with one of prefixes, under "<dir>/<pod>/". It is for a test
// that deletes its own pods before the env's collection runs.
func CollectPods(ctx context.Context, kube Kube, prefixes []string, dir string, sink apps.ArtifactSink, logf func(string, ...any)) {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	states, err := kube.PodStates(ctx)
	if err != nil {
		logf("diagnostics: pod states in %s (best-effort): %v", kube.Namespace(), err)
		return
	}
	for _, st := range states {
		if !hasAnyPrefix(st.Name, prefixes) {
			continue
		}
		base := dir + "/" + st.Name
		if out, err := kube.LogsAll(ctx, st.Name, logTailLines); err != nil {
			logf("diagnostics: logs %s (best-effort): %v", st.Name, err)
		} else {
			for container, blob := range splitByContainer(out) {
				add(sink, logf, base+"/"+container+".log", "log", blob)
			}
		}
		if out, err := kube.LogsAllPrevious(ctx, st.Name, logTailLines); err == nil {
			for container, blob := range splitByContainer(out) {
				add(sink, logf, base+"/"+container+".previous.log", "log", blob)
			}
		}
		if out, err := kube.Describe(ctx, "pod", st.Name); err != nil {
			logf("diagnostics: describe %s (best-effort): %v", st.Name, err)
		} else {
			add(sink, logf, base+"/describe.txt", "txt", out)
		}
	}
}

func hasAnyPrefix(s string, prefixes []string) bool {
	for _, p := range prefixes {
		if strings.HasPrefix(s, p) {
			return true
		}
	}
	return false
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
	// TCP connects timed out — with nothing collected about the nodes, whether
	// that was YB or the cluster could not be settled afterwards.
	//
	// Cluster-scoped, so it is gathered once per collection rather than per
	// namespace, and `top` needs metrics-server — hence best-effort like the rest.
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
		// replicasets alongside deployments: the endpoint compute workload is a
		// Deployment today and a bare ReplicaSet after it moves off the rollout
		// layer, and a missing kind is a quiet best-effort skip either way.
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
	states, err := kube.PodStates(ctx)
	if err != nil {
		logf("diagnostics: pod states in %s (best-effort): %v", namespace, err)
	}
	describe := map[string]bool{}
	for _, st := range states {
		if !sc.describeAll && st.Phase == "Running" && st.Ready &&
			!strings.HasPrefix(st.Name, endpointPodPrefix) && !isUniversePod(st.Name) {
			continue
		}
		describe[st.Name] = true
	}

	// Each call is a round trip from the agent to the API server, and a shared
	// namespace holds tens of pods, so one at a time this took minutes per test.
	var jobs []func()
	for _, pod := range pods {
		dir := sc.logDir(pod)
		jobs = append(jobs, func() {
			if out, err := kube.LogsAll(ctx, pod, logTailLines); err != nil {
				logf("diagnostics: logs %s/%s (best-effort): %v", namespace, pod, err)
			} else {
				for container, blob := range splitByContainer(out) {
					add(sink, logf, dir+"/"+container+".log", "log", blob)
				}
			}
		}, func() {
			// --previous errors when no container of the pod ever restarted, the
			// common case, so it is a quiet skip.
			if out, err := kube.LogsAllPrevious(ctx, pod, logTailLines); err != nil {
				logf("diagnostics: previous logs %s/%s (best-effort, likely no prior instance): %v", namespace, pod, err)
			} else {
				for container, blob := range splitByContainer(out) {
					add(sink, logf, dir+"/"+container+".previous.log", "log", blob)
				}
			}
		})
	}
	for name := range describe {
		jobs = append(jobs, func() {
			if out, err := kube.Describe(ctx, "pod", name); err != nil {
				logf("diagnostics: describe %s/%s (best-effort): %v", namespace, name, err)
			} else {
				add(sink, logf, sc.describeName(name), "txt", out)
			}
		})
	}
	runBounded(jobs, collectParallelism)
}

// collectParallelism bounds the concurrent kubectl calls of one collection.
const collectParallelism = 8

func runBounded(jobs []func(), n int) {
	sem := make(chan struct{}, n)
	var wg sync.WaitGroup
	for _, job := range jobs {
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer func() { <-sem; wg.Done() }()
			job()
		}()
	}
	wg.Wait()
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

var addMu sync.Mutex

// add records one non-empty blob, logging (never propagating) a sink error so a
// single failed Add can't abort the collection.
func add(sink apps.ArtifactSink, logf func(string, ...any), name, kind, content string) {
	if content == "" {
		return
	}
	addMu.Lock()
	defer addMu.Unlock()
	if err := sink.Add(name, kind, []byte(content)); err != nil {
		logf("diagnostics: record %s (best-effort): %v", name, err)
	}
}
