// A full backing hung clients instead of erroring: disk fills, writes are
// rejected below, and every COMMIT parks in SyncRep forever with the pod
// reporting healthy (meko#727 signature, observed as a 1h45m hang). This fills
// the backing's data disks and holds every statement to an answer. A full disk
// also refuses the endpoint's write-token renewal, so every endpoint stops by
// design (meko#1558). The test holds it to that, then grows the volumes as a
// customer would and holds amp to recovering on its own (meko#1110).
package stress

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/yugabyte/amp/test/functional/framework/core"
	"github.com/yugabyte/amp/test/functional/framework/env/universe"
	"github.com/yugabyte/amp/test/functional/framework/exec/k8s"
)

func init() {
	core.Register(core.Test{
		Name:        "stress/backing_disk_full",
		Suite:       "stress",
		Category:    core.Stress,
		Env:         core.EnvSpec{Sharing: core.Exclusive, Shape: core.DedicatedStack, FaultsSharedInfra: true},
		Tags:        []string{"stress"},
		OwningTeam:  core.TeamCompute,
		Description: "Fill every backing data disk to within a few MiB while tenants write through amp, and hold each INSERT to committing or erroring within the stall limit rather than hanging. Every endpoint must then lose its write token to the refused renewal and stop, by design. Grow the backing's volumes as a customer would, and assert the endpoints come back on their own, writes resume under load and every acknowledged row is present.",
		Params:      []string{"stress.backing_disk_full", "stress.consistency"},
		Timeout:     90 * time.Minute,
		Run:         runBackingDiskFull,
	})
}

type diskFullParams struct {
	Tenants          int
	BaselineDuration time.Duration
	FaultDuration    time.Duration
	StallLimit       time.Duration
	FullStallLimit   time.Duration
	KeepFreeMiB      int
	Fill             string
	SettleTimeout    time.Duration
	ResumeDuration   time.Duration
	PayloadBytes     int
	TokenLossTimeout time.Duration
	ExpandGB         int
	ExpandTimeout    time.Duration
}

const diskFullRestartIsFailKey = "stress.backing_disk_full.non_target_restart_is_failure"

func diskFullParamsFromConfig(c *core.Config) diskFullParams {
	return diskFullParams{
		Tenants:          c.GetInt("stress.backing_disk_full.tenants", 2),
		BaselineDuration: c.GetDuration("stress.backing_disk_full.baseline_duration", 20*time.Second),
		FaultDuration:    c.GetDuration("stress.backing_disk_full.fault_duration", 90*time.Second),
		StallLimit:       c.GetDuration("stress.backing_disk_full.stall_limit", 60*time.Second),
		FullStallLimit:   c.GetDuration("stress.backing_disk_full.full_stall_limit", 2*time.Minute),
		KeepFreeMiB:      c.GetInt("stress.backing_disk_full.keep_free_mib", 16),
		Fill:             c.GetString("stress.backing_disk_full.fill", "all"),
		SettleTimeout:    c.GetDuration("stress.backing_disk_full.settle_timeout", 10*time.Minute),
		ResumeDuration:   c.GetDuration("stress.backing_disk_full.resume_duration", 2*time.Minute),
		PayloadBytes:     c.GetInt("stress.backing_disk_full.payload_bytes", 256),
		TokenLossTimeout: c.GetDuration("stress.backing_disk_full.token_loss_timeout", 5*time.Minute),
		ExpandGB:         c.GetInt("stress.backing_disk_full.expand_gb", 50),
		ExpandTimeout:    c.GetDuration("stress.backing_disk_full.expand_timeout", 30*time.Minute),
	}
}

// fillerName is the file the filler leaves on each data disk; the suffix keeps
// two runs on one backing from freeing each other's file.
func fillerName(t *core.T) string { return "ampqa_fill_" + t.ResourceSuffix() }

