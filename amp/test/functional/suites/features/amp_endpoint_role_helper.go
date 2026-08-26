package features

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/yugabyte/amp/test/functional/framework/apps/controller"
)

const roleReadOnly = "read_only"

// Outside the endpoint_role enum.
const unknownRole = "read_sometimes"

// Bounds ONE create attempt, so a compute that never comes ready reports here
// instead of as the test's own deadline.
const roleEndpointReadyBudget = 4 * time.Minute

// createRoleEndpoint creates an endpoint of the given role, converging on the
// controller's non-idempotent create: create_endpoint can fail its helm deploy
// ("services yb-ep-… not found") yet leave a partial endpoint record whose
// deploy finishes moments later. So on a transient we back off and REUSE the
// role's endpoint if it is now present, rather than re-create — mirroring
// sharedk8s.createEndpoint.
func createRoleEndpoint(ctx context.Context, cl ampCtl, poolID, projID, branchID, role string) (controller.Endpoint, error) {
	var lastErr error
	for attempt := 0; attempt <= 5; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return controller.Endpoint{}, ctx.Err()
			case <-time.After(time.Duration(attempt) * 3 * time.Second):
			}
			if ep, ok := existingRoleEndpoint(ctx, cl, poolID, projID, branchID, role); ok {
				return ep, nil
			}
		}
		attemptCtx, cancel := context.WithTimeout(ctx, roleEndpointReadyBudget)
		_, err := cl.CreateEndpointAndWait(attemptCtx, poolID, projID, branchID, controller.CreateEndpointRequest{Role: role})
		cancel()
		if err != nil {
			lastErr = err
			if ctx.Err() == nil && attemptCtx.Err() != nil {
				return controller.Endpoint{}, fmt.Errorf("the %s endpoint's compute never became ready within %s", role, roleEndpointReadyBudget)
			}
			if transientDeploy(err) {
				continue
			}
			return controller.Endpoint{}, err
		}
		if ep, ok := existingRoleEndpoint(ctx, cl, poolID, projID, branchID, role); ok {
			return ep, nil
		}
		return controller.Endpoint{}, fmt.Errorf("create %s endpoint reported success but the branch lists no %s endpoint afterwards", role, role)
	}
	return controller.Endpoint{}, fmt.Errorf("create %s endpoint failed after retries: %w", role, lastErr)
}

func existingRoleEndpoint(ctx context.Context, cl ampCtl, poolID, projID, branchID, role string) (controller.Endpoint, bool) {
	eps, err := cl.ListAllEndpoints(ctx, poolID, projID, branchID)
	if err != nil {
		return controller.Endpoint{}, false
	}
	for i := len(eps) - 1; i >= 0; i-- {
		if eps[i].Role == role {
			return eps[i], true
		}
	}
	return controller.Endpoint{}, false
}

// A 5xx or a transport failure means the request never reached the validation
// under test, so only a 400 counts.
func isBadRequest(err error) bool {
	var apiErr *controller.APIError
	return errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusBadRequest
}

// transientDeploy reports the non-idempotent helm-deploy race ("services
// yb-ep-… not found") that clears once the deploy finishes.
func transientDeploy(err error) bool {
	s := err.Error()
	return strings.Contains(s, "services") && strings.Contains(s, "not found")
}

// pollUntil calls fn every interval until it returns true, the deadline passes,
// or fn errors.
func pollUntil(ctx context.Context, timeout, interval time.Duration, fn func() (bool, error)) error {
	deadline := time.Now().Add(timeout)
	for {
		ok, err := fn()
		if err != nil {
			return err
		}
		if ok {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("condition not met within %s", timeout)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(interval):
		}
	}
}
