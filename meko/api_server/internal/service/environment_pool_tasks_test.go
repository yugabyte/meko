package service

import (
	"fmt"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/lib/pq"

	"meko/api_server/internal/repository"
)

// poolTaskCols are the columns the environment_tasks SELECT returns, in order. Kept
// here (not shared with the repository test) so a column change surfaces in both.
var poolTaskCols = []string{
	"task_id", "kind", "status", "cluster_id", "target_ref", "error",
	"result", "ycp_generation", "task_created_at", "task_updated_at", "reported_at",
}

// poolEnvRow is an envRow with an explicit uuid/id/region and observed-resources JSON,
// so a test can give two environments distinct clusters. type is MANAGED and kind amp,
// which is what the pool resolver selects.
func poolEnvRow(uuid, id, region, resourcesJSON string) *sqlmock.Rows {
	return sqlmock.NewRows(envRowCols).AddRow(
		uuid, id, acctA, "cloud", region, "READY", "", 0, "",
		0, 400, "", nil, "", "", "[]", true, keyA, time.Now(), KindAmp, resourcesJSON, nil, "MANAGED", false,
	)
}

// clusterObserved is an observed cluster inventory entry under a pool, carrying its amp
// UUID in attrs["id"] -- the only bridge from a storage pool to a task's cluster_id.
func clusterObserved(pool, name, ampUUID string) string {
	return fmt.Sprintf(
		`{"kind":"cluster","id":"cluster/%s/%s","parent":"storage-pool/%s","attrs":{"id":"%s"}}`,
		pool, name, pool, ampUUID)
}

// expectPoolEnv registers the per-environment reads TasksByStoragePool does before it
// lists tasks: the environment row, its (empty) desired set, and the clusters
// remembered under pool -- the ones the observed inventory has since dropped.
func expectPoolEnv(mock sqlmock.Sqlmock, uuid, id, region, pool, resourcesJSON string, remembered ...string) {
	mock.ExpectQuery(`SELECT .* FROM meko_system\.environments e WHERE e\.uuid = \$1`).
		WithArgs(uuid).
		WillReturnRows(poolEnvRow(uuid, id, region, resourcesJSON))
	mock.ExpectQuery(`FROM meko_system\.environment_desired_resources`).
		WithArgs(uuid).
		WillReturnRows(sqlmock.NewRows([]string{"kind", "name", "spec", "generation"}))
	rows := sqlmock.NewRows([]string{"cluster_id"})
	for _, c := range remembered {
		rows.AddRow(c)
	}
	mock.ExpectQuery(`FROM meko_system\.environment_cluster_pools`).
		WithArgs(uuid, pool).
		WillReturnRows(rows)
}