// dataDirsShell sets $dirs to the tserver's data dirs: --fs_data_dirs off its
// command line or its --flagfile, else the chart's /mnt/disk* mounts.
const dataDirsShell = `pid=$(pgrep -xo yb-tserver || pgrep -xo yugabyted || echo 1)
argv=$(tr '\0' '\n' < /proc/$pid/cmdline 2>/dev/null || true)
dirs=$(printf '%s\n' "$argv" | sed -n 's/^--fs_data_dirs=//p' | tr ',' ' ')
if [ -z "$dirs" ]; then
  for f in $(printf '%s\n' "$argv" | awk '/^--flagfile=/{sub(/^--flagfile=/,""); print} /^--flagfile$/{getline; print}'); do
    dirs="$dirs $(sed -n 's/^--fs_data_dirs=//p' "$f" 2>/dev/null | tr ',' ' ')"
  done
fi
[ -n "$(echo $dirs)" ] || dirs=$(awk '$2 ~ /^\/mnt\/disk/ {print $2}' /proc/mounts)
dirs=$(echo $dirs)
`

// fillScript grows one file per data dir until keep MiB are left. A dir on the
// node's own filesystem is skipped: filling it puts the node under DiskPressure
// and the kubelet evicts the backing pods, which is not the fault under test.
func fillScript(file string, keepMiB int) string {
	return fmt.Sprintf(`set -e
%s
[ -n "$dirs" ] || { echo "no data dir found" >&2; exit 3; }
nodedev=" $(stat -c %%d /) $(stat -c %%d /etc/hosts 2>/dev/null) "
filled=0
for d in $dirs; do
  [ -d "$d" ] || { echo "skip $d: missing"; continue; }
  case "$nodedev" in *" $(stat -c %%d "$d") "*) echo "skip $d: on the node's own filesystem"; continue;; esac
  avail=$(df -Pk "$d" | awk 'NR==2{print $4}')
  want=$((avail - %d*1024))
  [ "$want" -gt 1024 ] || { echo "skip $d avail=${avail}k"; continue; }
  fallocate -l "${want}k" "$d/%s" || dd if=/dev/zero of="$d/%s" bs=1M count=$((want/1024)) status=none
  echo "filled $d by ${want}k, left $(df -Pk "$d" | awk 'NR==2{print $4}')k"
  filled=$((filled + 1))
done
[ "$filled" -gt 0 ] || { echo "no data dir on its own volume to fill among: $dirs" >&2; exit 4; }`, dataDirsShell, keepMiB, file, file)
}

func freeScript(file string) string {
	return fmt.Sprintf(`%s
for d in $dirs $(awk '$2 ~ /^\/mnt\/disk/ {print $2}' /proc/mounts); do rm -f "$d/%s"; done; exit 0`, dataDirsShell, file)
}

// stallStats is what the bounded writers saw while the disks were full.
type stallStats struct {
	acked, errored, hung int64
	slowestNanos         int64
	firstErr             atomic.Value
}

func (st *stallStats) slowest() time.Duration {
	return time.Duration(atomic.LoadInt64(&st.slowestNanos)).Round(time.Millisecond)
}

