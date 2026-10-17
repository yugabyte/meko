// Chaos aimed at the storage daemons INSIDE an endpoint pod.
//
// stress/amp_nemesis deletes the whole endpoint pod and stress/backing_nemesis
// deletes the backing YugabyteDB's pods. Both take everything in the pod down
// together, so neither ever runs a pageserver or safekeeper death UNDER a live
// postgres — the fault the endpoint supervisor is actually written for: it
// respawns a crashed child in place and only exits, letting the kubelet recreate
// the pod, once a child exceeds its restart budget
// (compute/endpoint_supervisor/src/supervise.rs:381-396).
//
// This test kills those daemons by pid while journaled writes are running,
// verifies the process really died, waits for the supervisor to bring it back,
// and then holds the product to the exact acknowledged set, to reads over a
// different connection than the one that was writing, and to writes flowing
// again.
package stress

import (
	"context"
	"fmt"
	"log"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/yugabyte/amp/test/functional/framework/apps/controller"
	"github.com/yugabyte/amp/test/functional/framework/core"
	"github.com/yugabyte/amp/test/functional/framework/exec/k8s"
)

func init() {
	core.Register(core.Test{
		Name:        "stress/storage_proc_nemesis",
		Suite:       "stress",
		Category:    core.Stress,
		Env:         core.EnvSpec{Sharing: core.Exclusive, Shape: core.DedicatedStack},
		Tags:        []string{"stress"},
		OwningTeam:  core.TeamControlPlane,
		Description: "Kill the pageserver and safekeeper processes by pid inside a tenant's endpoint pod, under a live postgres, while journaled writes run; then hold the endpoint supervisor to what it promises — the daemon respawned in place within its restart budget, no pod recreation — and hold storage to the exact acknowledged set, to a read over a fresh connection, and to writes committing again.",
		Params:      []string{"stress.storage_proc_nemesis", "stress.consistency"},
		Timeout:     90 * time.Minute,
		Run:         runStorageProcNemesis,
	})
}

// The endpoint supervisor's restart policy (supervise.rs:245-256): a crashed
// child is respawned after backoff, up to max restarts inside a rolling window,
// and only past that does the supervisor exit so the pod is recreated
// (supervise.rs:382-393, main.rs:456-458).
const (
	supervisorMaxRestarts   = 5
	supervisorRestartWindow = 180 * time.Second
)

// storageDaemonNames maps each supervised child this test may aim at to its
// process name (boot.rs services(): safekeeper, pageserver, compute_ctl, the
// last spawned as the `compute-ctl` binary). compute_ctl takes its postmaster
// down with it (meko#898) and the supervisor must respawn it in place like the
// storage daemons. postgres itself is compute-ctl's child, not the supervisor's,
// so it is not a target here.
var storageDaemonNames = map[string]string{"pageserver": "pageserver", "safekeeper": "safekeeper", "compute_ctl": "compute-ctl"}

// daemonProcess is the `comm` a service name is killed and re-found by.
func daemonProcess(service string) string {
	if p, ok := storageDaemonNames[service]; ok {
		return p
	}
	return service
}

// killConfirmTimeout bounds how long a killed pid may still be listed. The
// supervisor respawns after a 2s backoff, so this only has to outlast the kill
// itself, not the restart.
const killConfirmTimeout = 30 * time.Second

// pidPollInterval is how often the pod is asked for the daemon's pids.
const pidPollInterval = time.Second

const pidListAttempts = 5

type storageProcParams struct {
	Tenants          int
	BaselineDuration time.Duration
	// FaultDuration is the write window the kills land inside.
	FaultDuration  time.Duration
	Daemons        []string
	KillsPerDaemon int
	KillGap        time.Duration
	SettleTimeout  time.Duration
	ResumeDuration time.Duration
	PayloadBytes   int
}

func storageProcParamsFromConfig(c *core.Config) storageProcParams {
	return storageProcParams{
		Tenants:          c.GetInt("stress.storage_proc_nemesis.tenants", 2),
		BaselineDuration: c.GetDuration("stress.storage_proc_nemesis.baseline_duration", 20*time.Second),
		FaultDuration:    c.GetDuration("stress.storage_proc_nemesis.fault_duration", 90*time.Second),
		Daemons:          c.GetStringSlice("stress.storage_proc_nemesis.daemons", []string{"pageserver", "safekeeper", "compute_ctl"}),
		KillsPerDaemon:   c.GetInt("stress.storage_proc_nemesis.kills_per_daemon", 1),
		KillGap:          c.GetDuration("stress.storage_proc_nemesis.kill_gap", 15*time.Second),
		SettleTimeout:    c.GetDuration("stress.storage_proc_nemesis.settle_timeout", 10*time.Minute),
		ResumeDuration:   c.GetDuration("stress.storage_proc_nemesis.resume_duration", 30*time.Second),
		PayloadBytes:     c.GetInt("stress.storage_proc_nemesis.payload_bytes", 256),
	}
}

