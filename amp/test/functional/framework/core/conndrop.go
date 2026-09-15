package core

import (
	"context"
	"errors"
	"time"

	"github.com/yugabyte/amp/test/functional/framework/apps/pg"
)

// PGConnDropAttester verifies a mid-statement connection drop was the harness's
// transport (kubectl port-forward), not the product: ok only when the same
// compute pod survived with no container restarts, the same postmaster is
// still running (no in-place respawn), and postgres accepts again. It is keyed
// on addr, the dropped connection's host:port.
type PGConnDropAttester interface {
	AttestPGConnDrop(ctx context.Context, addr string) (reason string, ok bool)
}

// IsConnDrop is true only for errors the pg client itself marked as a severed
// connection (pg.TransportError). A body error that merely quotes such text,
// e.g. from a workload log, does not qualify.
func IsConnDrop(err error) bool {
	var te *pg.TransportError
	return errors.As(err, &te)
}

const connDropAttestTimeout = 2 * time.Minute

// Own bounded context, deliberately detached: the attestation is post-mortem
// evidence gathering and should finish even when the run is being aborted.
func attestConnDrop(env Environment, err error) (string, bool) {
	var te *pg.TransportError
	if !errors.As(err, &te) || te.Addr == "" {
		return "", false
	}
	a, ok := env.(PGConnDropAttester)
	if !ok {
		return "", false
	}
	ctx, cancel := context.WithTimeout(context.Background(), connDropAttestTimeout)
	defer cancel()
	return a.AttestPGConnDrop(ctx, te.Addr)
}