func runBackingDiskFull(t *core.T) error {
	env, ok := t.Env.(ampTenantEnv)
	if !ok {
		t.Skip("requires an env exposing a standing amp control plane (sharedk8s)")
		return nil
	}
	backingEnv, ok := t.Env.(backingKubeEnv)
	if !ok || backingEnv.BackingKube() == nil {
		t.Skip("requires a kube handle for the backing's namespace: the data disks are only reachable by pod exec")
		return nil
	}
	if o, ok := t.Env.(interface{ OwnsUniverse() bool }); ok && !o.OwnsUniverse() {
		t.Skip("the backing is an attached universe, and growing its volumes would outlive this run")
		return nil
	}
	params := diskFullParamsFromConfig(t.Options)
	logf := t.Masker().Logf(log.Printf)
	kube := backingEnv.BackingKube()
	file := fillerName(t)

	fleet, err := newTenantFleet(t, env, logf)
	if err != nil {
		return err
	}
	defer fleet.Close()

	var pods []string
	filled := false
	free := func() {
		if !filled {
			return
		}
		ctx, cancel := context.WithTimeout(context.WithoutCancel(t.Context), 3*time.Minute)
		defer cancel()
		for _, pod := range pods {
			if _, err := kube.ForPod(pod, "").Run(ctx, freeScript(file)); err != nil {
				logf("backing_disk_full: free %s on %s: %v", file, pod, err)
			}
		}
		filled = false
	}
	defer free()

	if err := t.Step("create_tenants", func(s *core.StepHandle) error {
		if err := fleet.Create(t, params.Tenants); err != nil {
			return err
		}
		s.Set("tenants", len(fleet.Tenants))
		return nil
	}); err != nil {
		return err
	}

	journals := make([]*AckJournal, len(fleet.Tenants))
	for i := range journals {
		journals[i] = NewAckJournal()
	}
	if err := t.Step("write_baseline_data", func(s *core.StepHandle) error {
		st := boundedWriters(t.Context, env, fleet.Tenants, journals, params.BaselineDuration, params.StallLimit, params.PayloadBytes)
		s.Set("baseline_rows", st.acked)
		if st.hung > 0 {
			return fmt.Errorf("%d INSERT(s) exceeded %s on a healthy backing before any disk was filled", st.hung, params.StallLimit)
		}
		if st.acked == 0 {
			return fmt.Errorf("no tenant committed a row before the disks were filled")
		}
		return nil
	}); err != nil {
		return err
	}

	var gate *settleGate
	if err := t.Step("fill_backing_disks", func(s *core.StepHandle) error {
		// A cascade out of a full backing is this test's subject, so a pod it
		// never aimed at going down is a product failure here, not env damage.
		gate, err = awaitSteadyState(t, s, env, fleet.Tenants, journals, params.PayloadBytes, 0)
		if err != nil {
			return err
		}
		gate.asProductFailure(t.Options, diskFullRestartIsFailKey)
		pods, err = backingPods(t, kube)
		if err != nil {
			return err
		}
		if len(pods) == 0 {
			return fmt.Errorf("no backing pod matched any known selector in namespace %q", kube.Namespace())
		}
		switch params.Fill {
		case "all":
		case "one":
			pods = pods[:1]
		default:
			return fmt.Errorf("stress.backing_disk_full.fill is %q, want all or one", params.Fill)
		}
		s.Set("fill", params.Fill)
		filled = true
		var report []string
		for _, pod := range pods {
			res, err := kube.ForPod(pod, "").Run(t.Context, fillScript(file, params.KeepFreeMiB))
			if err != nil {
				return core.AsEnvFailure(fmt.Errorf("fill the data disks of %s: %w", pod, err))
			}
			if res.ExitCode != 0 {
				return core.AsEnvFailure(fmt.Errorf("fill the data disks of %s exited %d: %s %s", pod, res.ExitCode, strings.TrimSpace(res.Stdout), strings.TrimSpace(res.Stderr)))
			}
			report = append(report, pod+": "+strings.ReplaceAll(strings.TrimSpace(res.Stdout), "\n", "; "))
		}
		s.Set("filled", strings.Join(report, " | "))
		return nil
	}); err != nil {
		return err
	}

	var fullSlowest time.Duration
	if err := t.Step("writes_answer_on_full_disk", func(s *core.StepHandle) error {
		st := boundedWriters(t.Context, env, fleet.Tenants, journals, params.FaultDuration, params.FullStallLimit, params.PayloadBytes)
		s.Set("acked", st.acked)
		s.Set("errored", st.errored)
		s.Set("hung", st.hung)
		s.Set("slowest_insert", st.slowest().String())
		fullSlowest = st.slowest()
		if e, ok := st.firstErr.Load().(string); ok {
			s.Set("first_error", e)
		}
		if st.hung > 0 {
			return fmt.Errorf("%d INSERT(s) neither committed nor errored within %s while the backing disks were full (%d acked, %d errored): a full backing parks COMMIT forever instead of refusing the write (meko#727)",
				st.hung, params.FullStallLimit, st.acked, st.errored)
		}
		if st.acked == 0 && st.errored == 0 {
			return fmt.Errorf("no INSERT was even attempted during the %s window", params.FaultDuration)
		}
		if params.Fill == "one" && st.acked == 0 {
			return fmt.Errorf("one full backing pod out of %d stopped every write for %s (%d errored): replication should route around a single full node", len(pods), params.FaultDuration, st.errored)
		}
		return nil
	}); err != nil {
		return err
	}

	if err := t.Step("endpoints_lose_write_token", func(s *core.StepHandle) error {
		defer func() { t.Metric("full_disk_slowest_insert_seconds", fullSlowest.Seconds(), "s") }()
		if params.Fill != "all" {
			s.Set("skipped", "fill=one leaves replicas with disk, so no endpoint has to lose its token")
			return nil
		}
		ampEnv, ok := t.Env.(ampKubeEnv)
		if !ok || ampEnv.AmpKube() == nil {
			return core.EnvFail("no kube handle for amp's namespace: the endpoints' logs are the only record of why they stopped")
		}
		loadCtx, stopLoad := context.WithCancel(t.Context)
		done := make(chan *stallStats, 1)
		go func() {
			done <- boundedWriters(loadCtx, env, fleet.Tenants, journals, params.TokenLossTimeout, params.FullStallLimit, params.PayloadBytes)
		}()
		lost, waitErr := awaitWriteTokenLoss(t, ampEnv.AmpKube(), gate, fleet.Tenants, params.TokenLossTimeout)
		stopLoad()
		st := <-done
		s.Set("acked", st.acked)
		s.Set("errored", st.errored)
		s.Set("slowest_insert", st.slowest().String())
		fullSlowest = max(fullSlowest, st.slowest())
		for name, line := range lost {
			s.Set("lost_"+name, line)
		}
		if waitErr != nil {
			return waitErr
		}
		if st.hung > 0 {
			return fmt.Errorf("%d INSERT(s) neither committed nor errored within %s while the endpoints were losing their write tokens: a stopping endpoint must refuse the write, not park it (meko#727)",
				st.hung, params.FullStallLimit)
		}
		return nil
	}); err != nil {
		return err
	}

	if err := t.Step("expand_backing_disks", func(s *core.StepHandle) error {
		how, err := growBackingVolumes(t, kube, pods, params.ExpandGB, params.ExpandTimeout)
		if err != nil {
			return err
		}
		s.Set("expanded", how)
		avail, err := awaitDataDirsAvail(t, kube, pods, int64(params.ExpandGB)*1024*1024/2, params.ExpandTimeout)
		if err != nil {
			return err
		}
		s.Set("avail_after", avail)
		waited, err := awaitBackingReady(t, kube, params.SettleTimeout)
		if err != nil {
			return err
		}
		s.Set("ready_after", waited.Round(time.Second).String())
		targets := append([]string(nil), pods...)
		if params.Fill == "all" {
			targets = append(targets, fleetEndpointTargets(fleet.Tenants)...)
		}
		return gate.attestNonTarget(t, s, targets...)
	}); err != nil {
		return err
	}

	if err := t.Step("writes_flow_again", func(s *core.StepHandle) error {
		waited, err := awaitWritePath(t.Context, env, fleet.Tenants, journals, params.SettleTimeout, params.PayloadBytes)
		if err != nil {
			return fmt.Errorf("the write path never came back after the disks were expanded: %w", err)
		}
		s.Set("write_path_back_after", waited.Round(time.Second).String())
		before := make([]int64, len(fleet.Tenants))
		for i := range fleet.Tenants {
			before[i] = atomic.LoadInt64(&fleet.Tenants[i].Written)
		}
		st := boundedWriters(t.Context, env, fleet.Tenants, journals, params.ResumeDuration, params.StallLimit, params.PayloadBytes)
		s.Set("rows_after_expand", st.acked)
		s.Set("errored_after_expand", st.errored)
		if st.hung > 0 {
			return fmt.Errorf("%d INSERT(s) exceeded %s after the disks were expanded and the backing reported ready", st.hung, params.StallLimit)
		}
		var idle []string
		for i := range fleet.Tenants {
			if atomic.LoadInt64(&fleet.Tenants[i].Written) == before[i] {
				idle = append(idle, fleet.Tenants[i].Name)
			}
		}
		if len(idle) > 0 {
			return fmt.Errorf("%d tenant(s) committed no row in the %s after the disks were expanded: %s", len(idle), params.ResumeDuration, strings.Join(idle, ", "))
		}
		return nil
	}); err != nil {
		return err
	}

	if err := t.Step("verify_exact_sets", func(s *core.StepHandle) error {
		return verifyExactSets(t, s, env, fleet.Tenants, journals, params.PayloadBytes)
	}); err != nil {
		return err
	}

	return t.Step("physical_consistency", func(s *core.StepHandle) error {
		return verifyPhysicalConsistency(t, s, env, fleet.Tenants)
	})
}

