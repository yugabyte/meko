package ybastack

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"syscall"
	"time"

	"github.com/yugabyte/amp/test/functional/framework/core"
)

// How the deployed YBA is reached, and what to do when the direct address is a
// lie.
//
// The tailnet path assumes a cluster-internal address (ClusterIP, else the pod
// IP) is routable from the coordinator. On amp-dev it is not: the pod CIDR
// 10.21.128.0/17 sits inside a 10.21.0.0/16 that is already advertised on the
// tailnet for an unrelated network, so every dial of a pod IP TIMES OUT rather
// than being refused. Unprobed, that address was handed straight to bootstrap,
// which then spent its whole 10-minute budget on an address nothing was ever
// going to answer — and the caller quietly fell back to a single-node
// yugabyted. Three "YBA RF=3" runs measured single-node yugabyted that way.
//
// So the pod IP is probed like the ClusterIP already was, and when neither
// answers the platform is reached over a kubectl port-forward — the same
// fallback ampstack's controller has. The forward outlives Deploy (its process
// is background-rooted; only Close stops it) because the universe provisioning
// that follows talks to the same URL for the next half hour.

const (
	// directProbeTimeout bounds ONE probe of a candidate address. It is short on
	// purpose: an unroutable address times out rather than refusing, and the
	// point of probing is to find that out in seconds instead of discovering it
	// via the bootstrap deadline.
	directProbeTimeout = 5 * time.Second
	// directProbeAttempts rides out the gap between helm reporting the release
	// deployed and yugaware actually answering, without turning an unroutable
	// address into minutes of waiting.
	directProbeAttempts = 3
	directProbeDelay    = 3 * time.Second
	// reforwardAttempts rides out a yugaware pod that is still being rescheduled.
	reforwardAttempts = 4
	reforwardDelay    = 5 * time.Second
	reforwardCooldown = 3 * time.Second
)

// reachPlatform decides the URL every later call uses: the load-balancer ingress
// in loadbalancer mode, else a probed direct address, else a port-forward.
func (d *Deployer) reachPlatform(ctx context.Context) (string, error) {
	if d.cfg.Reach == ReachLoadBalancer {
		return d.loadBalancerURL(ctx)
	}
	direct, directErr := d.resolveDirectURL(ctx)
	if directErr == nil {
		return direct, nil
	}
	d.log("ybastack: %v; continuing over a kubectl port-forward", directErr)
	fwd, err := d.forwardPlatform(ctx)
	if err != nil {
		return "", core.AsEnvFailure(fmt.Errorf("yba unreachable directly (%v) and the port-forward fallback failed: %w", directErr, err))
	}
	return fwd, nil
}

// log is logf with the nil-logger case folded in.
func (d *Deployer) log(format string, args ...any) {
	if d.logf != nil {
		d.logf(format, args...)
	}
}

// loadBalancerURL reads the Service's external ingress IP (the stress path).
func (d *Deployer) loadBalancerURL(ctx context.Context) (string, error) {
	ip, err := d.kube.Get(ctx, "svc", d.serviceName(), "{.status.loadBalancer.ingress[0].ip}")
	if err != nil {
		return "", fmt.Errorf("read load-balancer ip of svc %s: %w", d.serviceName(), err)
	}
	if ip = strings.TrimSpace(ip); ip == "" {
		return "", fmt.Errorf("svc %s has no load-balancer ingress ip yet", d.serviceName())
	}
	return d.urlFor(ip), nil
}

// resolveDirectURL returns a cluster-internal URL that ANSWERED — the ClusterIP
// when it does, else the pod IP on the Service's targetPort. The error names
// every address tried, because the caller logs it before port-forwarding.
func (d *Deployer) resolveDirectURL(ctx context.Context) (string, error) {
	clusterIP := "<unresolved>"
	if ip, err := d.kube.Get(ctx, "svc", d.serviceName(), "{.spec.clusterIP}"); err == nil {
		if ip = strings.TrimSpace(ip); ip != "" && ip != "None" {
			clusterIP = ip
			if d.probe(ctx, d.urlFor(ip)) {
				return d.urlFor(ip), nil
			}
		}
	}
	podIP, err := d.kube.GetSelector(ctx, "pod", d.podSelector(), "{.items[0].status.podIP}")
	if err != nil {
		return "", fmt.Errorf("yba ClusterIP %s unreachable; pod IP lookup failed: %w", clusterIP, err)
	}
	if podIP = strings.TrimSpace(podIP); podIP == "" {
		return "", fmt.Errorf("yba ClusterIP %s unreachable and no yugaware pod found (selector %s)", clusterIP, d.podSelector())
	}
	// The pod listens on the Service's targetPort (the container port), not the
	// Service port: the Service VIP maps one to the other but is not routable
	// from outside the cluster, so the direct-to-pod path must use targetPort.
	port := d.podTargetPort(ctx)
	podURL := fmt.Sprintf("http://%s:%d", podIP, port)
	if !d.probe(ctx, podURL) {
		return "", fmt.Errorf("yba not reachable at ClusterIP %s nor pod IP %s:%d", clusterIP, podIP, port)
	}
	return podURL, nil
}