func runStorageProcNemesis(t *core.T) error {
	env, ok := t.Env.(ampTenantEnv)
	if !ok {
		t.Skip("requires an env exposing a standing amp control plane (sharedk8s)")
		return nil
	}
	ampEnv, ok := t.Env.(ampKubeEnv)
	if !ok || ampEnv.AmpKube() == nil {
		t.Skip("requires a kube handle for the control-plane namespace: the daemons live inside the endpoint pods and are only reachable by pod exec")
		return nil
	}
	params := storageProcParamsFromConfig(t.Options)
	if err := validateStorageDaemons(params.Daemons); err != nil {
		return err
	}
	logf := t.Masker().Logf(log.Printf)
	kube := ampEnv.AmpKube()

	fleet, err := newTenantFleet(t, env, logf)
	if err != nil {
		return err
	}
	defer fleet.Close()

	var journals []*AckJournal

	if err := t.Step("create_tenants", func(s *core.StepHandle) error {
		if err := fleet.Create(t, params.Tenants); err != nil {
			return err
		}
		journals = make([]*AckJournal, len(fleet.Tenants))
		for i := range journals {
			journals[i] = NewAckJournal()
		}
		s.Set("tenants", len(fleet.Tenants))
		return nil
	}); err != nil {
		return err
	}

	if err := t.Step("seed_writes", func(s *core.StepHandle) error {
		runJournaledWriters(t.Context, env, fleet.Tenants, journals, t.Seed.Fork("seed"),
			params.BaselineDuration, commitDurabilityParams{PayloadBytes: params.PayloadBytes}, nil)
		acked := totalAcked(journals)
		s.Set("acked", acked)
		if acked == 0 {
			return fmt.Errorf("no tenant was acknowledged for a single write in %s before the kills, so there is no promise to hold the product to",
				params.BaselineDuration)
		}
		return nil
	}); err != nil {
		return err
	}

	var kills []killRecord
	var gate *settleGate

	if err := t.Step("kill_storage_daemons", func(s *core.StepHandle) error {
		gate, err = awaitSteadyState(t, s, env, fleet.Tenants, journals, params.PayloadBytes, 0)
		if err != nil {
			return err
		}
		kills, err = killStorageDaemons(t, s, env, kube, fleet, journals, params, logf)
		return err
	}); err != nil {
		return err
	}

	if err := t.Step("daemons_restarted", func(s *core.StepHandle) error {
		if err := assertDaemonsRestarted(t, s, kube, kills, params); err != nil {
			return err
		}
		return gate.attestNonTarget(t, s, fleetEndpointTargets(fleet.Tenants)...)
	}); err != nil {
		return err
	}

	if err := t.Step("endpoints_serve_again", func(s *core.StepHandle) error {
		if err := assertEndpointsServe(t, env, fleet.Tenants, params.SettleTimeout); err != nil {
			return err
		}
		s.Set("endpoints_recovered", len(fleet.Tenants))
		return nil
	}); err != nil {
		return err
	}

	// A read over a connection that never wrote: the getPage path has to work for
	// a backend other than the one whose connection the fault disturbed.
	if err := t.Step("reads_serve_after_kill", func(s *core.StepHandle) error {
		return probeReadPath(t, s, env, fleet.Tenants, params.SettleTimeout)
	}); err != nil {
		return err
	}

	if err := t.Step("verify_exact_sets", func(s *core.StepHandle) error {
		return verifyExactSets(t, s, env, fleet.Tenants, journals, params.PayloadBytes)
	}); err != nil {
		return err
	}

	// SELECT 1 answering while no COMMIT ever returns is the shape of meko#727,
	// and an ordinary OOM reaches it as easily as an injected kill does.
	if err := t.Step("writes_flow_again", func(s *core.StepHandle) error {
		before := totalAcked(journals)
		runJournaledWriters(t.Context, env, fleet.Tenants, journals, t.Seed.Fork("resume"),
			params.ResumeDuration, commitDurabilityParams{PayloadBytes: params.PayloadBytes}, nil)
		delta := totalAcked(journals) - before
		s.Set("acked_after_recovery", delta)
		if delta == 0 {
			return fmt.Errorf("no tenant was acknowledged for a single write in %s after every killed daemon was back and every endpoint answered SELECT 1: the endpoints serve reads but no COMMIT completes",
				params.ResumeDuration)
		}
		return verifyExactSets(t, s, env, fleet.Tenants, journals, params.PayloadBytes)
	}); err != nil {
		return err
	}

	return t.Step("physical_consistency", func(s *core.StepHandle) error {
		return verifyPhysicalConsistency(t, s, env, fleet.Tenants)
	})
}

