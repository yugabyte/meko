package sharedk8s

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/yugabyte/amp/test/functional/framework/apps/pg"
)

// Short on purpose: a slowly-recovering product must not pass as env.
const connDropReadyTimeout = 30 * time.Second

// pgPodSnap pins the compute pod and its postmaster behind e.pg when postgres
// was last verified. The postmaster start time is what moves when the endpoint
// supervisor respawns a crashed postgres in place (pod uid and container
// restart count both stay put), so it is the check that keeps a real compute
// crash out of env_failed.
type pgPodSnap struct {
	name            string
	uid             string
	restarts        int
	postmasterStart string
}

// Best-effort: without a snapshot AttestPGConnDrop declines.
func (e *Env) capturePGPod(ctx context.Context) {
	if e.kube == nil {
		return
	}
	snap, err := e.endpointPodState(ctx)
	if err != nil {
		e.log("sharedk8s: pg pod snapshot for endpoint %s not captured: %v", e.endpointID, err)
		return
	}
	e.pgMu.Lock()
	ci := e.pg
	e.pgMu.Unlock()
	start, err := e.postmasterStart(ctx, ci)
	if err != nil {
		e.log("sharedk8s: postmaster start time for endpoint %s not captured: %v", e.endpointID, err)
		return
	}
	snap.postmasterStart = start
	e.pgMu.Lock()
	e.pgPod = &snap
	e.pgMu.Unlock()
}

func (e *Env) postmasterStart(ctx context.Context, ci pg.ConnInfo) (string, error) {
	if e.pgProbe != nil {
		return e.pgProbe(ctx, ci)
	}
	qctx, cancel := context.WithTimeout(ctx, connDropReadyTimeout)
	defer cancel()
	cl, err := pg.Connect(qctx, ci)
	if err != nil {
		return "", err
	}
	defer cl.Close()
	return cl.QueryString(qctx, "SELECT pg_postmaster_start_time()::text")
}

func (e *Env) endpointPodState(ctx context.Context) (pgPodSnap, error) {
	pod, err := e.computePodName(ctx)
	if err != nil {
		return pgPodSnap{}, err
	}
	out, err := e.kube.Get(ctx, "pod", pod,
		`{.metadata.uid} {.status.phase} {.status.containerStatuses[*].restartCount}`)
	if err != nil {
		return pgPodSnap{}, err
	}
	fields := strings.Fields(out)
	if len(fields) < 2 {
		return pgPodSnap{}, fmt.Errorf("pod %s: unexpected state %q", pod, out)
	}
	if fields[1] != "Running" {
		return pgPodSnap{}, fmt.Errorf("pod %s is %s, not Running", pod, fields[1])
	}
	restarts := 0
	for _, f := range fields[2:] {
		n, err := strconv.Atoi(f)
		if err != nil {
			return pgPodSnap{}, fmt.Errorf("pod %s: restart count %q: %w", pod, f, err)
		}
		restarts += n
	}
	return pgPodSnap{name: pod, uid: fields[0], restarts: restarts}, nil
}

// AttestPGConnDrop testifies only when every check passes; any doubt keeps the
// product verdict. The declined paths log why, so the run record explains the
// verdict either way.
func (e *Env) AttestPGConnDrop(ctx context.Context) (string, bool) {
	e.pgMu.Lock()
	owned := len(e.pfs) > 0
	snap := e.pgPod
	ci := e.pg
	e.pgMu.Unlock()
	if !owned || snap == nil || e.kube == nil {
		return "", false
	}
	cur, err := e.endpointPodState(ctx)
	if err != nil {
		e.log("sharedk8s: conn-drop attestation declined: %v", err)
		return "", false
	}
	if cur.uid != snap.uid || cur.restarts > snap.restarts {
		e.log("sharedk8s: conn-drop attestation declined: pod %s changed (uid %s -> %s, restarts %d -> %d)",
			cur.name, snap.uid, cur.uid, snap.restarts, cur.restarts)
		return "", false
	}
	rebuilt := false
	if !pgForwardAccepts(ctx, ci) {
		fresh, rerr := e.RefreshPGEndpoint(ctx)
		if rerr != nil {
			e.log("sharedk8s: conn-drop attestation declined: forward rebuild failed: %v", rerr)
			return "", false
		}
		ci = fresh
		rebuilt = true
		e.log("sharedk8s: conn-drop attestation rebuilt the dead pg forward for endpoint %s (now %s)", e.endpointID, ci.Addr())
	}
	if err := pg.WaitReady(ctx, ci, connDropReadyTimeout); err != nil {
		e.log("sharedk8s: conn-drop attestation declined: postgres not answering: %v", err)
		return "", false
	}
	start, err := e.postmasterStart(ctx, ci)
	if err != nil || start == "" || start != snap.postmasterStart {
		e.log("sharedk8s: conn-drop attestation declined: postmaster start time %q vs snapshot %q (err=%v), postgres restarted in place",
			start, snap.postmasterStart, err)
		return "", false
	}
	detail := ""
	if rebuilt {
		detail = ", forward rebuilt"
	}
	return fmt.Sprintf(
		"env: connection dropped mid-statement but compute pod %s survived it (uid unchanged, %d container restarts, same postmaster since %s) and postgres accepts again%s: harness port-forward drop, not a product failure",
		cur.name, cur.restarts, snap.postmasterStart, detail), true
}
