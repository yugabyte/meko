package features

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/yugabyte/amp/test/functional/framework/apps/controller"
)

// forkRetryBudget bounds waiting out a parent whose safekeeper API has not come
// up yet. The endpoint is already deployed and Ready by the time a caller gets
// here; what is left is the daemon inside it binding its port.
const (
	forkRetryBudget   = 3 * time.Minute
	forkRetryInterval = 5 * time.Second
)

// createBranchRetrying forks and consumes the one 503 the fork API defines: a
// parent whose endpoint is not yet serving its safekeeper API, which the
// controller tells the caller to retry (ac-apis.md, meko#724). Every other
// answer, including a 400 verdict, is returned as it is — the distinction
// between "retry" and "no" is the contract, and swallowing the second would
// turn a refusal into a timeout.
func createBranchRetrying(ctx context.Context, cl *controller.Client, poolID, clusterID string, req controller.CreateBranchRequest) error {
	deadline := time.Now().Add(forkRetryBudget)
	for attempt := 1; ; attempt++ {
		_, err := cl.CreateBranchAndWait(ctx, poolID, clusterID, req)
		if err == nil || !forkParentNotReady(err) || time.Now().After(deadline) {
			if err != nil && forkParentNotReady(err) {
				return fmt.Errorf("the parent branch's safekeeper API was still not serving after %s and %d attempts, so the fork never became possible: %w", forkRetryBudget, attempt, err)
			}
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(forkRetryInterval):
		}
	}
}

// forkParentNotReady is the retryable fork 503, as opposed to a 503 from an
// unreachable state store (which the client retries itself) or any 4xx verdict.
func forkParentNotReady(err error) bool {
	if apiErrorStatus(err) != http.StatusServiceUnavailable {
		return false
	}
	return strings.Contains(err.Error(), "safekeeper API")
}