func validateStorageDaemons(daemons []string) error {
	if len(daemons) == 0 {
		return fmt.Errorf("stress.storage_proc_nemesis.daemons is empty, so no fault would be injected at all")
	}
	for _, d := range daemons {
		if _, ok := storageDaemonNames[d]; !ok {
			return fmt.Errorf("stress.storage_proc_nemesis.daemons names %q, which is not a child the endpoint supervisor runs (pageserver, safekeeper, compute_ctl)", d)
		}
	}
	return nil
}

// daemonTarget is one daemon killed in one tenant's endpoint pod.
type daemonTarget struct {
	Daemon string
	Tenant int
}

// storageKillPlan picks, from the seed alone, the order the daemons are killed
// in and which tenant's pod each is killed in.
func storageKillPlan(seed *core.Seed, tenants int, daemons []string) []daemonTarget {
	if tenants <= 0 || len(daemons) == 0 {
		return nil
	}
	rnd := seed.Rand()
	plan := make([]daemonTarget, 0, len(daemons))
	for _, i := range rnd.Perm(len(daemons)) {
		plan = append(plan, daemonTarget{Daemon: daemons[i], Tenant: rnd.IntN(tenants)})
	}
	return plan
}

// killRecord is one landed kill and what it takes to judge the recovery.
type killRecord struct {
	Daemon     string
	Tenant     int
	Pod        string
	PIDs       []int
	Confirm    time.Duration
	KilledAt   time.Time
	PodUID     string
	Restarts   string
	Recovery   time.Duration
	NewPIDs    []int
	KillsInPod int
}

// killStorageDaemons kills the planned daemons by pid while every tenant writes.
// A kill counts only once the pids it signalled are gone from the pod.
func killStorageDaemons(t *core.T, s *core.StepHandle, env ampTenantEnv, kube *k8s.Kubectl,
	fleet *tenantFleet, journals []*AckJournal, params storageProcParams,
	logf func(string, ...any)) ([]killRecord, error) {
	plan := storageKillPlan(t.Seed.Fork("storage-proc"), len(fleet.Tenants), params.Daemons)
	if len(plan) == 0 {
		return nil, fmt.Errorf("nothing to kill: %d tenant(s), %d daemon(s)", len(fleet.Tenants), len(params.Daemons))
	}

	// The journals already carry the seed writes, and seed_writes refuses to
	// pass with none, so only the DELTA over this window says whether anything
	// was committed while a daemon was dying.
	ackedBefore := totalAcked(journals)

	writers := make(chan struct{})
	go func() {
		defer close(writers)
		runJournaledWriters(t.Context, env, fleet.Tenants, journals, t.Seed.Fork("fault-writers"),
			params.FaultDuration, commitDurabilityParams{PayloadBytes: params.PayloadBytes}, nil)
	}()

	var (
		records []killRecord
		targets []string
		failed  []string
	)
	for _, tgt := range plan {
		ten := &fleet.Tenants[tgt.Tenant]
		pod, err := endpointPodFor(t, kube, ten.Endpoint)
		if err != nil {
			<-writers
			return nil, err
		}
		uid, restarts, err := podIdentity(t.Context, kube, pod)
		if err != nil {
			<-writers
			return nil, err
		}
		targets = append(targets, fmt.Sprintf("%s@%s", tgt.Daemon, pod))
		for n := 0; n < params.KillsPerDaemon; n++ {
			rec, err := killDaemonOnce(t, kube.ForPod(pod, ""), tgt.Daemon)
			if err != nil {
				failed = append(failed, err.Error())
				logf("storage_proc_nemesis: %v", err)
			} else {
				rec.Tenant, rec.Pod, rec.PodUID, rec.Restarts = tgt.Tenant, pod, uid, restarts
				rec.KillsInPod = params.KillsPerDaemon
				records = append(records, rec)
			}
			if n+1 < params.KillsPerDaemon {
				if err := sleepCtx(t.Context, params.KillGap); err != nil {
					break
				}
			}
		}
	}
	<-writers

	s.Set("targets", strings.Join(targets, ","))
	s.Set("kills_landed", len(records))
	s.Set("kills_planned", len(plan)*params.KillsPerDaemon)
	ackedUnderFault := totalAcked(journals) - ackedBefore
	s.Set("acked_under_fault", ackedUnderFault)
	if len(records) == 0 {
		return nil, fmt.Errorf("not one kill landed on a storage daemon in %s, so this run injected no fault and proves nothing: %s",
			params.FaultDuration, strings.Join(failed, "; "))
	}
	// Faults landing is half of it: with nothing committed while they landed,
	// the exact-set verification afterwards has no commit that crossed a fault
	// to check, and the run passes having proven nothing about durability.
	if ackedUnderFault == 0 {
		return nil, fmt.Errorf("%d kill(s) landed but no tenant committed a single row during the %s fault window, so nothing was written across a daemon death and the verification below has nothing to prove",
			len(records), params.FaultDuration)
	}
	return records, nil
}