// boundedWriters runs one sequential writer per tenant for d, each INSERT under
// its own stall deadline, and journals every id's outcome.
func boundedWriters(ctx context.Context, env ampTenantEnv, tenants []tenant, journals []*AckJournal,
	d, stall time.Duration, payloadBytes int) *stallStats {
	st := &stallStats{}
	loadCtx, cancel := context.WithTimeout(ctx, d)
	defer cancel()
	var wg sync.WaitGroup
	for i := range tenants {
		wg.Add(1)
		go func(ten *tenant, j *AckJournal) {
			defer wg.Done()
			for loadCtx.Err() == nil {
				if ten.Conn == nil {
					conn, err := env.ConnectEndpoint(loadCtx, ten.Endpoint)
					if err != nil {
						sleepCtx(loadCtx, time.Second)
						continue
					}
					ten.Conn = conn
					atomic.AddInt64(&ten.Reconnects, 1)
				}
				id := ten.nextRowID()
				value := id * 7
				atomic.AddInt64(&ten.Attempted, 1)
				stmtCtx, stmtCancel := context.WithTimeout(context.WithoutCancel(loadCtx), stall)
				began := time.Now()
				_, err := ten.Conn.Exec(stmtCtx, fmt.Sprintf(
					"INSERT INTO %s (id, v, pad) VALUES ($1, $2, repeat('t', $3::int)) ON CONFLICT (id) DO NOTHING", ten.Table),
					id, value, payloadBytes)
				stmtCancel()
				for took := int64(time.Since(began)); ; {
					cur := atomic.LoadInt64(&st.slowestNanos)
					if took <= cur || atomic.CompareAndSwapInt64(&st.slowestNanos, cur, took) {
						break
					}
				}
				switch {
				case err == nil:
					atomic.AddInt64(&ten.Written, 1)
					atomic.AddInt64(&st.acked, 1)
					j.RecordAcked(id, value)
				case errors.Is(stmtCtx.Err(), context.DeadlineExceeded):
					atomic.AddInt64(&st.hung, 1)
					j.RecordUnknown(id, value)
					ten.Conn.Close()
					ten.Conn = nil
					return
				default:
					atomic.AddInt64(&ten.WriteErrors, 1)
					atomic.AddInt64(&st.errored, 1)
					st.firstErr.CompareAndSwap(nil, err.Error())
					j.RecordUnknown(id, value)
					ten.Conn.Close()
					ten.Conn = nil
					sleepCtx(loadCtx, time.Second)
				}
			}
		}(&tenants[i], journals[i])
	}
	wg.Wait()
	return st
}

