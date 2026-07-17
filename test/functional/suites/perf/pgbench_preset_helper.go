package perf

import (
	"context"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/yugabyte/yb-amp/test/functional/framework/apps/pg"
	"github.com/yugabyte/yb-amp/test/functional/framework/apps/pgbench"
	"github.com/yugabyte/yb-amp/test/functional/framework/apps/ybk8s"
	"github.com/yugabyte/yb-amp/test/functional/framework/artifact"
	"github.com/yugabyte/yb-amp/test/functional/framework/core"
	"github.com/yugabyte/yb-amp/test/functional/framework/env/reap"
)

// pgbenchClientShape is the standing-env shape StandUpPgbenchClient records.
const pgbenchClientShape = "pgbench_client"

// StandUpPgbenchClient is the setup-only preset behind `ampqa setup -t
// presets/pgbench_client`: it stands up a co-located YugabyteDB-in-k8s backing
// and a long-lived client pod carrying YB's pgbench (ysql_bench, wrapped as
// `pgbench`), loads the pgbench schema, and LEAVES them running for a developer
// to drive load by hand. It reuses the same backing + client-pod + pgbench
// delivery glue as the perf/pgbench test (no amp control plane — a pgbench
// load-test env drives YSQL directly). The SetupResult carries ready-to-run
// `kubectl exec … pgbench …` commands + connection info, and a serialized reap
// plan so `ampqa env rm` reclaims the namespace (cascading the client pod) in a
// separate process. It self-skips unless perf.enabled + a k8s cluster + a chart
// version are set (the perf gate).
func StandUpPgbenchClient(t *core.T) (*core.SetupResult, error) {
	cfg := perfFromConfig(t.Options)
	params := pgbenchParamsFromConfig(t.Options)
	if reason, ok := perfGate(t.Options, cfg); !ok {
		t.Skip(reason)
		return nil, nil
	}
	if reason, ok := gateNoDoubleProvision(t.Env); !ok {
		t.Skip(reason)
		return nil, nil
	}

	logf := t.Masker().Logf(log.Printf)
	sink := attachSink{t: t, logf: logf}
	td := &teardownStack{}
	teardown := func(ctx context.Context) error { return td.run(ctx, logf) }
	fail := func(err error) (*core.SetupResult, error) {
		tctx, cancel := context.WithTimeout(context.Background(), perfTeardownBudget)
		defer cancel()
		_ = teardown(tctx)
		return nil, err
	}

	info, kube, err := deployYBBacking(t.Context, t, cfg.RF, td, logf)
	if err != nil {
		return fail(err)
	}
	podName := "ampqa-pgbench-" + t.ResourceSuffix()
	podExec, err := startPerfClientPod(t.Context, kube, sink, td, podName, cfg.ClientImage, cfg.ClientPullSecret)
	if err != nil {
		return fail(err)
	}
	if err := waitBackingYSQL(t.Context, kube, info, cfg.RF, cfg.ReadyTimeout); err != nil {
		return fail(err)
	}
	spec, err := pgbenchArtifact(t.Options)
	if err != nil {
		return fail(err)
	}
	if _, err := deliverPgbench(t.Context, &artifact.Deployer{}, podExec, spec, cfg.RemoteDir); err != nil {
		return fail(err)
	}

	host, port := ybk8s.FirstHostPort(info.InClusterHostPorts)
	conn := pg.ConnInfo{Host: host, Port: port, User: info.User, Database: info.User, SSLMode: "disable"}
	// Load the pgbench schema so the standing env is immediately loadable.
	if err := pgbench.Init(t.Context, podExec, pgbench.InitOptions{Conn: conn, Scale: params.Scale}); err != nil {
		return fail(fmt.Errorf("pgbench schema init on standing env: %w", err))
	}

	k8sCfg := ybk8s.FromConfig(t.Options)
	plan := &reap.Plan{Namespaces: []reap.NSReap{{
		Kind:        reap.KindYBK8s,
		KubeContext: k8sCfg.KubeContext,
		Kubeconfig:  k8sCfg.Kubeconfig,
		Namespace:   info.Namespace,
	}}}
	planJSON, err := plan.JSON()
	if err != nil {
		return fail(fmt.Errorf("serialize reap plan: %w", err))
	}

	env := &core.StandingEnv{
		Name:     "preset-pgbench-client-" + t.ResourceSuffix(),
		Shape:    pgbenchClientShape,
		Provider: "functional",
		Ref:      t.ResourceSuffix(),
		Info:     pgbenchStandingInfo(k8sCfg.KubeContext, info.Namespace, podName, conn, params, planJSON),
	}
	return &core.SetupResult{Env: env, Teardown: teardown}, nil
}

// pgbenchStandingInfo is the standing-env Info map: the connection path, the
// ready-to-run `kubectl exec` init + run commands a developer copies to drive
// pgbench by hand, and the serialized reap plan `ampqa env rm` reaps by.
func pgbenchStandingInfo(kubeContext, namespace, pod string, conn pg.ConnInfo, params pgbenchParams, planJSON string) map[string]string {
	info := map[string]string{
		"shape":          pgbenchClientShape,
		"namespace":      namespace,
		"client_pod":     pod,
		"ysql_hostports": conn.Addr(),
		"pgbench_conn":   conn.DSN(),
		"pgbench_init":   kubectlExecPgbench(kubeContext, namespace, pod, pgbenchInitArgs(conn, params)),
		"pgbench_cmd":    kubectlExecPgbench(kubeContext, namespace, pod, pgbenchRunArgs(conn, params)),
	}
	if planJSON != "" {
		info["reap_plan"] = planJSON
	}
	return info
}

// kubectlExecPgbench renders a copy-paste `kubectl exec` that runs the in-pod
// `pgbench` wrapper with the given arguments. kubeContext is included only when
// set (else the ambient context).
func kubectlExecPgbench(kubeContext, namespace, pod, pgbenchArgs string) string {
	var b strings.Builder
	b.WriteString("kubectl ")
	if kubeContext != "" {
		fmt.Fprintf(&b, "--context %s ", kubeContext)
	}
	fmt.Fprintf(&b, "-n %s exec -it %s -- pgbench %s", namespace, pod, pgbenchArgs)
	return b.String()
}

// pgbenchInitArgs / pgbenchRunArgs are the pgbench argument strings for the
// surfaced dev commands (init loads the schema at scale; run drives the load).
func pgbenchInitArgs(conn pg.ConnInfo, params pgbenchParams) string {
	return strings.TrimSpace(fmt.Sprintf("-i -s %d %s", params.Scale, connFlags(conn)))
}

func pgbenchRunArgs(conn pg.ConnInfo, params pgbenchParams) string {
	secs := int(params.Duration.Round(time.Second) / time.Second)
	return strings.TrimSpace(fmt.Sprintf("-c %d -j %d -T %d %s", params.Clients, params.Jobs, secs, connFlags(conn)))
}

// connFlags renders the -h/-p/-U/-d flags for the surfaced pgbench commands. The
// backing needs no password, so none is emitted.
func connFlags(conn pg.ConnInfo) string {
	var parts []string
	if conn.Host != "" {
		parts = append(parts, "-h "+conn.Host)
	}
	if conn.Port != 0 {
		parts = append(parts, fmt.Sprintf("-p %d", conn.Port))
	}
	if conn.User != "" {
		parts = append(parts, "-U "+conn.User)
	}
	if conn.Database != "" {
		parts = append(parts, "-d "+conn.Database)
	}
	return strings.Join(parts, " ")
}
