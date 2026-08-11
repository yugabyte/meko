package ybastack

import (
	"context"
	"errors"
	"fmt"
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
	if !force && pf.Alive() {
		return pf.LocalAddr(), nil
	}
	if _, err := d.forwardPlatform(ctx); err != nil {
		return "", err
	}
	d.mu.Lock()
	defer d.mu.Unlock()
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

// revivingTransport keeps a client usable across the death of the kubectl
// port-forward it was created against. A forward is one local process; when it
// dies the pinned client keeps dialing a port nobody listens on, so a universe
// creation that is half an hour into its task polling fails on `connection
// refused` while YBA itself is healthy. The host is therefore resolved per
// request rather than baked in, and a refused connection costs one re-forward
// and a retry instead of the run.
//
// It deliberately does NOT retry on timeout: a timeout is the unroutable-address
// case, which re-forwarding cannot fix and which must surface as itself.
type revivingTransport struct {
	base http.RoundTripper
	host func(ctx context.Context, force bool) (string, error)
	logf func(string, ...any)
}

func (t *revivingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	res, err := t.attempt(req, false)
	if err == nil || !isConnRefused(err) {
		return res, err
	}
	// Only a replayable body can be retried. http.NewRequest sets GetBody for
	// every in-memory body, which is all this client sends.
	if req.Body != nil && req.GetBody == nil {
		return res, fmt.Errorf("%w (the yba port-forward is down and this request's body cannot be replayed)", err)
	}
	if t.logf != nil {
		t.logf("ybastack: the yba port-forward refused the connection; re-establishing it and retrying %s %s", req.Method, req.URL.Path)
	}
	res2, err2 := t.attempt(req, true)
	if err2 != nil && isConnRefused(err2) {
		return res2, core.AsEnvFailure(fmt.Errorf("yba unreachable: the kubectl port-forward died and could not be re-established: %w", err2))
	}
	return res2, err2
}

func (t *revivingTransport) attempt(req *http.Request, force bool) (*http.Response, error) {
	host, err := t.host(req.Context(), force)
	if err != nil {
		return nil, core.AsEnvFailure(fmt.Errorf("resolve yba address: %w", err))
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

// isConnRefused reports a refused TCP connection — a dead forward, not a YBA
// that answered badly. String matching backs up errors.Is because the error
// crosses url.Error and (in tests) fmt-wrapped boundaries.
func isConnRefused(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, syscall.ECONNREFUSED) {
		return true
	}
	return strings.Contains(err.Error(), "connection refused")
}
