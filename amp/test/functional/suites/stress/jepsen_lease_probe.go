// A direct probe of both computes during a lease race, so a fencing finding
// names the pod it happened on instead of resting on jepsen's history alone.
package stress

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/yugabyte/amp/test/functional/framework/apps/controller"
	"github.com/yugabyte/amp/test/functional/framework/apps/pg"
	"github.com/yugabyte/amp/test/functional/framework/core"
	"github.com/yugabyte/amp/test/functional/framework/exec/k8s"
)

// probeResult is one cycle's evidence about the thawed (old) pod.
type probeResult struct {
	Tenant int
	Cycle  int
	Old    string
	New    string
	// OldServing is false when the old pod's Postgres refused the session: it
	// fenced itself the strong way, and the rest of the probe is moot.
	OldServing bool
	// StaleReads counts reads the old pod answered without the marker the new
	// pod had already committed.
	StaleReads int
	// MarkerVisible is how long that marker took to show on the old pod;
	// negative means it never did within the probe.
	MarkerVisible time.Duration
	// OldWentDown is set when the old pod's Postgres dropped the session during
	// the probe: it fenced itself late.
	OldWentDown bool
	// OldWriteAccepted is the split-brain case: the old pod acknowledged a
	// write after the new one took the lease.
	OldWriteAccepted bool
	OldWriteErr      string
}

func (p probeResult) String() string {
	if !p.OldServing {
		return fmt.Sprintf("tenant %d cycle %d: old pod %s not serving (fenced)", p.Tenant, p.Cycle, p.Old)
	}
	vis := "never"
	if p.MarkerVisible >= 0 {
		vis = p.MarkerVisible.Round(100 * time.Millisecond).String()
	}
	if p.Stale() {
		vis += fmt.Sprintf(" (stale snapshot, %d read(s))", p.StaleReads)
	}
	if p.OldWentDown {
		vis += ", then the old pod dropped the session"
	}
	write := "refused: " + p.OldWriteErr
	if p.OldWriteAccepted {
		write = "ACCEPTED (split-brain write)"
	}
	return fmt.Sprintf("tenant %d cycle %d: old pod %s serving; marker from new pod %s visible after %s; old write %s",
		p.Tenant, p.Cycle, p.Old, p.New, vis, write)
}

// Stale reports the old pod answering reads from before the new pod's commit.
func (p probeResult) Stale() bool { return p.OldServing && p.MarkerVisible < 0 && p.StaleReads > 0 }

const leaseProbeTable = "ampqa_lease_probe"