// killDaemonOnce lists the daemon's pids in the pod, SIGKILLs exactly those, and
// returns only once they are gone — the shell's exit code says the signal was
// sent, not that anything died.
func killDaemonOnce(t *core.T, ex *k8s.Kubectl, daemon string) (killRecord, error) {
	rec := killRecord{Daemon: daemon}
	before, err := daemonPIDs(t.Context, ex, daemonProcess(daemon))
	if err != nil {
		return rec, err
	}
	if len(before) == 0 {
		return rec, fmt.Errorf("no %s process was running in the pod before the kill, though the endpoint supervisor starts one and keeps it up: there was nothing to fault", daemon)
	}
	rec.PIDs = before

	if _, err := ex.Run(t.Context, "kill -KILL "+joinPIDs(before, " ")); err != nil {
		return rec, fmt.Errorf("signalling %s pid(s) %s: %w", daemon, joinPIDs(before, ","), err)
	}

	start := time.Now()
	var listErr error
	err = t.WaitFor(fmt.Sprintf("%s pid(s) %s to disappear", daemon, joinPIDs(before, ",")),
		killConfirmTimeout, pidPollInterval, func() (bool, error) {
			now, err := daemonPIDs(t.Context, ex, daemonProcess(daemon))
			if listErr = err; err != nil {
				// A kill past the restart budget restarts the container, and the
				// exec fails until it is back.
				return false, nil
			}
			return len(intersectPIDs(before, now)) == 0, nil
		})
	if err != nil && listErr != nil {
		return rec, core.AsEnvFailure(fmt.Errorf("could not list %s in the pod for %s after the SIGKILL to pid(s) %s: %w",
			daemon, killConfirmTimeout, joinPIDs(before, ","), listErr))
	}
	if err != nil {
		return rec, fmt.Errorf("SIGKILL to %s pid(s) %s left them running after %s, so no fault was injected: %w",
			daemon, joinPIDs(before, ","), killConfirmTimeout, err)
	}
	rec.Confirm = time.Since(start)
	rec.KilledAt = time.Now()
	return rec, nil
}

