package features

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/yugabyte/amp/test/functional/framework/apps/ampstack"
	"github.com/yugabyte/amp/test/functional/framework/apps/controller"
	"github.com/yugabyte/amp/test/functional/framework/core"
	"github.com/yugabyte/amp/test/functional/framework/exec/k8s"
)

// The kinds the endpoint chart renders and labels itself. helm's release
// Secrets and cert-manager's TLS Secrets lack the instance label, so the base
// selector already leaves them out.
var tagKinds = []string{"deployment", "pod", "service", "configmap", "secret", "persistentvolumeclaim", "certificates.cert-manager.io"}

var tagRequiredKinds = []string{"deployment", "pod", "service", "configmap"}

const tagLabelSettle = 3 * time.Minute

func init() {
	core.Register(core.Test{
		Name:        "features/amp_resource_tags",
		Suite:       "features",
		Category:    core.Functional,
		Env:         core.EnvSpec{Sharing: core.Exclusive, Shape: core.SharedControlPlane},
		Tags:        []string{"regression"},
		OwningTeam:  core.TeamControlPlane,
		Description: "Cluster and endpoint tags (PLAT-22775) and the identity metadata on endpoint pods (PLAT-22805, PLAT-22807). Tags that break the Kubernetes label rules, more than 50 tags, and keys the chart or the controller own are refused with 400 on both creates and leave no cluster behind. A cluster's tags read back on get and list, an endpoint reports only its own, and every object the endpoint chart renders is labelled with the cluster's tags merged under the endpoint's, the endpoint winning a shared key. Each pod carries the amp.yugabyte.com id labels and the pool, cluster, branch and endpoint name annotations, the endpoint name only when it has one. The labels survive an add_branch_extensions redeploy, an unnamed endpoint on a fork inherits the cluster's tags, names its branch and has no endpoint name annotation, and an endpoint of another cluster carries none of them.",
		Params:      []string{"functional", "env.ampstack", "apps.ybastack", "apps.yba", "apps.ybk8s"},
		Timeout:     45 * time.Minute,
		Run:         runAmpResourceTags,
	})
}

type tagsRun struct {
	a    *ampFeatureRun
	cl   *controller.Client
	kube *k8s.Kubectl

	pool        controller.StoragePool
	cluster     controller.Cluster
	clusterTags map[string]string
	writerTags  map[string]string
	writerName  string
	writer      controller.Endpoint
}

func runAmpResourceTags(t *core.T) error {
	return runAmpFeature(t, func(a *ampFeatureRun) []phase {
		r := &tagsRun{a: a}
		return []phase{
			{"tag_refusals", r.refusals},
			{"tags_on_release_objects", r.render},
			{"tags_survive_redeploy", r.redeploy},
			{"tags_on_fork_endpoint", r.fork},
			{"tags_isolated_per_cluster", r.isolation},
		}
	})
}

