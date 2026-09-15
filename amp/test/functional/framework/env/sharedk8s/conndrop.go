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
	snap, err := e.endpointPodStateFor(ctx, e.endpointID)
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

func (e *Env) waitPGReady(ctx context.Context, ci pg.ConnInfo) error {
	if e.pgReady != nil {
		return e.pgReady(ctx, ci)
	}
	return pg.WaitReady(ctx, ci, connDropReadyTimeout)
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

func (e *Env) endpointPodStateFor(ctx context.Context, id string) (pgPodSnap, error) {
	pod, err := e.computePodNameFor(ctx, id)
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
// product verdict. addr names the connection that died, so a drop on a tenant
// endpoint from ConnectEndpoint is judged against THAT endpoint's pod and never
// against the env's own idle one. The declined paths log why.
func (e *Env) AttestPGConnDrop(ctx context.Context, addr string) (string, bool) {
	e.pgMu.Lock()
	owned := len(e.pfs) > 0
	snap := e.pgPod
	ci := e.pg
	own := addr == ci.Addr() || e.ownAddrs[addr]
	e.pgMu.Unlock()
	if e.kube == nil {
		return "", false
	}
	if !own {
		return e.attestExtra(ctx, addr)
	}
	if !owned || snap == nil {
		return "", false
	}
	cur, err := e.endpointPodStateFor(ctx, e.endpointID)
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
	if !e.pgAliveSince(ctx, ci, snap.postmasterStart) {
		return "", false
	}
	detail := ""
	if rebuilt {
		detail = ", forward rebuilt"
	}
	return attestation(cur.name, cur.restarts, snap.postmasterStart, detail), true
}

// A co-tenant may still be using the recorded forward, so a rebuild here probes
// over a throwaway one instead of swapping it.
func (e *Env) attestExtra(ctx context.Context, addr string) (string, bool) {
	entry := e.extraEndpointFor(addr)
	if entry == nil || entry.snap == nil {
		e.log("sharedk8s: conn-drop attestation declined: no pg pod baseline for %s", addr)
		return "", false
	}
	cur, err := e.endpointPodStateFor(ctx, entry.ep.ID)
	if err != nil {
		e.log("sharedk8s: conn-drop attestation declined for %s: %v", addr, err)
		return "", false
	}
	if cur.uid != entry.snap.uid || cur.restarts > entry.snap.restarts {
		e.log("sharedk8s: conn-drop attestation declined: endpoint %s pod %s changed (uid %s -> %s, restarts %d -> %d)",
			entry.ep.ID, cur.name, entry.snap.uid, cur.uid, entry.snap.restarts, cur.restarts)
		return "", false
	}
	ci, detail := entry.ci, ""
	if !pgForwardAccepts(ctx, ci) {
		if e.prov == nil {
			e.log("sharedk8s: conn-drop attestation declined for %s: no provider to re-resolve endpoint %s", addr, entry.ep.ID)
			return "", false
		}
		fresh, pf, rerr := e.prov.resolvePG(ctx, e.kube, entry.ep)
		if rerr != nil {
			e.log("sharedk8s: conn-drop attestation declined for %s: re-resolve endpoint %s: %v", addr, entry.ep.ID, rerr)
			return "", false
		}
		if pf != nil {
			defer func() { _ = pf.Close() }()
		}
		ci = fresh
		detail = ", probed over a fresh forward"
	}
	if !e.pgAliveSince(ctx, ci, entry.snap.postmasterStart) {
		return "", false
	}
	return attestation(cur.name, cur.restarts, entry.snap.postmasterStart, detail), true
}

func (e *Env) pgAliveSince(ctx context.Context, ci pg.ConnInfo, want string) bool {
	if err := e.waitPGReady(ctx, ci); err != nil {
		e.log("sharedk8s: conn-drop attestation declined: postgres not answering: %v", err)
		return false
	}
	start, err := e.postmasterStart(ctx, ci)
	if err != nil || start == "" || start != want {
		e.log("sharedk8s: conn-drop attestation declined: postmaster start time %q vs snapshot %q (err=%v), postgres restarted in place",
			start, want, err)
		return false
	}
	return true
}

// The closing phrase is matched verbatim by core/envfail.go's scheduling signal.
func attestation(pod string, restarts int, since, detail string) string {
	return fmt.Sprintf(
		"env: connection dropped mid-statement but compute pod %s survived it (uid unchanged, %d container restarts, same postmaster since %s) and postgres accepts again%s: harness port-forward drop, not a product failure",
		pod, restarts, since, detail)
}
