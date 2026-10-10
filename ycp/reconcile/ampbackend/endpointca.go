package ampbackend

import (
	"context"
	"net/http"
	"strconv"
	"time"

	openapi_types "github.com/oapi-codegen/runtime/types"

	"meko/ycp/internal/ampclient"
	"meko/ycp/reconcile"
)

// heldCA is an endpoint's last GET …/ca answer, and the addresses it was read
// against. Each read costs the controller two Kubernetes GETs, so it is re-read on
// a timer rather than every tick. The timer is what catches a CA change: the
// controller reads ca.crt from the certificate Secret, which cert-manager rewrites
// on a rotation without the pod being re-rendered or its address moving.
type heldCA struct {
	bundle *ampclient.EndpointCaBundle // nil until a read succeeds
	readAt time.Time
	next   time.Time // when to read again
	// address and documentdb are the endpoint's addresses at the read. A new
	// address discards the answer: it is another pod. A new documentdb_address
	// keeps the CA half and re-reads at once, because documentdb_verifiable is
	// answered only where the gateway is reachable.
	address, documentdb string
}

const (
	caRefreshAfter = 5 * time.Minute
	caRetryAfter   = time.Minute
	// caStaleAfter is how long a held answer outlives failed refreshes. Past it
	// the CA attrs are dropped, which the console reads as "could not check" —
	// never as "not required", which is what a held false would keep claiming.
	caStaleAfter = 15 * time.Minute
)

// endpointCA returns the CA answer to report for an endpoint, or nil when there is
// none to trust: never read, read against other addresses, or too old. A failed
// read keeps the held answer rather than dropping it, so one transient error does
// not flip the console to an unverified connection string.
func (b *Backend) endpointCA(ctx context.Context, key reconcile.ResourceKey, poolID, clusterID openapi_types.UUID, ep ampclient.Endpoint) *ampclient.EndpointCaBundle {
	// Nothing to connect to yet, and the controller would answer for a pod that
	// does not exist.
	if ep.Address == "" {
		return nil
	}
	var documentdb string
	if ep.DocumentdbAddress != nil {
		documentdb = *ep.DocumentdbAddress
	}
	now := b.clock()
	b.mu.RLock()
	held, ok := b.lastCA[key]
	b.mu.RUnlock()
	switch {
	case !ok || held.address != ep.Address:
		held = heldCA{address: ep.Address, documentdb: documentdb}
	case held.documentdb != documentdb:
		// Held through a failed re-read like any other answer, so a gateway
		// becoming reachable cannot cost the endpoint its CA attrs.
		held.documentdb = documentdb
		if held.bundle != nil {
			ca := *held.bundle
			ca.DocumentdbVerifiable = nil
			held.bundle = &ca
		}
	case now.Before(held.next):
		return held.current(now)
	}

	var bundle ampclient.EndpointCaBundle
	var status int
	if err := b.getJSON(ctx, func(c context.Context) (*http.Response, error) {
		resp, err := b.api.GetEndpointCaBundle(c, poolID, clusterID, ep.BranchId, ep.Id)
		if resp != nil {
			status = resp.StatusCode
		}
		return resp, err
	}, &bundle); err != nil {
		held.next = now.Add(caRetryAfter)
		// A controller older than GET …/ca answers 404 for every endpoint, and
		// will not grow the route before it is upgraded.
		if status == http.StatusNotFound {
			held.next = now.Add(caRefreshAfter)
		}
	} else {
		held.bundle, held.readAt, held.next = &bundle, now, now.Add(caRefreshAfter)
	}
	b.mu.Lock()
	if b.lastCA == nil {
		b.lastCA = map[reconcile.ResourceKey]heldCA{}
	}
	b.lastCA[key] = held
	b.mu.Unlock()
	return held.current(now)
}

func (h heldCA) current(now time.Time) *ampclient.EndpointCaBundle {
	if h.bundle == nil || now.Sub(h.readAt) >= caStaleAfter {
		return nil
	}
	return h.bundle
}

// attrPutCA reports what a client needs to verify the endpoint's certificate. All
// of it is absent when ca is nil: absent means unknown, and a "false" written for
// a read that failed would hand the console a verified-looking string for a
// certificate nobody checked.
func attrPutCA(m map[string]string, ca *ampclient.EndpointCaBundle) map[string]string {
	if ca == nil {
		return m
	}
	m = attrSet(m, "tls_ca_required", strconv.FormatBool(ca.Required))
	// The issuing CA only — the controller never returns the leaf or its key.
	if ca.Required && ca.CaCrt != nil {
		m = attrPut(m, "tls_ca_crt", *ca.CaCrt)
	}
	if ca.DocumentdbVerifiable != nil {
		m = attrSet(m, "documentdb_verifiable", strconv.FormatBool(*ca.DocumentdbVerifiable))
	}
	return m
}