func (r *tagsRun) refusals(ctx context.Context) error {
	cl, err := r.a.f.controllerClient(ctx)
	if err != nil {
		return err
	}
	r.cl = cl
	r.kube = r.a.f.ampKube()
	if r.kube == nil {
		r.a.f.t.Skip("no kube handle for the amp namespace, and tags are only observable as labels on the endpoint's objects")
		return nil
	}
	tooMany := map[string]string{}
	for i := range 51 {
		tooMany[fmt.Sprintf("k%02d", i)] = "v"
	}
	cases := []struct {
		name string
		tags map[string]string
	}{
		{"a key with a space", map[string]string{"bad key": "v"}},
		{"a key starting with a dash", map[string]string{"-team": "v"}},
		{"a value with a space", map[string]string{"team": "two words"}},
		{"a 64-character value", map[string]string{"team": strings.Repeat("v", 64)}},
		{"a 64-character key name", map[string]string{strings.Repeat("k", 64): "v"}},
		{"a key under amp.yugabyte.com/", map[string]string{"amp.yugabyte.com/endpoint-id": "x"}},
		{"app.kubernetes.io/instance", map[string]string{"app.kubernetes.io/instance": "x"}},
		{"app.kubernetes.io/managed-by", map[string]string{"app.kubernetes.io/managed-by": "x"}},
		{"helm.sh/chart", map[string]string{"helm.sh/chart": "x"}},
		{"51 tags", tooMany},
	}
	proj := r.a.proj
	var failures []string
	for i, tc := range cases {
		name := fmt.Sprintf("tagbad-%d-%s", i, r.a.f.suffix())
		_, _, err := cl.CreateCluster(ctx, proj.poolID, controller.CreateClusterRequest{Name: name, PgVersion: r.a.f.cfg.PgVersion, Tags: tc.tags})
		if status := apiErrorStatus(err); status != http.StatusBadRequest {
			failures = append(failures, fmt.Sprintf("cluster create with %s answered %d (%v), want 400", tc.name, status, err))
			if err == nil {
				r.a.f.register("delete wrongly accepted cluster "+name, func(ctx context.Context) error {
					if c, ferr := findClusterCtl(ctx, cl, proj.poolID, name); ferr == nil {
						_, e := cl.DeleteClusterAndWait(ctx, proj.poolID, c.ID)
						return e
					}
					return nil
				})
			}
			continue
		}
		if !strings.Contains(err.Error(), "tag") {
			failures = append(failures, fmt.Sprintf("cluster create with %s was refused with %q, which does not say a tag is wrong", tc.name, err))
		}
		_, _, err = cl.CreateEndpoint(ctx, proj.poolID, proj.projID, proj.branchID, controller.CreateEndpointRequest{Role: roleReadWrite, Tags: tc.tags})
		if status := apiErrorStatus(err); status != http.StatusBadRequest {
			failures = append(failures, fmt.Sprintf("endpoint create with %s answered %d (%v), want 400", tc.name, status, err))
		}
	}
	list, err := cl.ListClustersIncludingFailed(ctx, proj.poolID, 200, 0)
	if err != nil {
		return fmt.Errorf("list clusters after the refusals: %w", err)
	}
	for _, c := range list.Clusters {
		if strings.HasPrefix(c.Name, "tagbad-") && strings.HasSuffix(c.Name, r.a.f.suffix()) {
			failures = append(failures, fmt.Sprintf("cluster %s (%s) exists after its create was refused", c.Name, c.ID))
		}
	}
	eps, err := cl.ListAllEndpoints(ctx, proj.poolID, proj.projID, proj.branchID)
	if err != nil {
		return fmt.Errorf("list endpoints after the refusals: %w", err)
	}
	if len(eps) != 0 {
		failures = append(failures, fmt.Sprintf("the branch has %d endpoints after every tagged endpoint create was refused", len(eps)))
	}
	if len(failures) > 0 {
		return fmt.Errorf("%d tag refusals did not hold: %s", len(failures), strings.Join(failures, "; "))
	}
	return nil
}

func (r *tagsRun) render(ctx context.Context) error {
	if r.kube == nil {
		return nil
	}
	cl, proj := r.cl, r.a.proj
	pool, err := cl.GetStoragePool(ctx, proj.poolID)
	if err != nil {
		return fmt.Errorf("read the pool: %w", err)
	}
	r.pool = pool
	r.clusterTags = map[string]string{"team": "payments", "cost-center": "42", "ampqa.example.com/owner": "tags-" + r.a.f.suffix()}
	r.writerTags = map[string]string{"team": "analytics", "tier": "gold"}
	r.writerName = "orders-writer"
	name := "tags-" + r.a.f.suffix()
	if _, err := cl.CreateClusterAndWait(ctx, proj.poolID, controller.CreateClusterRequest{Name: name, PgVersion: r.a.f.cfg.PgVersion, Tags: r.clusterTags}); err != nil {
		return fmt.Errorf("create the tagged cluster: %w", err)
	}
	listed, err := findClusterCtl(ctx, cl, proj.poolID, name)
	if err != nil {
		return err
	}
	r.a.f.register("delete tagged cluster", func(ctx context.Context) error {
		_, e := cl.DeleteClusterAndWait(ctx, proj.poolID, listed.ID)
		return e
	})
	if !maps.Equal(listed.Tags, r.clusterTags) {
		return fmt.Errorf("listClusters reports tags %v for %s, want %v", listed.Tags, listed.ID, r.clusterTags)
	}
	got, err := cl.GetCluster(ctx, proj.poolID, listed.ID)
	if err != nil {
		return err
	}
	if !maps.Equal(got.Tags, r.clusterTags) {
		return fmt.Errorf("getCluster reports tags %v, want %v", got.Tags, r.clusterTags)
	}
	r.cluster = got

	r.writer, err = createEndpointReq(ctx, cl, proj.poolID, got.ID, got.DefaultBranch.BranchID,
		controller.CreateEndpointRequest{Role: roleReadWrite, Name: r.writerName, Tags: r.writerTags})
	if err != nil {
		return fmt.Errorf("create the tagged writer: %w", err)
	}
	if err := r.endpointReportsOwnTags(ctx, r.writer, r.writerTags); err != nil {
		return err
	}
	if err := r.releaseLabelled(ctx, r.writer, r.mergedWriterTags(), nil); err != nil {
		return err
	}
	return r.podIdentity(ctx, r.writer, got.DefaultBranch.Name, r.writerName)
}