// TestTasksByStoragePool_MergesAcrossEnvsNewestFirst: two environments' pool-scoped
// task pages are clubbed into one list, sorted newest-first across environments, and
// paginated globally. Each item keeps its own environment_id/region.
func TestTasksByStoragePool_MergesAcrossEnvsNewestFirst(t *testing.T) {
	svc, mock, done := newEnvSvc(t)
	defer done()

	const (
		pool    = "aaaaaaaa-1111-2222-3333-444444444444"
		envEast = "11111111-1111-1111-1111-111111111111"
		envWest = "22222222-2222-2222-2222-222222222222"
		caEast  = "33333333-3333-3333-3333-333333333333"
		caWest  = "44444444-4444-4444-4444-444444444444"
	)
	t1 := time.Date(2026, 10, 1, 7, 30, 0, 0, time.UTC) // oldest
	t2 := time.Date(2026, 10, 1, 7, 35, 0, 0, time.UTC) // middle
	t3 := time.Date(2026, 10, 1, 7, 40, 0, 0, time.UTC) // newest

	// Mapping resolves two MANAGED amp envs, ordered by region ASC (east then west).
	mock.ExpectQuery(`FROM meko_system\.environment_aeon_account_mapping`).
		WithArgs(pool).
		WillReturnRows(sqlmock.NewRows([]string{"environment_id", "region"}).
			AddRow(envEast, "us-east-1").
			AddRow(envWest, "us-west-2"))

	// East: cluster caEast, two tasks (A newest t3, B oldest t1).
	expectPoolEnv(mock, envEast, "amp-east", "us-east-1", pool, "["+clusterObserved(pool, "c1", caEast)+"]")
	mock.ExpectQuery(`SELECT count\(\*\) FROM meko_system\.environment_tasks`).
		WithArgs(envEast, pq.Array([]string{caEast})).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(2))
	mock.ExpectQuery(`FROM meko_system\.environment_tasks`).
		WithArgs(envEast, pq.Array([]string{caEast}), tasksMaxMerge, 0).
		WillReturnRows(sqlmock.NewRows(poolTaskCols).
			AddRow("task-A", "resize_endpoint", "running", caEast, "endpoint/a", nil, "", nil, t3, t3, t3).
			AddRow("task-B", "create_cluster", "succeeded", caEast, nil, nil, "", nil, t1, t1, t1))

	// West: cluster caWest, one task (C middle t2).
	expectPoolEnv(mock, envWest, "amp-west", "us-west-2", pool, "["+clusterObserved(pool, "c2", caWest)+"]")
	mock.ExpectQuery(`SELECT count\(\*\) FROM meko_system\.environment_tasks`).
		WithArgs(envWest, pq.Array([]string{caWest})).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(1))
	mock.ExpectQuery(`FROM meko_system\.environment_tasks`).
		WithArgs(envWest, pq.Array([]string{caWest}), tasksMaxMerge, 0).
		WillReturnRows(sqlmock.NewRows(poolTaskCols).
			AddRow("task-C", "create_branch", "succeeded", caWest, nil, nil, "", nil, t2, t2, t2))

	// page_size 2 over the merged 3: newest two, has_more true.
	got, err := svc.TasksByStoragePool(pool, repository.EnvironmentTaskListOpts{Limit: 2, Offset: 0})
	if err != nil {
		t.Fatalf("TasksByStoragePool: %v", err)
	}
	if got.Total != 3 {
		t.Errorf("total = %d, want 3 (merged across envs)", got.Total)
	}
	if !got.HasMore {
		t.Error("has_more = false with 2 of 3 merged tasks returned")
	}
	if len(got.Items) != 2 {
		t.Fatalf("items = %d, want 2 (the page)", len(got.Items))
	}
	// Newest first across environments: A (east, t3) then C (west, t2).
	if got.Items[0].TaskID != "task-A" || got.Items[0].EnvironmentID != "amp-east" || got.Items[0].Region != "us-east-1" {
		t.Errorf("items[0] = %+v, want task-A on amp-east/us-east-1", got.Items[0])
	}
	if got.Items[1].TaskID != "task-C" || got.Items[1].EnvironmentID != "amp-west" || got.Items[1].Region != "us-west-2" {
		t.Errorf("items[1] = %+v, want task-C on amp-west/us-west-2", got.Items[1])
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

// TestTasksByStoragePool_ScopesToPoolClustersOnly is the leak test: a shared
// environment carries clusters of two pools, and a query for one pool must bind only
// that pool's cluster UUID -- never the sibling's -- so the sibling's tasks cannot
// surface. The exact pq.Array arg is what proves it.
func TestTasksByStoragePool_ScopesToPoolClustersOnly(t *testing.T) {
	svc, mock, done := newEnvSvc(t)
	defer done()

	const (
		poolA = "aaaaaaaa-1111-2222-3333-444444444444"
		poolB = "bbbbbbbb-1111-2222-3333-444444444444"
		env   = "11111111-1111-1111-1111-111111111111"
		caA   = "33333333-3333-3333-3333-333333333333"
		caB   = "55555555-5555-5555-5555-555555555555"
	)
	now := time.Now().UTC()

	mock.ExpectQuery(`FROM meko_system\.environment_aeon_account_mapping`).
		WithArgs(poolA).
		WillReturnRows(sqlmock.NewRows([]string{"environment_id", "region"}).
			AddRow(env, "us-east-1"))

	// The one shared env observes BOTH pools' clusters.
	resources := "[" + clusterObserved(poolA, "c1", caA) + "," + clusterObserved(poolB, "c9", caB) + "]"
	expectPoolEnv(mock, env, "amp-shared", "us-east-1", poolA, resources)
	// Only poolA's cluster UUID may be bound -- caB must never appear.
	mock.ExpectQuery(`SELECT count\(\*\) FROM meko_system\.environment_tasks`).
		WithArgs(env, pq.Array([]string{caA})).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(1))
	mock.ExpectQuery(`FROM meko_system\.environment_tasks`).
		WithArgs(env, pq.Array([]string{caA}), tasksMaxMerge, 0).
		WillReturnRows(sqlmock.NewRows(poolTaskCols).
			AddRow("task-A", "resize_endpoint", "running", caA, nil, nil, "", nil, now, now, now))

	got, err := svc.TasksByStoragePool(poolA, repository.EnvironmentTaskListOpts{Limit: 25})
	if err != nil {
		t.Fatalf("TasksByStoragePool: %v", err)
	}
	if len(got.Items) != 1 || got.Items[0].TaskID != "task-A" {
		t.Fatalf("items = %+v, want only poolA's task-A", got.Items)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

// TestTasksByStoragePool_EmptyObservedContributesNothing: an env the pool is mapped to
// but has no observed cluster in must yield zero tasks and never touch the task log --
// the one path where falling through to the shared env's unfiltered history would leak.
func TestTasksByStoragePool_EmptyObservedContributesNothing(t *testing.T) {
	svc, mock, done := newEnvSvc(t)
	defer done()

	const (
		pool = "aaaaaaaa-1111-2222-3333-444444444444"
		env  = "11111111-1111-1111-1111-111111111111"
	)
	mock.ExpectQuery(`FROM meko_system\.environment_aeon_account_mapping`).
		WithArgs(pool).
		WillReturnRows(sqlmock.NewRows([]string{"environment_id", "region"}).
			AddRow(env, "us-east-1"))
	// No cluster observed under the pool. No ListTasks expectation is registered, so a
	// query against the task table here is an unexpected call and fails the test.
	expectPoolEnv(mock, env, "amp-empty", "us-east-1", pool, "[]")

	got, err := svc.TasksByStoragePool(pool, repository.EnvironmentTaskListOpts{Limit: 25})
	if err != nil {
		t.Fatalf("TasksByStoragePool: %v", err)
	}
	if len(got.Items) != 0 || got.Total != 0 {
		t.Errorf("got %+v, want an empty page (no leak of the shared env log)", got)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("the empty-observed env touched the task table: %v", err)
	}
}

// TestTasksByStoragePool_CallerClusterIDOutsidePoolYieldsNothing: a caller-supplied
// cluster_id that is not one of the pool's own clusters cannot be used to read another
// pool's cluster tasks in a shared env.
func TestTasksByStoragePool_CallerClusterIDOutsidePoolYieldsNothing(t *testing.T) {
	svc, mock, done := newEnvSvc(t)
	defer done()

	const (
		pool    = "aaaaaaaa-1111-2222-3333-444444444444"
		env     = "11111111-1111-1111-1111-111111111111"
		caPool  = "33333333-3333-3333-3333-333333333333"
		caOther = "99999999-9999-9999-9999-999999999999"
	)
	mock.ExpectQuery(`FROM meko_system\.environment_aeon_account_mapping`).
		WithArgs(pool).
		WillReturnRows(sqlmock.NewRows([]string{"environment_id", "region"}).
			AddRow(env, "us-east-1"))
	expectPoolEnv(mock, env, "amp-shared", "us-east-1", pool, "["+clusterObserved(pool, "c1", caPool)+"]")
	// caOther is not in the pool's observed set, so the env is skipped: no ListTasks.

	got, err := svc.TasksByStoragePool(pool, repository.EnvironmentTaskListOpts{Limit: 25, ClusterID: caOther})
	if err != nil {
		t.Fatalf("TasksByStoragePool: %v", err)
	}
	if len(got.Items) != 0 {
		t.Errorf("got %+v, want nothing for a cluster_id outside the pool", got.Items)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("a cluster_id outside the pool still read tasks: %v", err)
	}
}

// TestTasksByStoragePool_UnmappedPoolIsEmpty: a pool nothing is mapped to returns an
// empty page without error, and the Items slice is non-nil so it serializes as [].
func TestTasksByStoragePool_UnmappedPoolIsEmpty(t *testing.T) {
	svc, mock, done := newEnvSvc(t)
	defer done()

	mock.ExpectQuery(`FROM meko_system\.environment_aeon_account_mapping`).
		WithArgs("aaaaaaaa-1111-2222-3333-444444444444").
		WillReturnRows(sqlmock.NewRows([]string{"environment_id", "region"}))

	got, err := svc.TasksByStoragePool("aaaaaaaa-1111-2222-3333-444444444444",
		repository.EnvironmentTaskListOpts{Limit: 25})
	if err != nil {
		t.Fatalf("TasksByStoragePool: %v", err)
	}
	if got.Items == nil {
		t.Error("Items is nil; it must be an empty slice so it serializes as []")
	}
	if len(got.Items) != 0 || got.Total != 0 || got.HasMore {
		t.Errorf("got %+v, want an empty page", got)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

// TestTasksByStoragePool_NoRepoAndMissingNameGuards: a nil task repo degrades to an
// empty page rather than panicking, and a blank name is a validation error.
func TestTasksByStoragePool_NoRepoAndMissingNameGuards(t *testing.T) {
	svc := NewEnvironmentService(nil, nil)
	if _, err := svc.TasksByStoragePool("", repository.EnvironmentTaskListOpts{}); err == nil {
		t.Error("a blank name must be a validation error")
	}
	got, err := svc.TasksByStoragePool("some-pool", repository.EnvironmentTaskListOpts{Limit: 10})
	if err != nil {
		t.Fatalf("TasksByStoragePool with no repo: %v", err)
	}
	if got.Items == nil {
		t.Error("Items is nil with no repo; it must serialize as []")
	}
}

// TestTasksByStoragePool_DeletedClusterStillListed is CLOUDGA-37425: once the agent
// collects a deleted cluster it leaves the observed inventory, and its tasks -- the
// delete_cluster task among them -- must still be listed, through the pairing
// remembered for the pool. A cluster_id filter naming it must be accepted too.
func TestTasksByStoragePool_DeletedClusterStillListed(t *testing.T) {
	svc, mock, done := newEnvSvc(t)
	defer done()

	const (
		pool   = "aaaaaaaa-1111-2222-3333-444444444444"
		env    = "11111111-1111-1111-1111-111111111111"
		caGone = "66666666-6666-6666-6666-666666666666"
	)
	now := time.Now().UTC()

	mock.ExpectQuery(`FROM meko_system\.environment_aeon_account_mapping`).
		WithArgs(pool).
		WillReturnRows(sqlmock.NewRows([]string{"environment_id", "region"}).
			AddRow(env, "us-east-1"))
	expectPoolEnv(mock, env, "amp-shared", "us-east-1", pool, "[]", caGone)
	mock.ExpectQuery(`SELECT count\(\*\) FROM meko_system\.environment_tasks`).
		WithArgs(env, "delete_cluster", caGone, pq.Array([]string{caGone})).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(1))
	mock.ExpectQuery(`FROM meko_system\.environment_tasks`).
		WithArgs(env, "delete_cluster", caGone, pq.Array([]string{caGone}), tasksMaxMerge, 0).
		WillReturnRows(sqlmock.NewRows(poolTaskCols).
			AddRow("task-D", "delete_cluster", "succeeded", caGone, nil, nil, "", nil, now, now, now))

	got, err := svc.TasksByStoragePool(pool, repository.EnvironmentTaskListOpts{
		Limit: 25, Kind: "delete_cluster", ClusterID: caGone,
	})
	if err != nil {
		t.Fatalf("TasksByStoragePool: %v", err)
	}
	if len(got.Items) != 1 || got.Items[0].TaskID != "task-D" {
		t.Fatalf("items = %+v, want the deleted cluster's delete_cluster task", got.Items)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

// TestApplyStatus_RemembersClusterPools: every cluster in a reported inventory is
// paired with the pool its path starts with, so the pairing outlives the cluster.
// Non-cluster kinds and a cluster without a UUID are not paired.
func TestApplyStatus_RemembersClusterPools(t *testing.T) {
	svc, mock, done := newEnvSvc(t)
	defer done()

	const (
		poolA = "aaaaaaaa-1111-2222-3333-444444444444"
		poolB = "bbbbbbbb-1111-2222-3333-444444444444"
		caA   = "33333333-3333-3333-3333-333333333333"
		caB   = "55555555-5555-5555-5555-555555555555"
	)
	expectHeartbeat(mock)
	mock.ExpectExec(`INSERT INTO meko_system\.environment_cluster_pools`).
		WithArgs(envUUID, pq.Array([]string{caA, caB}), pq.Array([]string{poolA, poolB}), sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 2))

	res, err := svc.ApplyStatus(envUUID, StatusReport{
		EnvironmentState: "READY",
		Resources: []ResourceStatus{
			{Kind: "storage-pool", ID: "storage-pool/" + poolA, Attrs: map[string]string{"id": caB}},
			{Kind: "cluster", ID: "cluster/" + poolA + "/c1", Attrs: map[string]string{"id": caA}},
			{Kind: "cluster", ID: "cluster/" + poolB + "/c9", Attrs: map[string]string{"id": caB}},
			{Kind: "cluster", ID: "cluster/" + poolB + "/pending"},
		},
	})
	if err != nil || !res.Found {
		t.Fatalf("ApplyStatus = %+v, %v", res, err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("the inventory's clusters were not remembered: %v", err)
	}
}
