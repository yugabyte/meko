package core

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"syscall"
	"time"

	"github.com/yugabyte/amp/test/functional/framework/apps/pg"
)

// PGEndpointEnv is an environment exposing a Postgres endpoint to connect to.
type PGEndpointEnv interface {
	PGEndpoint() pg.ConnInfo
}

// PGRefresher is an environment that can re-establish a DEAD port-forward to
// its endpoint. The env decides from the forward's own state and refuses when
// the plumbing is fine, so a product outage can never be papered over.
type PGRefresher interface {
	RefreshPGEndpoint(ctx context.Context) (pg.ConnInfo, error)
}

// ConnectPG opens a connection to the environment's endpoint, recovering from a
// dead port-forward exactly once.
//
// Every suite needs this, and for a long time only one of them had it: the
// recovery lived in the features package's own connect(), so a stress test whose
// endpoint pod was rescheduled — the very thing stress tests cause — reported
// ECONNREFUSED against a loopback port as a product failure. Recovery belongs
// where every caller can reach it.
//
// The rule it enforces: a refused connection to a forward THIS process owns is
// the only thing that authorises a rebuild. A loopback port with nothing
// listening cannot have carried an answer from the endpoint, so re-establishing
// it cannot hide a real fault — and the caller waits again afterwards, so one
// that is down still fails on its own terms. Anything else (postgres down,
// crash-looping, auth, resets) fails exactly as before.
func ConnectPG(t *T, env PGEndpointEnv, timeout time.Duration) (*pg.Client, error) {
	ci, err := ResolvePGEndpoint(t, env, timeout)
	if err != nil {
		return nil, err
	}
	return pg.Connect(t, ci)
}

// ResolvePGEndpoint waits for the environment's endpoint to answer — recovering
// a dead forward once, on the terms ConnectPG describes — and returns the
// connection info that actually works. Callers that need to vary the connection
// (a different database on the same endpoint) use this, then dial themselves.
func ResolvePGEndpoint(t *T, env PGEndpointEnv, timeout time.Duration) (pg.ConnInfo, error) {
	ci := env.PGEndpoint()
	err := pg.WaitReady(t, ci, timeout)
	if err != nil {
		r, canRefresh := env.(PGRefresher)
		if !canRefresh || !IsConnRefused(err) {
			return pg.ConnInfo{}, err
		}
		// Its own step, so a recovered forward is VISIBLE in the run instead of
		// being silently absorbed into a pass.
		if serr := t.Step("recover_pg_port_forward", func(s *StepHandle) error {
			fresh, rerr := r.RefreshPGEndpoint(t.Context)
			if rerr != nil {
				return fmt.Errorf("%w (port-forward not re-established: %v)", err, rerr)
			}
			s.Set("address", fmt.Sprintf("%s:%d", fresh.Host, fresh.Port))
			ci = fresh
			if werr := pg.WaitReady(t, ci, timeout); werr != nil {
				return fmt.Errorf("postgres still unreachable after re-establishing the port-forward: %w", werr)
			}
			return nil
		}); serr != nil {
			return pg.ConnInfo{}, serr
		}
	}
	return ci, nil
}

// IsConnRefused reports a refused TCP connection. errors.Is covers a properly
// wrapped syscall error; the string check covers drivers that stringify the
// cause on the way out.
func IsConnRefused(err error) bool {
	if err == nil {
		return false
	}
	return errors.Is(err, syscall.ECONNREFUSED) ||
		strings.Contains(err.Error(), "connection refused")
}
