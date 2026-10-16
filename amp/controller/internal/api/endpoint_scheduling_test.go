package api_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/yugabyte/amp/controller/internal/api"
	"github.com/yugabyte/amp/controller/internal/core"
)

func dedicatedToleration() api.Toleration {
	op, eff := api.TolerationOperatorEqual, api.NoSchedule
	return api.Toleration{Key: strptr("dedicated"), Operator: &op, Value: strptr("amp"), Effect: &eff}
}

func zoneSpread(clusterID uuid.UUID) api.TopologySpreadConstraint {
	return api.TopologySpreadConstraint{
		MaxSkew: 1, TopologyKey: "topology.kubernetes.io/zone", WhenUnsatisfiable: api.ScheduleAnyway,
		LabelSelector: api.LabelSelector{MatchLabels: &map[string]string{"amp.yugabyte.com/cluster-id": clusterID.String()}},
	}
}

// newScheduledEndpoint creates an endpoint carrying the given lists and runs its
// create task.
func newScheduledEndpoint(t *testing.T, tols *[]api.Toleration, tscs *[]api.TopologySpreadConstraint) (
	*api.ClientWithResponses, *fakeDeployer, func(), [4]uuid.UUID,
) {
	t.Helper()
	fake := &fakeDeployer{}
	prov := api.Provisioning{Deployer: fake}
	client, backend, store := newTestServerWith(t, core.NewMemBackend(), prov)
	ctx := context.Background()

	user := mustCreateUser(t, client, false)
	pool := mustCreatePool(t, client, user.Id, "sched-pool")
	clusterID, branchID := uuid.New(), uuid.New()
	backend.SeedBranch(pool.Id, core.Branch{
		ClusterID: clusterID, BranchID: branchID, Name: "main", PgVersion: 18, IsDefault: true,
	})
	rw := api.CreateEndpointRequestRoleReadWrite
	created, err := client.CreateEndpointWithResponse(ctx, pool.Id, clusterID, branchID, api.CreateEndpointRequest{
		Role: &rw, Password: strptr(testPassword), Tolerations: tols, TopologySpreadConstraints: tscs,
	})
	if err != nil || created.StatusCode() != http.StatusAccepted {
		t.Fatalf("CreateEndpoint: status=%d err=%v body=%s", created.StatusCode(), err, created.Body)
	}
	drain := func() { drainTasks(t, store, backend, prov) }
	drain()
	list, err := client.ListEndpointsWithResponse(ctx, pool.Id, clusterID, branchID, nil)
	if err != nil || list.JSON200 == nil || len(list.JSON200.Endpoints) != 1 {
		t.Fatalf("ListEndpoints: %v", err)
	}
	return client, fake, drain, [4]uuid.UUID{pool.Id, clusterID, branchID, list.JSON200.Endpoints[0].Id}
}