// awaitWriteTokenLoss waits until every tenant's endpoint has logged its lease
// running out over a disk-full refusal. A restart inside the window is judged on
// the previous container alone: a replacement's own acquire failures say nothing
// about why the first one died.
func awaitWriteTokenLoss(t *core.T, kube *k8s.Kubectl, gate *settleGate, tenants []tenant, timeout time.Duration) (map[string]string, error) {
	lost := make(map[string]string)
	deadline := time.Now().Add(timeout)
	for {
		states, err := kube.PodStates(t.Context)
		if err != nil {
			return lost, core.AsEnvFailure(fmt.Errorf("read amp's pods: %w", err))
		}
		names := make([]string, 0, len(states))
		restarts := make(map[string]int, len(states))
		for _, st := range states {
			names = append(names, st.Name)
			restarts[st.Name] = st.Restarts
		}
		var pending []string
		var readErr error
		for i := range tenants {
			ten := &tenants[i]
			if _, ok := lost[ten.Name]; ok {
				continue
			}
			pod := podWithPrefix(names, endpointPodPrefix+ten.Endpoint.ID+"-")
			if pod == "" {
				pending = append(pending, ten.Name+" (no pod)")
				continue
			}
			if restarts[pod] > gate.pods[kube.Namespace()+"/"+pod].Restarts {
				prev, err := kube.LogsPrevious(t.Context, pod, "endpoint", 3000)
				if err != nil {
					readErr = fmt.Errorf("previous log of %s: %w", pod, err)
					pending = append(pending, ten.Name)
					continue
				}
				if line := writeTokenLossLine(prev); line != "" {
					lost[ten.Name] = pod + ": " + line
					continue
				}
				return lost, fmt.Errorf("endpoint pod %s of %s restarted while the disks were full without losing its write token to the full disk; its previous container ended with:\n%s",
					pod, ten.Name, lastLines(ansiEscape.ReplaceAllString(prev, ""), 15))
			}
			cur, err := kube.Logs(t.Context, pod, "endpoint", 3000)
			if err != nil {
				readErr = fmt.Errorf("log of %s: %w", pod, err)
			} else if line := writeTokenLossLine(cur); line != "" {
				lost[ten.Name] = pod + ": " + line
				continue
			}
			pending = append(pending, ten.Name)
		}
		if len(pending) == 0 {
			return lost, nil
		}
		if t.Context.Err() != nil {
			return lost, t.Context.Err()
		}
		if time.Now().After(deadline) {
			if readErr != nil {
				return lost, core.AsEnvFailure(fmt.Errorf("could not read the logs of %d endpoint(s) to see whether they lost their write tokens: %s: %w",
					len(pending), strings.Join(pending, ", "), readErr))
			}
			return lost, fmt.Errorf("%d endpoint(s) kept serving %s into a backing with every data disk full, without logging the write-token renewal refused for disk space and the lease running out: %s",
				len(pending), timeout, strings.Join(pending, ", "))
		}
		time.Sleep(10 * time.Second)
	}
}

