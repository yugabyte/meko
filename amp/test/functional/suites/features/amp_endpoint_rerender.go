package features

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/yugabyte/amp/test/functional/framework/apps/controller"
	"github.com/yugabyte/amp/test/functional/framework/core"
	"github.com/yugabyte/amp/test/functional/framework/exec/k8s"
)

const (
	rerenderTable     = "amp_rerender"
	rerenderRows      = 50
	unknownEndpointID = "00000000-0000-4000-8000-000000000000"
)

func init() {
	core.Register(core.Test{
		Name:        "features/amp_endpoint_rerender",
		Suite:       "features",
		Category:    core.Stress,
		Env:         core.EnvSpec{Sharing: core.Exclusive, Shape: core.SharedControlPlane, SharedLock: core.LockEndpointFleetUpgrade},
		Tags:        []string{"regression"},
		OwningTeam:  core.TeamControlPlane,
		Description: "POST /endpoint-rerenders: max_parallel 0 and 51 are refused with 400; a roll naming one endpoint twice plus an unknown id re-renders that endpoint once, reports the unknown id as skipped, leaves a neighbour endpoint's pod alone, and, because nothing about the endpoint changed, neither recreates its pod nor restarts its postmaster, with its data readable and writes committing afterwards.",
		Params:      []string{"functional", "env.ampstack", "apps.ybastack", "apps.yba", "apps.ybk8s"},
		Run:         runAmpEndpointRerender,
	})
}

func runAmpEndpointRerender(t *core.T) error {
	return runAmpFeature(t, func(a *ampFeatureRun) []phase {
		return []phase{{"endpoint_rerender", func(ctx context.Context) error {
			cl, err := a.f.controllerClient(ctx)
			if err != nil {
				return err
			}
			neighbour, err := createClusterOnPool(ctx, cl, a.proj.poolID, "rr-"+a.f.suffix(), a.f.cfg.PgVersion, a.f.register)
			if err != nil {
				return err
			}
			return endpointRerenderSteps(ctx, cl, a.proj, neighbour, a.f.endpointSQL, a.f.ampKube())
		}}}
	})
}

type rerenderLine struct {
	EndpointID string `json:"endpoint_id"`
	Outcome    string `json:"outcome"`
	Reason     string `json:"reason"`
}

type rerenderResult struct {
	Endpoints      []rerenderLine `json:"endpoints"`
	Rerendered     int            `json:"rerendered"`
	Skipped        int            `json:"skipped"`
	NamedEndpoints int            `json:"named_endpoints"`
}