func (r *tagsRun) mergedWriterTags() map[string]string {
	m := maps.Clone(r.clusterTags)
	maps.Copy(m, r.writerTags)
	return m
}

func (r *tagsRun) endpointReportsOwnTags(ctx context.Context, ep controller.Endpoint, want map[string]string) error {
	got, err := r.cl.GetEndpoint(ctx, r.a.proj.poolID, ep.ClusterID, ep.BranchID, ep.ID)
	if err != nil {
		return fmt.Errorf("read endpoint %s: %w", ep.ID, err)
	}
	if got.Tags == nil || !maps.Equal(got.Tags, want) {
		return fmt.Errorf("getEndpoint %s reports tags %v, want exactly its own %v (not merged with the cluster's %v)", ep.ID, got.Tags, want, r.clusterTags)
	}
	return nil
}

// releaseLabelled holds every chart-rendered object of ep's release to the
// labels in want, and none of absent.
func (r *tagsRun) releaseLabelled(ctx context.Context, ep controller.Endpoint, want map[string]string, absent []string) error {
	release := ampstack.EndpointWorkloadName(ep.ID)
	base := "app.kubernetes.io/instance=" + release
	sel := []string{base}
	for _, k := range slices.Sorted(maps.Keys(want)) {
		sel = append(sel, k+"="+want[k])
	}
	for _, k := range absent {
		sel = append(sel, "!"+k)
	}
	tagged := strings.Join(sel, ",")
	var last []string
	err := pollUntil(ctx, tagLabelSettle, 5*time.Second, func() (bool, error) {
		last = last[:0]
		for _, kind := range tagKinds {
			all, err := r.kube.GetSelector(ctx, kind, base, "{.items[*].metadata.name}")
			if err != nil {
				if strings.Contains(err.Error(), "the server doesn't have a resource type") {
					continue
				}
				return false, fmt.Errorf("list %s of %s: %w", kind, release, err)
			}
			if all == "" && slices.Contains(tagRequiredKinds, kind) {
				last = append(last, "no "+kind+" at all")
				continue
			}
			ok, err := r.kube.GetSelector(ctx, kind, tagged, "{.items[*].metadata.name}")
			if err != nil {
				return false, fmt.Errorf("list %s of %s by tag: %w", kind, release, err)
			}
			have := strings.Fields(ok)
			for _, n := range strings.Fields(all) {
				if !slices.Contains(have, n) {
					last = append(last, kind+"/"+n)
				}
			}
		}
		return len(last) == 0, nil
	})
	if err != nil {
		return fmt.Errorf("endpoint %s: %d objects of release %s do not match the selector %q: %s", ep.ID, len(last), release, tagged, strings.Join(last, ", "))
	}
	return nil
}

func (r *tagsRun) podIdentity(ctx context.Context, ep controller.Endpoint, branchName, endpointName string) error {
	sel := "app.kubernetes.io/instance=" + ampstack.EndpointWorkloadName(ep.ID)
	labels, err := r.podMap(ctx, sel, "labels")
	if err != nil {
		return err
	}
	ann, err := r.podMap(ctx, sel, "annotations")
	if err != nil {
		return err
	}
	var bad []string
	for k, v := range map[string]string{
		"amp.yugabyte.com/endpoint-id":     ep.ID,
		"amp.yugabyte.com/branch-id":       ep.BranchID,
		"amp.yugabyte.com/cluster-id":      ep.ClusterID,
		"amp.yugabyte.com/storage-pool-id": r.pool.ID,
	} {
		if labels[k] != v {
			bad = append(bad, fmt.Sprintf("label %s=%q, want %q", k, labels[k], v))
		}
	}
	wantAnn := map[string]string{
		"amp.yugabyte.com/storage-pool-name": r.pool.Name,
		"amp.yugabyte.com/cluster-name":      r.cluster.Name,
		"amp.yugabyte.com/branch-name":       branchName,
	}
	if endpointName != "" {
		wantAnn["amp.yugabyte.com/endpoint-name"] = endpointName
	} else if v, ok := ann["amp.yugabyte.com/endpoint-name"]; ok {
		bad = append(bad, fmt.Sprintf("annotation amp.yugabyte.com/endpoint-name=%q on an endpoint created without a name", v))
	}
	for k, v := range wantAnn {
		if ann[k] != v {
			bad = append(bad, fmt.Sprintf("annotation %s=%q, want %q", k, ann[k], v))
		}
	}
	if len(bad) > 0 {
		slices.Sort(bad)
		return fmt.Errorf("endpoint %s pod identity: %s", ep.ID, strings.Join(bad, "; "))
	}
	return nil
}

