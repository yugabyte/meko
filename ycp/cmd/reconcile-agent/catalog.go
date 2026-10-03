package main

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"time"

	"meko/ycp/internal/api"
)

// catalogResendEvery re-sends an unchanged catalog this often. Change detection is
// in-memory, so without it a control plane that lost its copy (a restore, a
// re-registration) would stay blank until the agent restarted.
const catalogResendEvery = time.Hour

// catalogRetryAfter spaces out attempts after a failure on either side. A controller
// that predates GET /extensions, or a control plane (or YBM passthrough) without the
// route, answers 404 on every try — and a control plane that serves it only as PUT
// answers 405 — which must not become one extra request and log line per tick.
const catalogRetryAfter = 5 * time.Minute

type catalogSource interface {
	ExtensionCatalog(ctx context.Context) ([]api.Extension, error)
}

type catalogSink interface {
	PostExtensions(ctx context.Context, cat api.ExtensionCatalog) error
}

// catalogRelay forwards the controller's extension catalog to the control plane
// when it changes. It is its own call rather than a field on the status heartbeat
// because the catalog changes rarely and the heartbeat goes out every tick.
//
// Not safe for concurrent use: it belongs to the single-threaded reconcile loop.
type catalogRelay struct {
	src  catalogSource
	dst  catalogSink
	now  func() time.Time
	logf func(string, ...any)

	sent     bool
	lastSent [sha256.Size]byte
	sentAt   time.Time
	retryAt  time.Time
}

func newCatalogRelay(src catalogSource, dst catalogSink, logf func(string, ...any)) *catalogRelay {
	return &catalogRelay{src: src, dst: dst, now: time.Now, logf: logf}
}

// Sync sends the catalog if it differs from the last one the control plane
// accepted, or that one is older than catalogResendEvery. Failures are logged, not
// returned: the catalog is additive and must not fail a tick.
func (r *catalogRelay) Sync(ctx context.Context) {
	now := r.now()
	if now.Before(r.retryAt) {
		return
	}
	cat, err := r.src.ExtensionCatalog(ctx)
	if err != nil {
		r.retryAt = now.Add(catalogRetryAfter)
		r.logf("read amp extension catalog (retrying in %s): %v", catalogRetryAfter, err)
		return
	}
	raw, err := json.Marshal(cat)
	if err != nil {
		r.logf("encode amp extension catalog: %v", err)
		return
	}
	sum := sha256.Sum256(raw)
	if r.sent && sum == r.lastSent && now.Sub(r.sentAt) < catalogResendEvery {
		return
	}
	if err := r.dst.PostExtensions(ctx, api.ExtensionCatalog{Extensions: cat}); err != nil {
		r.retryAt = now.Add(catalogRetryAfter)
		r.logf("relay amp extension catalog (retrying in %s): %v", catalogRetryAfter, err)
		return
	}
	r.sent, r.lastSent, r.sentAt = true, sum, now
}
