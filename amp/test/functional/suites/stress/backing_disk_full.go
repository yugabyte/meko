// A full backing hung clients instead of erroring: disk fills, writes are
// rejected below, and every COMMIT parks in SyncRep forever with the pod
// reporting healthy (meko#727 signature, observed as a 1h45m hang). This fills
// the backing's data disks and holds every statement to an answer.
package stress

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/yugabyte/amp/test/functional/framework/core"
)

func init() {
	core.Register(core.Test{
		Name:        "stress/backing_disk_full",
		Suite:       "stress",
		Category:    core.Stress,
		Env:         core.EnvSpec{Sharing: core.Exclusive, Shape: core.SharedControlPlane, FaultsSharedInfra: true},
		Tags:        []string{"stress"},
		OwningTeam:  core.TeamCompute,
		Description: "Fill every backing data disk to within a few MiB while tenants write through amp, and hold each INSERT to committing or erroring within the stall limit rather than hanging; then free the disks and assert writes resume and every acknowledged row is present.",
		Params:      []string{"stress.backing_disk_full"},
		Timeout:     60 * time.Minute,
		Run:         runBackingDiskFull,
	})
}

type diskFullParams struct {
	Tenants          int
	BaselineDuration time.Duration
	FaultDuration    time.Duration
	StallLimit       time.Duration
	KeepFreeMiB      int
	Fill             string
	SettleTimeout    time.Duration
	ResumeDuration   time.Duration
	PayloadBytes     int
}

func diskFullParamsFromConfig(c *core.Config) diskFullParams {
	return diskFullParams{
		Tenants:          c.GetInt("stress.backing_disk_full.tenants", 2),
		BaselineDuration: c.GetDuration("stress.backing_disk_full.baseline_duration", 20*time.Second),
		FaultDuration:    c.GetDuration("stress.backing_disk_full.fault_duration", 90*time.Second),
		StallLimit:       c.GetDuration("stress.backing_disk_full.stall_limit", 60*time.Second),
		KeepFreeMiB:      c.GetInt("stress.backing_disk_full.keep_free_mib", 16),
		Fill:             c.GetString("stress.backing_disk_full.fill", "all"),
		SettleTimeout:    c.GetDuration("stress.backing_disk_full.settle_timeout", 10*time.Minute),
		ResumeDuration:   c.GetDuration("stress.backing_disk_full.resume_duration", 30*time.Second),
		PayloadBytes:     c.GetInt("stress.backing_disk_full.payload_bytes", 256),
	}
}

// fillerName is the file the filler leaves on each data disk; the suffix keeps
// two runs on one backing from freeing each other's file.
func fillerName(t *core.T) string { return "ampqa_fill_" + t.ResourceSuffix() }

// fillScript grows one file per data dir until keep MiB are left. Data dirs
// come from the tserver's --fs_data_dirs, else the chart's /mnt/disk* mounts.
func fillScript(file string, keepMiB int) string {
	return fmt.Sprintf(`set -e
dirs=$(tr '\0' '\n' < /proc/$(pgrep -xo yb-tserver || pgrep -xo yugabyted || echo 1)/cmdline 2>/dev/null | sed -n 's/^--fs_data_dirs=//p' | tr ',' ' ')
[ -n "$dirs" ] || dirs=$(awk '$2 ~ /^\/mnt\/disk/ {print $2}' /proc/mounts)
[ -n "$dirs" ] || { echo "no data dir found" >&2; exit 3; }
for d in $dirs; do
  avail=$(df -Pk "$d" | awk 'NR==2{print $4}')
  want=$((avail - %d*1024))
  [ "$want" -gt 1024 ] || { echo "skip $d avail=${avail}k"; continue; }
  fallocate -l "${want}k" "$d/%s" || dd if=/dev/zero of="$d/%s" bs=1M count=$((want/1024)) status=none
  echo "filled $d by ${want}k, left $(df -Pk "$d" | awk 'NR==2{print $4}')k"
done`, keepMiB, file, file)
}

func freeScript(file string) string {
	return fmt.Sprintf(`for d in $(tr '\0' '\n' < /proc/$(pgrep -xo yb-tserver || pgrep -xo yugabyted || echo 1)/cmdline 2>/dev/null | sed -n 's/^--fs_data_dirs=//p' | tr ',' ' ') $(awk '$2 ~ /^\/mnt\/disk/ {print $2}' /proc/mounts); do rm -f "$d/%s"; done; exit 0`, file)
}

// stallStats is what the bounded writers saw while the disks were full.
type stallStats struct {
	acked, errored, hung int64
	firstErr             atomic.Value
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

	if err := t.Step("fill_backing_disks", func(s *core.StepHandle) error {
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

	if err := t.Step("writes_answer_on_full_disk", func(s *core.StepHandle) error {
		st := boundedWriters(t.Context, env, fleet.Tenants, journals, params.FaultDuration, params.StallLimit, params.PayloadBytes)
		s.Set("acked", st.acked)
		s.Set("errored", st.errored)
		s.Set("hung", st.hung)
		if e, ok := st.firstErr.Load().(string); ok {
			s.Set("first_error", e)
		}
		if st.hung > 0 {
			return fmt.Errorf("%d INSERT(s) neither committed nor errored within %s while the backing disks were full (%d acked, %d errored): a full backing parks COMMIT forever instead of refusing the write (meko#727)",
				st.hung, params.StallLimit, st.acked, st.errored)
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

	if err := t.Step("free_backing_disks", func(s *core.StepHandle) error {
		free()
		waited, err := awaitBackingReady(t, kube, params.SettleTimeout)
		if err != nil {
			return err
		}
		s.Set("ready_after", waited.Round(time.Second).String())
		return nil
	}); err != nil {
		return err
	}

	if err := t.Step("writes_flow_again", func(s *core.StepHandle) error {
		waited, err := awaitWritePath(t.Context, env, fleet.Tenants, params.SettleTimeout, params.PayloadBytes)
		if err != nil {
			return fmt.Errorf("the write path never came back after the disks were freed: %w", err)
		}
		s.Set("write_path_back_after", waited.Round(time.Second).String())
		st := boundedWriters(t.Context, env, fleet.Tenants, journals, params.ResumeDuration, params.StallLimit, params.PayloadBytes)
		s.Set("rows_after_free", st.acked)
		if st.hung > 0 {
			return fmt.Errorf("%d INSERT(s) exceeded %s after the disks were freed and the backing reported ready", st.hung, params.StallLimit)
		}
		if st.acked == 0 {
			return fmt.Errorf("no tenant committed a row in the %s after the disks were freed", params.ResumeDuration)
		}
		return nil
	}); err != nil {
		return err
	}

	return t.Step("verify_exact_sets", func(s *core.StepHandle) error {
		return verifyExactSets(t, s, env, fleet.Tenants, journals, params.PayloadBytes)
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
				_, err := ten.Conn.Exec(stmtCtx, fmt.Sprintf(
					"INSERT INTO %s (id, v, pad) VALUES ($1, $2, repeat('t', $3::int)) ON CONFLICT (id) DO NOTHING", ten.Table),
					id, value, payloadBytes)
				stmtCancel()
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
