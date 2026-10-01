// SQLancer's logic-bug oracles run against an amp endpoint, one oracle at a
// time, on a quiet stack.
//
// Every other test in this tier checks that amp keeps what it acknowledged.
// This one checks that what amp ANSWERS is right: each oracle derives the same
// result two ways (a partitioned query against the whole, a plan with an
// optimisation against one without) and reports a disagreement. amp's compute
// is stock PostgreSQL over amp storage, so a finding is either a storage-layer
// read returning the wrong page or a PostgreSQL bug; the reproducer log says
// which by whether it replays on vanilla PostgreSQL.
//
// Nothing faults the stack. SQLancer counts any exception a thread throws as a
// bug, a dropped connection included, so a fault here would only manufacture
// findings.
package stress

import (
	"context"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/yugabyte/amp/test/functional/framework/apps/clientpod"
	"github.com/yugabyte/amp/test/functional/framework/apps/sqlancer"
	"github.com/yugabyte/amp/test/functional/framework/core"
	"github.com/yugabyte/amp/test/functional/framework/exec/k8s"
)

func init() {
	core.Register(core.Test{
		Name:        "stress/sqlancer",
		Suite:       "stress",
		Category:    core.Stress,
		Env:         core.EnvSpec{Sharing: core.Exclusive, Shape: core.DedicatedStack},
		Tags:        []string{"stress"},
		OwningTeam:  core.TeamControlPlane,
		Description: "Install SQLancer from its master-yb release (yugabyte/sqlancer, sqlancer_2.0.0-yb) and run its logic-bug oracles through the amp provider (PostgreSQL-compatible mode), several at once, one endpoint each, on a stack nothing faults; every oracle must finish without writing a reproducer log.",
		Params:      []string{"stress.sqlancer", "apps.sqlancer"},
		Timeout:     6 * time.Hour,
		Run:         runSQLancer,
	})
}