// assertDaemonsRestarted holds the supervisor to its promise: a child killed
// inside its restart budget comes back in the SAME pod, respawned in place.
func assertDaemonsRestarted(t *core.T, s *core.StepHandle, kube *k8s.Kubectl,
	kills []killRecord, params storageProcParams) error {
	for i := range kills {
		rec := &kills[i]
		ex := kube.ForPod(rec.Pod, "")
		start := time.Now()
		var back []int
		err := t.WaitFor(fmt.Sprintf("%s to be respawned in pod %s", rec.Daemon, rec.Pod),
			params.SettleTimeout, pidPollInterval, func() (bool, error) {
				now, err := daemonPIDs(t.Context, ex, daemonProcess(rec.Daemon))
				if err != nil {
					// The pod is replaced only past the restart budget; until the
					// exec works again there is nothing to conclude.
					return false, nil
				}
				back = subtractPIDs(now, rec.PIDs)
				return len(back) > 0, nil
			})
		if err != nil {
			return fmt.Errorf("the endpoint supervisor respawns a crashed child up to %d times per %s and only exits past that (supervise.rs:381-396), but pod %s ran no %s again within %s of its pid(s) %s being killed: %w",
				supervisorMaxRestarts, supervisorRestartWindow, rec.Pod, rec.Daemon,
				params.SettleTimeout, joinPIDs(rec.PIDs, ","), err)
		}
		rec.Recovery = time.Since(rec.KilledAt)
		rec.NewPIDs = back
		s.Set(fmt.Sprintf("poll_seconds_%s_%d", rec.Daemon, rec.Tenant), time.Since(start).Seconds())
		// An upper bound: polling starts after the write window, so a daemon back
		// long before this step still reads as the whole window.
		s.Set(fmt.Sprintf("respawn_seen_after_kill_seconds_%s_%d", rec.Daemon, rec.Tenant), rec.Recovery.Seconds())
		s.Set(fmt.Sprintf("pid_before_%s_%d", rec.Daemon, rec.Tenant), joinPIDs(rec.PIDs, ","))
		s.Set(fmt.Sprintf("pid_after_%s_%d", rec.Daemon, rec.Tenant), joinPIDs(back, ","))

		uid, restarts, err := podIdentity(t.Context, kube, rec.Pod)
		if err != nil {
			return err
		}
		s.Set(fmt.Sprintf("container_restarts_%s_%d", rec.Daemon, rec.Tenant), restarts)
		// Only a run that stayed inside the budget is entitled to this assertion.
		if rec.KillsInPod > supervisorMaxRestarts {
			continue
		}
		if uid != rec.PodUID || restarts != rec.Restarts {
			return fmt.Errorf("pod %s was recreated or its container restarted after %d kill(s) of %s: the supervisor exits only once a child exceeds %d restarts in %s (supervise.rs:382-393), so a killed daemon inside that budget must be respawned in place (uid %s -> %s, container restarts %q -> %q)",
				rec.Pod, rec.KillsInPod, rec.Daemon, supervisorMaxRestarts, supervisorRestartWindow,
				rec.PodUID, uid, rec.Restarts, restarts)
		}
	}
	return nil
}

// probeReadPath reads every tenant's table over a connection that never wrote,
// so a read path that only recovers for the writer's own backend is caught.
func probeReadPath(t *core.T, s *core.StepHandle, env ampTenantEnv, tenants []tenant, settle time.Duration) error {
	start := time.Now()
	for i := range tenants {
		ten := &tenants[i]
		var last error
		err := t.WaitFor(fmt.Sprintf("tenant %d to answer a read on a fresh connection", ten.Index),
			settle, 5*time.Second, func() (bool, error) {
				conn, err := env.ConnectEndpoint(t.Context, ten.Endpoint)
				if err != nil {
					last = err
					return false, nil
				}
				defer conn.Close()
				bad, err := conn.QueryInt(t.Context, fmt.Sprintf(
					"SELECT count(*) FROM %s WHERE v <> id * 7", ten.Table))
				if err != nil {
					last = err
					return false, nil
				}
				if bad != 0 {
					return false, fmt.Errorf("tenant %d (%s) reads back %d row(s) in %s whose v is not id*7",
						ten.Index, ten.Name, bad, ten.Table)
				}
				return true, nil
			})
		if err != nil {
			return fmt.Errorf("tenant %d (%s) cannot be read on a connection that never wrote, %s after the daemon kills, though its endpoint answers SELECT 1: %w (last read error: %v)",
				ten.Index, ten.Name, settle, err, last)
		}
	}
	s.Set("read_probe_seconds", time.Since(start).Seconds())
	return nil
}

// endpointPodFor finds the compute pod serving one endpoint. The controller
// names an endpoint's deployment "yb-ep-<endpoint id>" (controller/internal/
// task/tasks.go:318), so its pods carry that name as a prefix.
func endpointPodFor(t *core.T, kube *k8s.Kubectl, ep controller.Endpoint) (string, error) {
	pods, err := kube.ListPods(t.Context)
	if err != nil {
		return "", err
	}
	prefix := endpointPodPrefix + ep.ID
	for _, p := range pods {
		if strings.HasPrefix(p, prefix) {
			return p, nil
		}
	}
	return "", fmt.Errorf("no pod named %s* in namespace %q, though the endpoint exists and serves SQL", prefix, kube.Namespace())
}

