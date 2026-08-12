package features

import (
	"context"
	"errors"
	"testing"

	"github.com/yugabyte/amp/test/functional/framework/apps/ybbacking"
	"github.com/yugabyte/amp/test/functional/framework/core"
	"github.com/yugabyte/amp/test/functional/framework/env/reap"
)

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