func runSQLancer(t *core.T) error {
	if !t.Options.GetBool("stress.sqlancer.enabled", false) {
		t.Skip("stress.sqlancer.enabled is off")
		return nil
	}
	env, ok := t.Env.(ampTenantEnv)
	if !ok {
		t.Skip("requires an env exposing a standing amp control plane (sharedk8s)")
		return nil
	}
	kubeEnv, ok := t.Env.(ampKubeEnv)
	if !ok || kubeEnv.AmpKube() == nil {
		t.Skip("requires a kube context for the control-plane namespace: SQLancer runs in a pod beside the endpoint")
		return nil
	}
	addrEnv, ok := t.Env.(endpointAddrEnv)
	if !ok {
		t.Skip("this env cannot name an endpoint's in-cluster address, so a pod cannot dial it")
		return nil
	}

	cfg := sqlancer.FromConfig(t.Options)
	oracles := t.Options.GetStringSlice("stress.sqlancer.oracles", nil)
	if len(oracles) == 0 {
		oracles = sqlancer.Oracles
	}
	// One endpoint per concurrent oracle: two oracles on one endpoint would
	// share its connection slots and CPU, and a finding could not be pinned to
	// the oracle whose load caused it.
	endpoints := t.Options.GetInt("stress.sqlancer.endpoints", 4)
	if endpoints < 1 {
		endpoints = 1
	}
	if endpoints > len(oracles) {
		endpoints = len(oracles)
	}
	qpg := map[string]bool{}
	for _, o := range t.Options.GetStringSlice("stress.sqlancer.qpg_oracles", nil) {
		qpg[strings.ToUpper(strings.TrimSpace(o))] = true
	}
	logf := t.Masker().Logf(log.Printf)
	kube := kubeEnv.AmpKube()
	sink := attachSink{t: t, logf: logf}

	fleet, err := newTenantFleet(t, env, logf)
	if err != nil {
		return err
	}
	fleet.EndpointCPU = t.Options.GetString("stress.sqlancer.endpoint_cpu", "")
	fleet.EndpointMemory = t.Options.GetString("stress.sqlancer.endpoint_memory", "")
	fleet.EndpointPgSettings = stringSettings(t.Options.GetMap("stress.sqlancer.endpoint_pg_settings", nil))
	if ps := t.Options.GetMap("stress.sqlancer.endpoint_pageserver_settings", nil); len(ps) > 0 {
		fleet.EndpointPageserverSettings = ps
	}
	defer fleet.Close()
	// Registered after fleet.Close, so it runs before it: the env's teardown
	// diagnostics run after the tenant is deleted and would otherwise collect
	// some other endpoint's logs, never the one SQLancer drove.
	defer attachTenantEndpointLogs(kube, fleet, sink, logf)

	pod := clientpod.NewK8s(kube)
	podStarted := false
	defer func() {
		if !podStarted {
			return
		}
		// Its own deadline: the test context is usually already cancelled here.
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		if err := pod.Stop(ctx, sink); err != nil {
			logf("sqlancer: stop runner pod: %v", err)
		}
	}()

	var targets []sqlancer.Target
	if err := t.Step("provision", func(s *core.StepHandle) error {
		// Setup, not the subject: nothing has run yet, so a failure here says
		// nothing about amp's answers.
		if err := fleet.Create(t, endpoints); err != nil {
			return core.AsEnvFailure(fmt.Errorf("stand up the %d tenant(s) SQLancer runs against: %w", endpoints, err))
		}
		// Set before Start: Start applies the pod and then waits for Ready, so a
		// failed wait still leaves a pod that teardown has to delete.
		podStarted = true
		if err := pod.Start(t.Context, clientpod.Spec{
			Name:    "ampqa-sqlancer-" + t.ResourceSuffix(),
			Image:   cfg.Image,
			Command: []string{"sh", "-c", "sleep infinity"},
		}); err != nil {
			return core.AsEnvFailure(fmt.Errorf("start the SQLancer runner pod: %w", err))
		}
		var addrs []string
		for i := range fleet.Tenants {
			host, port := addrEnv.EndpointInClusterAddr(fleet.Tenants[i].Endpoint)
			// SQLancer creates, drops and alters databases, roles and system
			// settings, so it needs the endpoint's superuser.
			targets = append(targets, sqlancer.Target{
				Host:     host,
				Port:     port,
				User:     t.Options.GetString("functional.amp.endpoint_user", "cloud_admin"),
				Password: t.Options.GetString("functional.amp.endpoint_password", "cloud_admin"),
			})
			addrs = append(addrs, fmt.Sprintf("%s:%d", host, port))
		}
		s.Set("endpoints", strings.Join(addrs, ","))
		s.Set("endpoint_resources", fmt.Sprintf("%s/%s", fleet.endpointResources().CPU, fleet.endpointResources().Memory))
		s.Set("endpoint_pg_settings", fmt.Sprint(fleet.EndpointPgSettings))
		s.Set("endpoint_pageserver_settings", fmt.Sprint(fleet.EndpointPageserverSettings))
		return nil
	}); err != nil {
		return err
	}

	app := sqlancer.New(pod.Exec(), cfg, logf)
	var jar string
	if err := t.Step("install", func(s *core.StepHandle) error {
		got, err := app.Install(t.Context)
		if err != nil {
			return core.AsEnvFailure(err)
		}
		jar = got.Jar
		// The release tag is recreated on every master-yb push, so the commit and
		// the tarball's sha256 are the only record of what this run tested.
		s.Set("release", cfg.ReleaseRepo+"@"+cfg.ReleaseTag)
		s.Set("commit", got.Commit)
		s.Set("sha256", got.SHA256)
		s.Set("jar", jar)
		return nil
	}); err != nil {
		return err
	}

	// Each worker owns one endpoint and takes the next oracle from the queue,
	// so a slow oracle does not hold the others back.
	queue := make(chan string, len(oracles))
	for _, o := range oracles {
		queue <- strings.ToUpper(strings.TrimSpace(o))
	}
	close(queue)
	var (
		mu      sync.Mutex
		found   []string
		stepErr error
		wg      sync.WaitGroup
	)
	for w := range targets {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for oracle := range queue {
				err := t.Step(oracle, func(s *core.StepHandle) error {
					extra := append([]string(nil), cfg.ExtraArgs...)
					if qpg[oracle] {
						extra = append(extra, "--qpg-enable", "true")
					}
					spec := sqlancer.RunSpec{
						Oracle:  oracle,
						Target:  targets[w],
						Jar:     jar,
						WorkDir: cfg.InstallDir + "-runs/" + strings.ToLower(oracle),
						Threads: cfg.Threads,
						Timeout: cfg.TimePerOracle,
						MaxHeap: cfg.MaxHeap,
						Extra:   extra,
					}
					s.Set("endpoint", fmt.Sprintf("%s:%d", targets[w].Host, targets[w].Port))
					s.Set("threads", spec.Threads)
					s.Set("timeout", spec.Timeout.String())
					s.Set("qpg", qpg[oracle])
					res, err := app.Run(t.Context, spec)
					app.Collect(t.Context, spec, res, sink)
					if err != nil {
						return core.AsEnvFailure(err)
					}
					s.Set("verdict", string(res.Verdict))
					s.Set("exit_code", res.ExitCode)
					s.Set("reproducers", len(res.Findings))
					if res.RunError != "" {
						s.Set("run_error", res.RunError)
					}
					// A finding does not stop the queue: one oracle's bug must
					// not hide what the rest would have found.
					if res.Verdict == sqlancer.VerdictFindings {
						mu.Lock()
						found = append(found, fmt.Sprintf("%s (%d)", oracle, len(res.Findings)))
						mu.Unlock()
					}
					return nil
				})
				if err != nil {
					mu.Lock()
					if stepErr == nil {
						stepErr = err
					}
					mu.Unlock()
					// An env failure on one endpoint ends that worker only; the
					// others finish the oracles already running.
					return
				}
			}
		}()
	}
	wg.Wait()
	// Findings outrank an env error from another oracle: the reproducers are a
	// product signal, and the env error alone would class the result
	// env_failed. The verdict carries its text unwrapped (%v) for that reason.
	if stepErr != nil && len(found) == 0 {
		return stepErr
	}

	return t.Step("verdict", func(s *core.StepHandle) error {
		s.Set("oracles_run", len(oracles))
		s.Set("oracles_with_findings", len(found))
		if len(found) > 0 {
			err := fmt.Errorf("SQLancer wrote reproducer logs for %d oracle(s): %s — see the sqlancer/<oracle>/ attachments",
				len(found), strings.Join(found, ", "))
			if stepErr != nil {
				s.Set("env_error", stepErr.Error())
				err = fmt.Errorf("%w; an oracle also stopped on an environment error: %v", err, stepErr)
			}
			return err
		}
		return nil
	})
}