func (r *tagsRun) podMap(ctx context.Context, selector, field string) (map[string]string, error) {
	raw, err := r.kube.GetSelector(ctx, "pod", selector, "{.items[0].metadata."+field+"}")
	if err != nil {
		return nil, fmt.Errorf("read the %s of the pod matching %s: %w", field, selector, err)
	}
	out := map[string]string{}
	if raw == "" {
		return out, nil
	}
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		return nil, fmt.Errorf("decode the %s of the pod matching %s (%q): %w", field, selector, raw, err)
	}
	return out, nil
}

// redeploy re-renders the writer through add_branch_extensions. A resize is no
// substitute: it can be in place and render nothing.
func (r *tagsRun) redeploy(ctx context.Context) error {
	if r.kube == nil {
		return nil
	}
	proj := r.a.proj
	before, err := r.podMap(ctx, "app.kubernetes.io/instance="+ampstack.EndpointWorkloadName(r.writer.ID), "labels")
	if err != nil {
		return err
	}
	_, err = r.cl.AddBranchExtensionsAndWait(ctx, proj.poolID, r.cluster.ID, r.cluster.DefaultBranch.BranchID, controller.AddBranchExtensionsRequest{Extensions: []string{"pgvector"}})
	if extensionsUnavailable(err) {
		r.a.f.logf("no extension catalog, so no redeploy re-renders the writer and the labels are not checked across one")
		return nil
	}
	if err != nil {
		return fmt.Errorf("redeploy the tagged endpoints: %w", err)
	}
	if err := ampstack.WaitEndpointWorkloadReady(ctx, r.kube, r.writer.ID, endpointReadyTimeout(r.a.f.t)); err != nil {
		return fmt.Errorf("endpoint %s is not ready after the redeploy: %w", r.writer.ID, err)
	}
	if err := r.releaseLabelled(ctx, r.writer, r.mergedWriterTags(), nil); err != nil {
		return fmt.Errorf("after the redeploy: %w", err)
	}
	after, err := r.podMap(ctx, "app.kubernetes.io/instance="+ampstack.EndpointWorkloadName(r.writer.ID), "labels")
	if err != nil {
		return err
	}
	for k := range r.mergedWriterTags() {
		if before[k] != after[k] {
			return fmt.Errorf("the writer's pod label %s was %q before the redeploy and %q after", k, before[k], after[k])
		}
	}
	return r.endpointReportsOwnTags(ctx, r.writer, r.writerTags)
}

func (r *tagsRun) fork(ctx context.Context) error {
	if r.kube == nil {
		return nil
	}
	proj := r.a.proj
	name := "tagfork-" + r.a.f.suffix()
	if _, err := r.cl.CreateBranchAndWait(ctx, proj.poolID, r.cluster.ID, controller.CreateBranchRequest{Name: name, ParentBranchID: r.cluster.DefaultBranch.BranchID}); err != nil {
		return fmt.Errorf("fork the tagged cluster's default branch: %w", err)
	}
	br, err := findBranchCtl(ctx, r.cl, proj.poolID, r.cluster.ID, name)
	if err != nil {
		return err
	}
	ep, err := createEndpointReq(ctx, r.cl, proj.poolID, r.cluster.ID, br.BranchID,
		controller.CreateEndpointRequest{Role: roleReadWrite})
	if err != nil {
		return fmt.Errorf("create the fork's endpoint: %w", err)
	}
	if err := r.endpointReportsOwnTags(ctx, ep, map[string]string{}); err != nil {
		return err
	}
	if err := r.releaseLabelled(ctx, ep, r.clusterTags, []string{"tier"}); err != nil {
		return fmt.Errorf("the fork's endpoint: %w", err)
	}
	return r.podIdentity(ctx, ep, name, "")
}

func (r *tagsRun) isolation(ctx context.Context) error {
	if r.kube == nil {
		return nil
	}
	proj := r.a.proj
	ep, err := createEndpoint(ctx, r.cl, proj.poolID, proj.projID, proj.branchID)
	if err != nil {
		return fmt.Errorf("create an endpoint on the untagged cluster: %w", err)
	}
	if err := r.endpointReportsOwnTags(ctx, ep, map[string]string{}); err != nil {
		return err
	}
	labels, err := r.podMap(ctx, "app.kubernetes.io/instance="+ampstack.EndpointWorkloadName(ep.ID), "labels")
	if err != nil {
		return err
	}
	for k := range r.mergedWriterTags() {
		if v, ok := labels[k]; ok {
			return fmt.Errorf("endpoint %s of the untagged cluster %s carries label %s=%q from cluster %s", ep.ID, proj.projID, k, v, r.cluster.ID)
		}
	}
	return nil
}
