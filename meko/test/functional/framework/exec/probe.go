package exec

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// HasCommand reports whether bin is on the target's PATH. The probe echoes its
// own exit status, so an answer only counts when the shell inside the target
// printed it: a kubectl exec that failed in transit also exits non-zero, and
// perf/sysbench reported "sysbench not found" mid-run on a pod that had run
// sysbench a minute earlier (run b0aa0ca4). Transport failures are retried.
func HasCommand(ctx context.Context, ex RemoteExecutor, bin string) (bool, error) {
	var last error
	for attempt := 0; attempt < 3; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return false, ctx.Err()
			case <-time.After(2 * time.Second):
			}
		}
		res, err := ex.Run(ctx, fmt.Sprintf("command -v %s >/dev/null 2>&1; echo probe-rc=$?", bin))
		switch out := res.Stdout; {
		case err == nil && strings.Contains(out, "probe-rc=0"):
			return true, nil
		case err == nil && strings.Contains(out, "probe-rc="):
			return false, nil
		case err != nil:
			last = err
		default:
			last = fmt.Errorf("exit %d: %s", res.ExitCode, strings.TrimSpace(res.Stderr))
		}
	}
	return false, fmt.Errorf("probe for %s never reached the target: %w", bin, last)
}