// attachTenantEndpointLogs attaches every container's log of the tenants'
// endpoint pods, including a restarted container's previous run. It roots its
// own context because the test's is usually cancelled by teardown.
func attachTenantEndpointLogs(kube *k8s.Kubectl, fleet *tenantFleet, sink attachSink, logf func(string, ...any)) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	all, err := kube.ListPods(ctx)
	if err != nil {
		logf("sqlancer: list pods for endpoint logs: %v", err)
		return
	}
	for _, p := range fleetEndpointPods(all, fleet.Tenants) {
		// The endpoint logs every request at INFO, so a long run is mostly
		// that; the tail bounds the attachment, not the evidence window.
		if out, err := kube.LogsAll(ctx, "pod/"+p, 200000); err != nil {
			logf("sqlancer: logs of endpoint pod %s: %v", p, err)
		} else {
			_ = sink.Add("endpoint/"+p+"/containers.log", "log", []byte(out))
		}
		// Errors when nothing restarted, which is the expected case.
		if out, err := kube.LogsAllPrevious(ctx, "pod/"+p, 200000); err == nil {
			_ = sink.Add("endpoint/"+p+"/containers.previous.log", "log", []byte(out))
		}
	}
}

// stringSettings renders a YAML map of Postgres settings as the strings the
// controller takes; nil stays nil so the product defaults apply.
func stringSettings(m map[string]any) map[string]string {
	if len(m) == 0 {
		return nil
	}
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[k] = fmt.Sprint(v)
	}
	return out
}