// forwardPlatform opens (or replaces) the kubectl port-forward to the yugaware
// Service and returns its loopback URL. The handle is kept for Teardown.
func (d *Deployer) forwardPlatform(ctx context.Context) (string, error) {
	pf, err := d.kube.PortForward(ctx, "svc/"+d.serviceName(), d.cfg.LocalPort, d.cfg.ServicePort)
	if err != nil {
		return "", fmt.Errorf("port-forward svc/%s: %w", d.serviceName(), err)
	}
	d.mu.Lock()
	if old := d.pf; old != nil {
		_ = old.Close()
	}
	d.pf = pf
	d.mu.Unlock()
	return "http://" + pf.LocalAddr(), nil
}

// forwardHost returns the live loopback host of the forward, re-establishing it
// when it has died (or when force says to). Empty when the platform was reached
// directly and there is nothing to revive.
func (d *Deployer) forwardHost(ctx context.Context, force bool) (string, error) {
	d.mu.Lock()
	pf := d.pf
	d.mu.Unlock()
	if pf == nil {
		return "", fmt.Errorf("yba was not reached over a port-forward")
	}
	// A break an earlier request reported is repaired before this one is sent.
	if !force && !d.forwardBroken.Load() && pf.Alive() {
		return pf.LocalAddr(), nil
	}
	// Serialised, then re-checked: concurrent pollers all see the one broken
	// tunnel, and whoever waited here takes the relaunch the first one did.
	d.reforwardMu.Lock()
	defer d.reforwardMu.Unlock()
	d.mu.Lock()
	fresh := time.Since(d.lastReforward) < reforwardCooldown
	addr := d.pf.LocalAddr()
	d.mu.Unlock()
	if fresh {
		return addr, nil
	}
	if _, err := d.forwardPlatform(ctx); err != nil {
		return "", err
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	d.lastReforward = time.Now()
	d.forwardBroken.Store(false)
	return d.pf.LocalAddr(), nil
}

// forwarded reports whether the platform is being reached over a port-forward.
func (d *Deployer) forwarded() bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.pf != nil
}

// probe reports whether the YBA at url answers, through the injected probe when
// one is wired (tests) or over HTTP.
func (d *Deployer) probe(ctx context.Context, url string) bool {
	if d.probeURL != nil {
		return d.probeURL(ctx, url)
	}
	return d.probeHTTP(ctx, url)
}

// probeHTTP reports whether the YBA at url answers its version endpoint, giving
// it a few short attempts.
func (d *Deployer) probeHTTP(ctx context.Context, url string) bool {
	for attempt := 0; ; attempt++ {
		pctx, cancel := context.WithTimeout(ctx, directProbeTimeout)
		_, err := d.newClient(url).AppVersion(pctx)
		cancel()
		if err == nil {
			return true
		}
		if attempt >= directProbeAttempts-1 {
			return false
		}
		select {
		case <-ctx.Done():
			return false
		case <-time.After(directProbeDelay):
		}
	}
}

// revivingTransport keeps a client usable across a broken kubectl port-forward,
// in either shape: the process exits and every dial is refused, or the pod goes
// away while kubectl keeps listening and every request is accepted and then
// EOFs. The second never heals on its own — the 0.260902.0 stress run lost
// backing_maintenance and then backing_nemesis to one forward, 15 minutes apart.
// The host is resolved per request, so a break costs a re-forward and a retry.
//
// What may be REPLAYED is narrower than what triggers a re-forward. A refused
// dial proves YBA never saw the request, so any method can go again. An EOF does
// not: the connection was established and the request may have been delivered,
// and this transport also carries YBAMaintainer's restart, gflag and software
// upgrades and the backup/restore/PITR creates, none of which YBA dedupes. Those
// are therefore rebuilt-then-failed rather than replayed, and the break is
// remembered so the NEXT request rebuilds the forward before it is sent — which
// is what a test opening on a forward an earlier test broke actually needs.
//
// It deliberately does NOT retry on timeout: a timeout is the unroutable-address
// case, which re-forwarding cannot fix and which must surface as itself.
type revivingTransport struct {
	base       http.RoundTripper
	host       func(ctx context.Context, force bool) (string, error)
	markBroken func()
	logf       func(string, ...any)
}

