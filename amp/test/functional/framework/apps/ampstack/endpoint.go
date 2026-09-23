package ampstack

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/yugabyte/amp/test/functional/framework/exec/k8s"
)

// EndpointWorkloadName is the controller's deploy/release name for an endpoint:
// the endpoint UUID prefixed with "yb-ep-". The endpoint's workload object, its
// Service, and its pods all carry this name, and the pods carry it as the
// app.kubernetes.io/instance label.
func EndpointWorkloadName(endpointID string) string { return "yb-ep-" + endpointID }

// WaitEndpointWorkloadReady blocks until the endpoint's compute workload reports
// ready, whichever workload kind the controller deployed it as.
//
// A Deployment is waited on with condition=Available — the historical behaviour,
// and the only call made when the endpoint is a Deployment. A bare ReplicaSet has
// no conditions at all, so its equivalent gate is readyReplicas; the kind is
// resolved from the Deployment wait's own not-found result rather than a
// pre-probe, so the Deployment path stays a single kubectl call.
func WaitEndpointWorkloadReady(ctx context.Context, kube *k8s.Kubectl, endpointID string, timeout time.Duration) error {
	name := EndpointWorkloadName(endpointID)
	err := kube.WaitFor(ctx, "deployment/"+name, "condition=Available", timeout)
	if err == nil || !isNotFound(err) {
		return err
	}
	if rsErr := kube.WaitFor(ctx, "replicaset/"+name, "jsonpath={.status.readyReplicas}=1", timeout); rsErr != nil {
		return fmt.Errorf("endpoint %s has no ready compute workload: no Deployment %s (%v) and ReplicaSet %s never reported a ready replica: %w",
			endpointID, name, err, name, rsErr)
	}
	return nil
}

// EndpointPodName names an endpoint's compute pod, preferring a Running one: a rolled endpoint leaves a Terminating pod on the same selector.
func EndpointPodName(ctx context.Context, kube *k8s.Kubectl, endpointID string) (string, error) {
	sel := "app.kubernetes.io/instance=" + EndpointWorkloadName(endpointID)
	running, err := kube.GetSelector(ctx, "pod", sel, `{.items[?(@.status.phase=="Running")].metadata.name}`)
	if err != nil {
		return "", err
	}
	if f := strings.Fields(running); len(f) > 0 {
		return f[0], nil
	}
	pod, err := kube.GetSelector(ctx, "pod", sel, "{.items[0].metadata.name}")
	if err != nil {
		return "", err
	}
	if pod = strings.TrimSpace(pod); pod == "" {
		return "", fmt.Errorf("no pod matched %s", sel)
	}
	return pod, nil
}

type EndpointAuth struct {
	Host     string
	Port     int
	User     string
	Password string
	Database string
}

const (
	endpointContainer        = "endpoint"
	endpointAuthProbeTimeout = 30 * time.Second
)

var endpointAuthPoll = 5 * time.Second

func WaitEndpointAuth(ctx context.Context, kube *k8s.Kubectl, endpointID string, a EndpointAuth, timeout time.Duration, logf func(string, ...any)) error {
	deadline := time.Now().Add(timeout)
	probe := psqlProbe(a, "select 1")
	var last error
	for attempt := 0; ; attempt++ {
		if err := ctx.Err(); err != nil {
			return errors.Join(err, last)
		}
		pod, err := EndpointPodName(ctx, kube, endpointID)
		if err != nil {
			last = fmt.Errorf("resolve a compute pod of endpoint %s: %w", endpointID, err)
		} else {
			res, rerr := kube.ForPod(pod, endpointContainer).Run(ctx, probe)
			switch {
			case rerr != nil:
				last = fmt.Errorf("exec psql in %s: %w", pod, rerr)
			case res.ExitCode != 0:
				last = fmt.Errorf("psql in %s exited %d: %s", pod, res.ExitCode, strings.TrimSpace(res.Stderr))
			case strings.TrimSpace(res.Stdout) == "1":
				if attempt > 0 && logf != nil {
					logf("endpoint %s authenticated %s after %d attempt(s)", endpointID, a.User, attempt+1)
				}
				return nil
			default:
				last = fmt.Errorf("psql in %s answered %q, want 1", pod, strings.TrimSpace(res.Stdout))
			}
		}
		if !time.Now().Before(deadline) {
			return fmt.Errorf("endpoint %s never authenticated %s at %s:%d within %s: %w",
				endpointID, a.User, a.Host, a.Port, timeout, last)
		}
		select {
		case <-ctx.Done():
			return errors.Join(ctx.Err(), last)
		case <-time.After(endpointAuthPoll):
		}
	}
}

func psqlProbe(a EndpointAuth, sql string) string {
	return fmt.Sprintf("PGPASSWORD=%s PGCONNECT_TIMEOUT=10 timeout %d psql -h %s -p %d -U %s -d %s -v ON_ERROR_STOP=1 -tAc %s",
		shellQuote(a.Password), int(endpointAuthProbeTimeout.Seconds()),
		a.Host, a.Port, a.User, a.Database, shellQuote(sql))
}

// EndpointPsql runs psql from fromEndpointID's pod: off loopback, so pg_hba asks for the password.
func EndpointPsql(ctx context.Context, kube *k8s.Kubectl, fromEndpointID string, a EndpointAuth, sql string) (string, error) {
	pod, err := EndpointPodName(ctx, kube, fromEndpointID)
	if err != nil {
		return "", fmt.Errorf("resolve a compute pod of endpoint %s: %w", fromEndpointID, err)
	}
	res, err := kube.ForPod(pod, endpointContainer).Run(ctx, psqlProbe(a, sql))
	if err != nil {
		return "", fmt.Errorf("exec psql in %s: %w", pod, err)
	}
	if res.ExitCode != 0 {
		return "", fmt.Errorf("psql in %s as %s to %s:%d exited %d: %s", pod, a.User, a.Host, a.Port, res.ExitCode, strings.TrimSpace(res.Stderr))
	}
	return strings.TrimSpace(res.Stdout), nil
}

func shellQuote(v string) string {
	return "'" + strings.ReplaceAll(v, "'", `'\''`) + "'"
}

func isNotFound(err error) bool {
	return err != nil && strings.Contains(strings.ToLower(err.Error()), "not found")
}