// probeComputes writes a marker through newPod, looks for it on oldPod for up
// to wait, then tries a write on oldPod. An error is a probe that could not
// tell, never evidence either way.
func probeComputes(t *core.T, kube *k8s.Kubectl, tenant, cycle int, oldPod, newPod string, wait time.Duration) (probeResult, error) {
	res := probeResult{Tenant: tenant, Cycle: cycle, Old: oldPod, New: newPod, MarkerVisible: -1}
	newC, closeNew, err := podConn(t.Context, kube, newPod)
	if err != nil {
		return res, fmt.Errorf("connect the new pod %s: %w", newPod, err)
	}
	defer closeNew()
	if _, err := newC.Exec(t.Context, "CREATE TABLE IF NOT EXISTS "+leaseProbeTable+
		" (id bigserial PRIMARY KEY, cycle int NOT NULL, writer text NOT NULL, at timestamptz NOT NULL DEFAULT clock_timestamp())"); err != nil {
		return res, fmt.Errorf("create %s through the new pod: %w", leaseProbeTable, err)
	}
	marker, err := newC.QueryInt(t.Context, "INSERT INTO "+leaseProbeTable+" (cycle, writer) VALUES ($1, 'new') RETURNING id", cycle)
	if err != nil {
		return res, fmt.Errorf("write the marker through the new pod: %w", err)
	}
	committedAt := time.Now()

	oldC, closeOld, err := podConn(t.Context, kube, oldPod)
	if err != nil {
		if sessionRefused(err) {
			return res, nil
		}
		return res, fmt.Errorf("connect the old pod %s: %w", oldPod, err)
	}
	defer closeOld()
	res.OldServing = true
	deadline := committedAt.Add(wait)
	for time.Now().Before(deadline) && t.Context.Err() == nil {
		n, qerr := oldC.QueryInt(t.Context, "SELECT count(*) FROM "+leaseProbeTable+" WHERE id = $1", marker)
		switch {
		case qerr == nil && n == 1:
			res.MarkerVisible = time.Since(committedAt)
		case qerr == nil, pgCode(qerr) == "42P01":
			// A missing table is the same answer as a missing row: the old pod's
			// snapshot predates the new pod's commit.
			res.StaleReads++
		case sessionRefused(qerr):
			res.OldWentDown = true
		default:
			return res, fmt.Errorf("read the marker on the old pod %s: %w", oldPod, qerr)
		}
		if res.MarkerVisible >= 0 || res.OldWentDown {
			break
		}
		time.Sleep(500 * time.Millisecond)
	}
	if res.OldWentDown {
		res.OldWriteErr = "the session was already gone"
		return res, nil
	}

	wctx, cancel := context.WithTimeout(t.Context, 20*time.Second)
	defer cancel()
	_, werr := oldC.Exec(wctx, "SET statement_timeout = '15s'")
	if werr == nil {
		var id int64
		id, werr = oldC.QueryInt(wctx, "INSERT INTO "+leaseProbeTable+" (cycle, writer) VALUES ($1, 'old') RETURNING id", cycle)
		if werr == nil {
			res.OldWriteAccepted = true
			res.OldWriteErr = "row id " + strconv.FormatInt(id, 10)
		}
	}
	if werr != nil {
		res.OldWriteErr = strings.TrimSpace(werr.Error())
	}
	return res, nil
}

// pgCode is the SQLSTATE of a Postgres error, or "".
func pgCode(err error) string {
	var pe *pgconn.PgError
	if errors.As(err, &pe) {
		return pe.Code
	}
	return ""
}

// sessionRefused reports Postgres on the far side of a working port-forward
// refusing or dropping the session: a fenced compute. Anything else (kubectl,
// the port-forward, a timeout) is the probe failing, not the pod.
func sessionRefused(err error) bool {
	switch pgCode(err) {
	case "57P01", "57P02", "57P03": // admin_shutdown, crash_shutdown, cannot_connect_now
		return true
	}
	msg := err.Error()
	for _, s := range []string{"connection refused", "unexpected EOF", "connection reset by peer",
		"server closed the connection", "conn closed", "broken pipe"} {
		if strings.Contains(msg, s) {
			return true
		}
	}
	return false
}

// podConn opens a session to one pod's compute through its own port-forward,
// bypassing the Service that would pick a pod at random.
func podConn(ctx context.Context, kube *k8s.Kubectl, pod string) (*pg.Client, func(), error) {
	portS, err := kube.Get(ctx, "pod", pod, `{.spec.containers[*].ports[?(@.name=="pg-compute")].containerPort}`)
	if err != nil {
		return nil, nil, fmt.Errorf("read the pg-compute port of pod %s: %w", pod, err)
	}
	port, err := strconv.Atoi(strings.TrimSpace(portS))
	if err != nil {
		return nil, nil, fmt.Errorf("pod %s has no pg-compute port (%q)", pod, portS)
	}
	pf, err := kube.PortForward(ctx, "pod/"+pod, 0, port)
	if err != nil {
		return nil, nil, fmt.Errorf("port-forward to pod %s: %w", pod, err)
	}
	cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	info := pg.ConnInfo{Host: "127.0.0.1", Port: pf.LocalPort(), User: "cloud_admin",
		Password: controller.DefaultEndpointPassword, Database: "postgres"}
	c, err := pg.Connect(cctx, info)
	// A pod reports Ready a moment before Postgres finishes recovery and still
	// answers 57P03 "the database system is starting up".
	for err != nil && pgCode(err) == "57P03" && cctx.Err() == nil {
		time.Sleep(time.Second)
		c, err = pg.Connect(cctx, info)
	}
	if err != nil {
		_ = pf.Close()
		return nil, nil, err
	}
	return c, func() { c.Close(); _ = pf.Close() }, nil
}