// NotReplayedError is a mutation the broken forward may or may not have
// delivered; only the caller can check which.
type NotReplayedError struct {
	Method, Path string
	Err          error
}

func (e *NotReplayedError) Error() string {
	return fmt.Sprintf("yba port-forward broke mid-request; %s %s may already have been applied and is not replayed: %v", e.Method, e.Path, e.Err)
}

func (e *NotReplayedError) Unwrap() error { return e.Err }

func (t *revivingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	res, err := t.attempt(req, false)
	if err == nil || !isDeadForward(err) {
		return res, err
	}
	if t.markBroken != nil {
		t.markBroken()
	}
	// Only a replayable body can be retried. http.NewRequest sets GetBody for
	// every in-memory body, which is all this client sends.
	if req.Body != nil && req.GetBody == nil {
		return res, core.AsEnvFailure(fmt.Errorf("%w (the yba port-forward is down and this request's body cannot be replayed)", err))
	}
	if !replayable(req.Method, err) {
		return res, core.AsEnvFailure(&NotReplayedError{Method: req.Method, Path: req.URL.Path, Err: err})
	}
	for attempt := 1; attempt <= reforwardAttempts; attempt++ {
		if t.logf != nil {
			t.logf("ybastack: the yba port-forward is broken (%v); re-establishing it and retrying %s %s (attempt %d/%d)",
				err, req.Method, req.URL.Path, attempt, reforwardAttempts)
		}
		res, err = t.attempt(req, true)
		if err == nil {
			return res, nil
		}
		// A forward that cannot be rebuilt yet is the pod still being rescheduled;
		// anything else that is not a broken forward is YBA's own answer.
		if !isDeadForward(err) && !isReforwardFailure(err) {
			return res, err
		}
		select {
		case <-req.Context().Done():
			return res, core.AsEnvFailure(fmt.Errorf("yba unreachable: the kubectl port-forward was still broken when the request context ended: %w", err))
		case <-time.After(reforwardDelay):
		}
	}
	return res, core.AsEnvFailure(fmt.Errorf("yba unreachable: the kubectl port-forward stayed broken across %d re-establish attempts: %w", reforwardAttempts, err))
}

// replayable reports whether the request may be sent again. A refused dial never
// reached YBA, so anything may; past that, only idempotent methods.
func replayable(method string, err error) bool {
	return isConnRefused(err) || method == http.MethodGet || method == http.MethodHead
}

func (t *revivingTransport) attempt(req *http.Request, force bool) (*http.Response, error) {
	host, err := t.host(req.Context(), force)
	if err != nil {
		return nil, core.AsEnvFailure(&reforwardFailure{err: fmt.Errorf("resolve yba address: %w", err)})
	}
	r := req.Clone(req.Context())
	if r.URL == nil {
		r.URL = &url.URL{}
	}
	r.URL.Host = host
	r.Host = ""
	if req.Body != nil && req.GetBody != nil {
		body, gerr := req.GetBody()
		if gerr != nil {
			return nil, fmt.Errorf("replay request body: %w", gerr)
		}
		r.Body = body
	}
	base := t.base
	if base == nil {
		base = http.DefaultTransport
	}
	return base.RoundTrip(r)
}

// reforwardFailure marks the forward itself as un-rebuildable right now — a
// yugaware pod with no ready endpoint behind it, which the next attempt may find.
type reforwardFailure struct{ err error }

func (e *reforwardFailure) Error() string { return e.err.Error() }
func (e *reforwardFailure) Unwrap() error { return e.err }

func isReforwardFailure(err error) bool {
	var rf *reforwardFailure
	return errors.As(err, &rf)
}

// isDeadForward reports a failure a relaunched forward can fix: a refused dial,
// or a connection accepted and dropped with no response. String matching backs
// up errors.Is because the error crosses url.Error and fmt-wrapped boundaries.
func isDeadForward(err error) bool {
	if err == nil {
		return false
	}
	if isConnRefused(err) {
		return true
	}
	if errors.Is(err, syscall.ECONNRESET) || errors.Is(err, syscall.EPIPE) ||
		errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return true
	}
	msg := strings.ToLower(err.Error())
	for _, m := range []string{"connection reset by peer", "broken pipe", "unexpected eof", ": eof", "server closed idle connection"} {
		if strings.Contains(msg, m) {
			return true
		}
	}
	return strings.HasSuffix(msg, "eof")
}

// isConnRefused reports a dial that never connected, which is the only failure
// this transport can prove YBA did not see.
func isConnRefused(err error) bool {
	if err == nil {
		return false
	}
	return errors.Is(err, syscall.ECONNREFUSED) || strings.Contains(strings.ToLower(err.Error()), "connection refused")
}