// podIdentity returns the pod's uid and its per-container restart counts, the
// two things that separate a daemon respawned in place from a pod the kubelet
// restarted or replaced.
func podIdentity(ctx context.Context, kube *k8s.Kubectl, pod string) (uid, restarts string, err error) {
	uid, err = kube.Get(ctx, "pod", pod, "{.metadata.uid}")
	if err != nil {
		return "", "", fmt.Errorf("read uid of pod %s: %w", pod, err)
	}
	restarts, err = kube.Get(ctx, "pod", pod, "{.status.containerStatuses[*].restartCount}")
	if err != nil {
		return "", "", fmt.Errorf("read restart counts of pod %s: %w", pod, err)
	}
	return strings.TrimSpace(uid), strings.TrimSpace(restarts), nil
}

// procScanCommand lists a process's pids from /proc alone, for a container
// without procps. The endpoint image installs it (deploy/endpoint/Dockerfile),
// so this is the fallback, not the route.
func procScanCommand(daemon string) string {
	return fmt.Sprintf(`for d in /proc/[0-9]*; do [ "$(cat $d/comm 2>/dev/null)" = %q ] && echo "${d#/proc/}"; done; exit 0`, daemon)
}

// daemonPIDs lists the pids of one storage daemon inside the pod. kubectl exits 1
// both when pgrep matched nothing and when the exec itself failed, so exit 1 means
// no process only when stderr holds nothing but kubectl's notices for a remote
// exit. Exit 1 with other stderr is retried and then reported as env. Any other
// exit code falls back to a /proc scan, so a missing procps is not read as "the
// daemon is gone".
func daemonPIDs(ctx context.Context, ex *k8s.Kubectl, daemon string) ([]int, error) {
	res, err := ex.Run(ctx, "pgrep -x "+daemon)
	for attempt := 1; err == nil && res.ExitCode == 1 && execStderr(res.Stderr) != ""; attempt++ {
		if attempt == pidListAttempts {
			return nil, core.AsEnvFailure(fmt.Errorf("listing %s processes in the pod: kubectl exec exited 1 %d times: %s",
				daemon, attempt, execStderr(res.Stderr)))
		}
		if err := sleepCtx(ctx, pidPollInterval); err != nil {
			return nil, err
		}
		res, err = ex.Run(ctx, "pgrep -x "+daemon)
	}
	if err != nil {
		return nil, fmt.Errorf("listing %s processes in the pod: %w", daemon, err)
	}
	switch res.ExitCode {
	case 0:
		return parsePIDs(res.Stdout), nil
	case 1:
		return nil, nil
	}
	res, err = ex.Run(ctx, procScanCommand(daemon))
	if err != nil {
		return nil, fmt.Errorf("scanning /proc for %s in the pod: %w", daemon, err)
	}
	if res.ExitCode != 0 {
		return nil, fmt.Errorf("neither pgrep nor a /proc scan could list %s in the pod (exit %d): %s",
			daemon, res.ExitCode, strings.TrimSpace(res.Stderr))
	}
	return parsePIDs(res.Stdout), nil
}

func execStderr(stderr string) string {
	var kept []string
	for _, l := range strings.Split(stderr, "\n") {
		l = strings.TrimSpace(l)
		if l != "" && !strings.HasPrefix(l, "Defaulted container ") && l != "command terminated with exit code 1" {
			kept = append(kept, l)
		}
	}
	return strings.Join(kept, "\n")
}

func parsePIDs(out string) []int {
	var pids []int
	for _, f := range strings.Fields(out) {
		if n, err := strconv.Atoi(f); err == nil && n > 0 {
			pids = append(pids, n)
		}
	}
	sort.Ints(pids)
	return pids
}

func joinPIDs(pids []int, sep string) string {
	parts := make([]string, 0, len(pids))
	for _, p := range pids {
		parts = append(parts, strconv.Itoa(p))
	}
	return strings.Join(parts, sep)
}

// intersectPIDs is the pids present in both sets: what survived a kill.
func intersectPIDs(a, b []int) []int {
	in := make(map[int]bool, len(b))
	for _, p := range b {
		in[p] = true
	}
	var out []int
	for _, p := range a {
		if in[p] {
			out = append(out, p)
		}
	}
	return out
}

// subtractPIDs is a without b: the pids that are not the killed ones.
func subtractPIDs(a, b []int) []int {
	gone := make(map[int]bool, len(b))
	for _, p := range b {
		gone[p] = true
	}
	var out []int
	for _, p := range a {
		if !gone[p] {
			out = append(out, p)
		}
	}
	return out
}