var ansiEscape = regexp.MustCompile(`\x1b\[[0-9;]*m`)

func writeTokenLossLine(logs string) string {
	for _, line := range strings.Split(ansiEscape.ReplaceAllString(logs, ""), "\n") {
		if !strings.Contains(line, "write token") {
			continue
		}
		if !strings.Contains(line, "lease ran out") && !strings.Contains(line, "lease lapsed") {
			continue
		}
		if strings.Contains(line, "insufficient disk space") || strings.Contains(line, "No space left") {
			return strings.TrimSpace(line)
		}
	}
	return ""
}

func podWithPrefix(names []string, prefix string) string {
	for _, n := range names {
		if strings.HasPrefix(n, prefix) {
			return n
		}
	}
	return ""
}

func lastLines(s string, n int) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}

func growYBAVolumes(ctx context.Context, client universe.YBAMaintainer, id string, addGB int, timeout time.Duration) (string, error) {
	from, to, err := client.GrowUniverseVolumesAndWait(ctx, id, addGB, timeout)
	if err != nil {
		err = fmt.Errorf("YBA could not grow the universe's tserver volumes by %d GB while they were full: %w", addGB, err)
		// YBA never answered: that is the QA stack's YBA, not amp.
		var ue *url.Error
		if errors.As(err, &ue) {
			return "", core.AsEnvFailure(err)
		}
		return "", err
	}
	return fmt.Sprintf("YBA edit universe: tserver volume %d -> %d GB", from, to), nil
}