func jsonOf(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// What a create declares reaches the pod and is reported back as given.
func TestCreateEndpointCarriesScheduling(t *testing.T) {
	tols := []api.Toleration{dedicatedToleration()}
	tscs := []api.TopologySpreadConstraint{zoneSpread(uuid.New())}
	client, fake, _, ids := newScheduledEndpoint(t, &tols, &tscs)

	fake.mu.Lock()
	spec := fake.created[0]
	fake.mu.Unlock()
	if string(spec.Scheduling.Tolerations) != jsonOf(t, tols) {
		t.Errorf("deployed tolerations = %s, want %s", spec.Scheduling.Tolerations, jsonOf(t, tols))
	}
	if string(spec.Scheduling.TopologySpreadConstraints) != jsonOf(t, tscs) {
		t.Errorf("deployed spread = %s, want %s", spec.Scheduling.TopologySpreadConstraints, jsonOf(t, tscs))
	}

	got, _ := client.GetEndpointWithResponse(context.Background(), ids[0], ids[1], ids[2], ids[3])
	ep := got.JSON200
	if ep.Tolerations == nil || jsonOf(t, *ep.Tolerations) != jsonOf(t, tols) {
		t.Errorf("GET tolerations = %v, want %v", ep.Tolerations, tols)
	}
	if ep.TopologySpreadConstraints == nil || jsonOf(t, *ep.TopologySpreadConstraints) != jsonOf(t, tscs) {
		t.Errorf("GET spread = %v, want %v", ep.TopologySpreadConstraints, tscs)
	}
}

// An endpoint created without them reports neither, rather than empty lists.
func TestCreateEndpointWithoutSchedulingReportsNone(t *testing.T) {
	client, fake, _, ids := newScheduledEndpoint(t, nil, nil)
	if s := fake.created[0].Scheduling; s.Tolerations != nil || s.TopologySpreadConstraints != nil {
		t.Errorf("deployed scheduling = %+v, want none", s)
	}
	got, _ := client.GetEndpointWithResponse(context.Background(), ids[0], ids[1], ids[2], ids[3])
	if got.JSON200.Tolerations != nil || got.JSON200.TopologySpreadConstraints != nil {
		t.Errorf("GET = %v / %v, want both absent", got.JSON200.Tolerations, got.JSON200.TopologySpreadConstraints)
	}
}

// An update re-renders with the new list, keeps the omitted one, reports the
// change only once the task ran, and stamps the generation.
func TestUpdateEndpointSchedulingReplacesOneListAndKeepsTheOther(t *testing.T) {
	tols := []api.Toleration{dedicatedToleration()}
	tscs := []api.TopologySpreadConstraint{zoneSpread(uuid.New())}
	client, fake, drain, ids := newScheduledEndpoint(t, &tols, &tscs)
	ctx := context.Background()

	exists := api.TolerationOperatorExists
	newTols := []api.Toleration{{Key: strptr("gpu"), Operator: &exists}}
	gen := int64(3)
	resp, err := client.UpdateEndpointSchedulingWithResponse(ctx, ids[0], ids[1], ids[2], ids[3],
		api.UpdateEndpointSchedulingRequest{Tolerations: &newTols, YcpGeneration: &gen})
	if err != nil || resp.StatusCode() != http.StatusAccepted {
		t.Fatalf("UpdateEndpointScheduling: status=%d err=%v body=%s", resp.StatusCode(), err, resp.Body)
	}
	pending, _ := client.GetEndpointWithResponse(ctx, ids[0], ids[1], ids[2], ids[3])
	if jsonOf(t, *pending.JSON200.Tolerations) != jsonOf(t, tols) {
		t.Error("GET reports the new tolerations on a 202 alone, before the task ran")
	}

	drain()

	done, _ := client.GetEndpointWithResponse(ctx, ids[0], ids[1], ids[2], ids[3])
	ep := done.JSON200
	if ep.Tolerations == nil || jsonOf(t, *ep.Tolerations) != jsonOf(t, newTols) {
		t.Errorf("tolerations = %v, want %v", ep.Tolerations, newTols)
	}
	if ep.TopologySpreadConstraints == nil || jsonOf(t, *ep.TopologySpreadConstraints) != jsonOf(t, tscs) {
		t.Errorf("spread = %v, want the stored one kept: it was omitted", ep.TopologySpreadConstraints)
	}
	if ep.YcpGeneration == nil || *ep.YcpGeneration != gen {
		t.Errorf("ycp_generation = %v, want %d", ep.YcpGeneration, gen)
	}

	fake.mu.Lock()
	defer fake.mu.Unlock()
	if len(fake.created) != 2 {
		t.Fatalf("deploy calls = %d, want the create and one re-render", len(fake.created))
	}
	rendered := fake.created[1].Scheduling
	if string(rendered.Tolerations) != jsonOf(t, newTols) || string(rendered.TopologySpreadConstraints) != jsonOf(t, tscs) {
		t.Errorf("re-render = %+v, want the new tolerations and the kept spread", rendered)
	}
}

// [] clears a list, and the endpoint then reads like one created without it.
func TestUpdateEndpointSchedulingClears(t *testing.T) {
	tols := []api.Toleration{dedicatedToleration()}
	client, fake, drain, ids := newScheduledEndpoint(t, &tols, nil)
	ctx := context.Background()

	resp, err := client.UpdateEndpointSchedulingWithResponse(ctx, ids[0], ids[1], ids[2], ids[3],
		api.UpdateEndpointSchedulingRequest{Tolerations: &[]api.Toleration{}})
	if err != nil || resp.StatusCode() != http.StatusAccepted {
		t.Fatalf("UpdateEndpointScheduling: status=%d err=%v body=%s", resp.StatusCode(), err, resp.Body)
	}
	drain()
	done, _ := client.GetEndpointWithResponse(ctx, ids[0], ids[1], ids[2], ids[3])
	if done.JSON200.Tolerations != nil {
		t.Errorf("tolerations = %v, want none", *done.JSON200.Tolerations)
	}
	if got := fake.created[len(fake.created)-1].Scheduling.Tolerations; got != nil {
		t.Errorf("re-render tolerations = %s, want none", got)
	}
}

// A failed re-render leaves GET reporting what the pod still runs with.
func TestUpdateEndpointSchedulingFailedRenderPersistsNothing(t *testing.T) {
	tols := []api.Toleration{dedicatedToleration()}
	client, fake, drain, ids := newScheduledEndpoint(t, &tols, nil)
	ctx := context.Background()

	fake.mu.Lock()
	fake.createErr = errors.New("helm upgrade failed")
	fake.mu.Unlock()
	resp, err := client.UpdateEndpointSchedulingWithResponse(ctx, ids[0], ids[1], ids[2], ids[3],
		api.UpdateEndpointSchedulingRequest{Tolerations: &[]api.Toleration{}})
	if err != nil || resp.StatusCode() != http.StatusAccepted {
		t.Fatalf("UpdateEndpointScheduling: status=%d err=%v body=%s", resp.StatusCode(), err, resp.Body)
	}
	drain()
	done, _ := client.GetEndpointWithResponse(ctx, ids[0], ids[1], ids[2], ids[3])
	if done.JSON200.Tolerations == nil || jsonOf(t, *done.JSON200.Tolerations) != jsonOf(t, tols) {
		t.Errorf("tolerations = %v, want the original kept after a failed render", done.JSON200.Tolerations)
	}
}

func TestUpdateEndpointSchedulingRefusals(t *testing.T) {
	client, fake, _, ids := newScheduledEndpoint(t, nil, nil)
	ctx := context.Background()
	exists := api.TolerationOperatorExists

	for name, tc := range map[string]struct {
		body api.UpdateEndpointSchedulingRequest
		want string
	}{
		"nothing to change": {api.UpdateEndpointSchedulingRequest{}, "send tolerations"},
		"exists with a value": {api.UpdateEndpointSchedulingRequest{
			Tolerations: &[]api.Toleration{{Key: strptr("k"), Operator: &exists, Value: strptr("v")}}}, "value must be empty"},
		"spread without a selector": {api.UpdateEndpointSchedulingRequest{
			TopologySpreadConstraints: &[]api.TopologySpreadConstraint{{
				MaxSkew: 1, TopologyKey: "topology.kubernetes.io/zone", WhenUnsatisfiable: api.DoNotSchedule}}},
			"matchLabels or matchExpressions is required"},
	} {
		t.Run(name, func(t *testing.T) {
			resp, err := client.UpdateEndpointSchedulingWithResponse(ctx, ids[0], ids[1], ids[2], ids[3], tc.body)
			if err != nil || resp.StatusCode() != http.StatusBadRequest {
				t.Fatalf("status=%d err=%v, want 400", resp.StatusCode(), err)
			}
			if !strings.Contains(string(resp.Body), tc.want) {
				t.Errorf("body = %s, want it to mention %q", resp.Body, tc.want)
			}
		})
	}

	missing, _ := client.UpdateEndpointSchedulingWithResponse(ctx, ids[0], ids[1], ids[2], uuid.New(),
		api.UpdateEndpointSchedulingRequest{Tolerations: &[]api.Toleration{}})
	if missing.StatusCode() != http.StatusNotFound {
		t.Errorf("unknown endpoint: status = %d, want 404", missing.StatusCode())
	}
	if len(fake.created) != 1 {
		t.Errorf("deploy calls = %d, want only the create: a refusal plans nothing", len(fake.created))
	}
}

// Like connectivity, the writer's scheduling on a writerless create is refused
// rather than dropped.
func TestCreateClusterRefusesSchedulingWithoutWriter(t *testing.T) {
	client, _, _ := newTestServerWith(t, core.NewMemBackend(), api.Provisioning{Deployer: &fakeDeployer{}})
	user := mustCreateUser(t, client, false)
	pool := mustCreatePool(t, client, user.Id, "sched-cluster-pool")
	tols := []api.Toleration{dedicatedToleration()}
	resp, err := client.CreateClusterWithResponse(context.Background(), pool.Id, api.CreateClusterRequest{
		Name: "orders", Tolerations: &tols,
	})
	if err != nil || resp.StatusCode() != http.StatusBadRequest {
		t.Fatalf("status=%d err=%v, want 400", resp.StatusCode(), err)
	}
	if !strings.Contains(string(resp.Body), "with_writer") {
		t.Errorf("body = %s, want it to point at with_writer", resp.Body)
	}
}
