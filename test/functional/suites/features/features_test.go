package features

import (
	"context"
	"errors"
	"fmt"
	"net"
	"syscall"
	"testing"

	"github.com/yugabyte/amp/test/functional/framework/apps/ybbacking"
	"github.com/yugabyte/amp/test/functional/framework/core"
	"github.com/yugabyte/amp/test/functional/framework/env/reap"
)

// Only a refused connection may authorise a port-forward recovery. Everything
// else — a timeout, a reset, an auth failure, "the database system is starting
// up" — is the endpoint's problem and must reach the caller untouched, or a
// product outage would be retried into a pass.
func TestIsConnRefusedDiscriminates(t *testing.T) {
	refused := []error{
		syscall.ECONNREFUSED,
		fmt.Errorf("dial error: %w", syscall.ECONNREFUSED),
		&net.OpError{Op: "dial", Net: "tcp", Err: syscall.ECONNREFUSED},
		errors.New(`failed to connect to "host=127.0.0.1": dial tcp 127.0.0.1:38791: connect: connection refused`),
	}
	for _, err := range refused {
		if !isConnRefused(err) {
			t.Errorf("isConnRefused(%v) = false, want true", err)
		}
	}

	notRefused := []error{
		context.DeadlineExceeded,
		syscall.ECONNRESET,
		errors.New("server error: FATAL: the database system is starting up (SQLSTATE 57P03)"),
		errors.New(`failed to connect: password authentication failed for user "cloud_admin"`),
		errors.New("unexpected EOF"),
		errors.New("connection reset by peer"),
	}
	for _, err := range notRefused {
		if isConnRefused(err) {
			t.Errorf("isConnRefused(%v) = true, want false — a product failure must not be retried", err)
		}
	}
}

// A shapeProvisioner stands REAL infrastructure up: env failure, not a defect.
func TestProvisionBackingFailureIsEnvFailure(t *testing.T) {
	boom := errors.New("provision yb in k8s: helm install exited 1: context deadline exceeded")
	f := &flow{
		td:   &teardownStack{},
		logf: func(string, ...any) {},
		prov: func(context.Context, *core.T, *teardownStack, func(string, ...any)) (ybbacking.BackingStore, *reap.Plan, error) {
			return nil, nil, boom
		},
	}
	err := f.provisionBacking(context.Background())
	if !core.IsEnvFailure(err) {
		t.Errorf("provisionBacking error = %v, want it marked an env failure", err)
	}
	if !errors.Is(err, boom) || err.Error() != boom.Error() {
		t.Errorf("marking changed the error: %q", err)
	}
}