// growBackingVolumes is the customer's remedy for a full backing: a bigger
// volume, through YBA when YBA manages the universe, else on the PVCs.
func growBackingVolumes(t *core.T, kube *k8s.Kubectl, pods []string, addGB int, timeout time.Duration) (string, error) {
	if benv, ok := t.Env.(ybaBackedEnv); ok {
		if client, id := benv.YBAUniverse(); client != nil {
			return growYBAVolumes(t.Context, client, id, addGB, timeout)
		}
	}
	var grown []string
	for _, pod := range pods {
		claims, err := kube.Get(t.Context, "pod", pod, "{.spec.volumes[*].persistentVolumeClaim.claimName}")
		if err != nil {
			return "", core.AsEnvFailure(fmt.Errorf("read the PVCs of %s: %w", pod, err))
		}
		for _, claim := range strings.Fields(claims) {
			size, err := kube.Get(t.Context, "pvc", claim, "{.spec.resources.requests.storage}")
			if err != nil {
				return "", core.AsEnvFailure(fmt.Errorf("read the size of PVC %s: %w", claim, err))
			}
			gi, ok := strings.CutSuffix(strings.TrimSpace(size), "Gi")
			n, convErr := strconv.Atoi(gi)
			if !ok || convErr != nil {
				return "", core.EnvFail("PVC %s requests %q, not a whole number of Gi", claim, size)
			}
			patch := fmt.Sprintf(`{"spec":{"resources":{"requests":{"storage":"%dGi"}}}}`, n+addGB)
			if err := kube.Patch(t.Context, "pvc", claim, "merge", patch); err != nil {
				return "", fmt.Errorf("grow PVC %s from %dGi by %d GB: %w", claim, n, addGB, err)
			}
			grown = append(grown, fmt.Sprintf("%s %dGi -> %dGi", claim, n, n+addGB))
		}
	}
	if len(grown) == 0 {
		return "", core.EnvFail("no PVC on any of %s to grow", strings.Join(pods, ", "))
	}
	return "PVC patch: " + strings.Join(grown, ", "), nil
}

// awaitDataDirsAvail waits for the filesystem behind every filled data dir to
// show the new space: the PVC can report the new size before the node has
// resized the filesystem the tserver writes to.
func awaitDataDirsAvail(t *core.T, kube *k8s.Kubectl, pods []string, wantKiB int64, timeout time.Duration) (string, error) {
	deadline := time.Now().Add(timeout)
	for {
		var report, short []string
		for _, pod := range pods {
			res, err := kube.ForPod(pod, "").Run(t.Context, availScript())
			if err != nil || res.ExitCode != 0 {
				short = append(short, fmt.Sprintf("%s: df failed: %v %s", pod, err, strings.TrimSpace(res.Stderr)))
				continue
			}
			for _, line := range strings.Split(strings.TrimSpace(res.Stdout), "\n") {
				dir, kib, ok := strings.Cut(strings.TrimSpace(line), " ")
				if !ok {
					continue
				}
				n, _ := strconv.ParseInt(kib, 10, 64)
				report = append(report, fmt.Sprintf("%s:%s=%dMiB", pod, dir, n/1024))
				if n < wantKiB {
					short = append(short, fmt.Sprintf("%s:%s has %d MiB", pod, dir, n/1024))
				}
			}
		}
		if len(short) == 0 && len(report) > 0 {
			return strings.Join(report, " "), nil
		}
		if t.Context.Err() != nil {
			return "", t.Context.Err()
		}
		if time.Now().After(deadline) {
			return "", fmt.Errorf("the volumes were grown but %d data dir(s) still show under %d MiB free after %s: %s",
				len(short), wantKiB/1024, timeout, strings.Join(short, "; "))
		}
		time.Sleep(15 * time.Second)
	}
}

func availScript() string {
	return fmt.Sprintf(`%s
nodedev=" $(stat -c %%d /) $(stat -c %%d /etc/hosts 2>/dev/null) "
for d in $dirs; do
  [ -d "$d" ] || continue
  case "$nodedev" in *" $(stat -c %%d "$d") "*) continue;; esac
  echo "$d $(df -Pk "$d" | awk 'NR==2{print $4}')"
done`, dataDirsShell)
}