func endpointRerenderSteps(ctx context.Context, cl *controller.Client, proj, neighbourProj ampCluster, connect endpointConnector, kube *k8s.Kubectl) error {
	for _, bad := range []int{0, 51} {
		n := bad
		_, _, err := cl.RerenderEndpoints(ctx, controller.RerenderEndpointsRequest{EndpointIDs: []string{unknownEndpointID}, MaxParallel: &n})
		var apiErr *controller.APIError
		if !errors.As(err, &apiErr) || apiErr.StatusCode != http.StatusBadRequest {
			return fmt.Errorf("a re-render with max_parallel=%d came back %v, want a 400: the spec bounds it to 1..50", bad, err)
		}
	}

	ep, err := createEndpoint(ctx, cl, proj.poolID, proj.projID, proj.branchID)
	if err != nil {
		return fmt.Errorf("create endpoint: %w", err)
	}
	neighbour, err := createEndpoint(ctx, cl, neighbourProj.poolID, neighbourProj.projID, neighbourProj.branchID)
	if err != nil {
		return fmt.Errorf("create neighbour endpoint: %w", err)
	}
	conn, err := connect(ctx, ep)
	if err != nil {
		return fmt.Errorf("connect endpoint: %w", err)
	}
	for _, stmt := range []string{
		"CREATE TABLE " + rerenderTable + " (id int PRIMARY KEY, v text NOT NULL)",
		fmt.Sprintf("INSERT INTO %s SELECT g, md5(g::text) FROM generate_series(1, %d) g", rerenderTable, rerenderRows),
	} {
		if _, err := conn.Exec(ctx, stmt); err != nil {
			conn.Close()
			return fmt.Errorf("seed: %w", err)
		}
	}
	started, err := conn.QueryString(ctx, "SELECT pg_postmaster_start_time()::text")
	conn.Close()
	if err != nil {
		return err
	}
	before, err := endpointPodUID(ctx, kube, ep.ID)
	if err != nil {
		return err
	}
	neighbourBefore, err := endpointPodUID(ctx, kube, neighbour.ID)
	if err != nil {
		return err
	}

	// The roll only ever names this test's endpoints: an empty endpoint_ids rolls
	// every endpoint on the shared controller.
	task, _, err := cl.RerenderEndpoints(ctx, controller.RerenderEndpointsRequest{EndpointIDs: []string{ep.ID, ep.ID, unknownEndpointID}})
	if err != nil {
		return fmt.Errorf("submit the re-render: %w", err)
	}
	done, err := cl.WaitTask(ctx, task.ID)
	if err != nil {
		return fmt.Errorf("wait for re-render task %s: %w", task.ID, err)
	}
	if done.Status != "succeeded" {
		return fmt.Errorf("re-render task %s ended %s: %s", task.ID, done.Status, derefOr(done.Error, "no error recorded"))
	}
	var res rerenderResult
	if err := json.Unmarshal(done.Result, &res); err != nil {
		return fmt.Errorf("decode re-render result %s: %w", string(done.Result), err)
	}
	if res.Rerendered != 1 || res.Skipped != 1 || len(res.Endpoints) != 2 {
		return fmt.Errorf("a roll naming %s twice and one unknown id reported %d re-rendered, %d skipped over %d line(s), want 1, 1 and 2 (a repeated id is collapsed, an unknown one is a skipped line): %s",
			ep.ID, res.Rerendered, res.Skipped, len(res.Endpoints), string(done.Result))
	}
	for _, l := range res.Endpoints {
		switch l.EndpointID {
		case ep.ID:
			if l.Outcome != "rerendered" {
				return fmt.Errorf("the named endpoint's line is %q (%s), want rerendered", l.Outcome, l.Reason)
			}
		case unknownEndpointID:
			if l.Outcome != "skipped" {
				return fmt.Errorf("the unknown id's line is %q (%s), want skipped", l.Outcome, l.Reason)
			}
		case neighbour.ID:
			return fmt.Errorf("the roll named only %s, yet its result carries a line for the neighbour endpoint %s", ep.ID, neighbour.ID)
		default:
			return fmt.Errorf("the re-render result names endpoint %s, which the roll was never asked about", l.EndpointID)
		}
	}

	after, err := endpointPodUID(ctx, kube, ep.ID)
	if err != nil {
		return err
	}
	if after != before {
		return fmt.Errorf("re-rendering endpoint %s with nothing about it changed replaced its pod (uid %s -> %s); the spec says a re-render carrying the current values is a no-op helm upgrade that does not recreate the pod", ep.ID, before, after)
	}
	neighbourAfter, err := endpointPodUID(ctx, kube, neighbour.ID)
	if err != nil {
		return err
	}
	if neighbourAfter != neighbourBefore {
		return fmt.Errorf("a re-render naming only %s replaced the neighbour endpoint %s's pod (uid %s -> %s)", ep.ID, neighbour.ID, neighbourBefore, neighbourAfter)
	}

	conn, err = connect(ctx, ep)
	if err != nil {
		return fmt.Errorf("reconnect after the re-render: %w", err)
	}
	defer conn.Close()
	startedAfter, err := conn.QueryString(ctx, "SELECT pg_postmaster_start_time()::text")
	if err != nil {
		return err
	}
	if startedAfter != started {
		return fmt.Errorf("the postmaster restarted across a no-op re-render (started %s, now %s)", started, startedAfter)
	}
	n, err := conn.QueryInt(ctx, "SELECT count(*) FROM "+rerenderTable)
	if err != nil {
		return err
	}
	if n != rerenderRows {
		return fmt.Errorf("%s holds %d rows after the re-render, want %d", rerenderTable, n, rerenderRows)
	}
	if _, err := conn.Exec(ctx, fmt.Sprintf("INSERT INTO %s VALUES (%d, 'after-rerender')", rerenderTable, rerenderRows+1)); err != nil {
		return fmt.Errorf("a write after the re-render: %w", err)
	}
	return nil
}

func endpointPodUID(ctx context.Context, kube *k8s.Kubectl, endpointID string) (string, error) {
	sel := "app.kubernetes.io/instance=" + endpointDeployName(endpointID)
	uids, err := kube.GetSelector(ctx, "pod", sel, "{.items[*].metadata.uid}")
	if err != nil {
		return "", fmt.Errorf("read endpoint %s's pod uid: %w", endpointID, err)
	}
	f := strings.Fields(uids)
	if len(f) != 1 {
		return "", fmt.Errorf("endpoint %s has %d pod(s) (%s), want exactly one to compare", endpointID, len(f), uids)
	}
	return f[0], nil
}

func derefOr(s *string, def string) string {
	if s == nil || *s == "" {
		return def
	}
	return *s
}
