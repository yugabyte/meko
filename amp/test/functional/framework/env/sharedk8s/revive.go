package sharedk8s

import (
	"fmt"
	"net/http"
	"net/url"

	"github.com/yugabyte/amp/test/functional/framework/core"
)

// revivingTransport keeps a controller client usable across the death of the
// kubectl port-forward it was created against.
//
// The forward is one local process shared by every parallel test, and a test is
// handed a client pinned to its local address. When the forward dies mid-test
// the pinned client keeps dialing a port nobody is listening on, so all tests in
// flight fail at once with `connection refused` — six of them in nightly
// 0.260810.0, while the controller pod itself was Running with zero restarts and
// had already accepted their work.
//
// So the host is resolved per request rather than baked in, and a connection
// error costs one re-forward and a retry instead of a run.
type revivingTransport struct {
	base http.RoundTripper
	// host returns the currently live "host:port" for the controller,
	// re-establishing the forward if needed.
	host func(force bool) (string, error)
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
		return res, fmt.Errorf("%w (controller port-forward is down and this request's body cannot be replayed)", err)
	}
	if t.logf != nil {
		t.logf("sharedk8s: controller port-forward refused the connection; re-establishing it and retrying %s %s", req.Method, req.URL.Path)
	}
	res2, err2 := t.attempt(req, true)
	if err2 != nil && isConnRefused(err2) {
		// Twice refused is the environment, not the product: say so, because the
		// bare error reads like the controller rejected the call.
		return res2, core.AsEnvFailure(fmt.Errorf("controller unreachable: the kubectl port-forward died and could not be re-established: %w", err2))
	}
	return res2, err2
}

// attempt rewrites the request at the live forward address and sends it.
func (t *revivingTransport) attempt(req *http.Request, force bool) (*http.Response, error) {
	host, err := t.host(force)
	if err != nil {
		return nil, core.AsEnvFailure(fmt.Errorf("resolve controller address: %w", err))
	}
	r := req.Clone(req.Context())
	if r.URL == nil {
		r.URL = &url.URL{}
	}
	r.URL.Host = host
	r.Host = ""
	if req.Body != nil && req.GetBody != nil {
		body, err := req.GetBody()
		if err != nil {
			return nil, fmt.Errorf("replay request body: %w", err)
		}
		r.Body = body
	}
	base := t.base
	if base == nil {
		base = http.DefaultTransport
	}
	res, err := base.RoundTrip(r)
	if err != nil && req.Body != nil {
		// The clone's body is spent either way; the caller retries via GetBody.
		_ = r.Body
	}
	return res, err
}
