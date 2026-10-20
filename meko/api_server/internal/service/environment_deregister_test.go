package service

import (
	"database/sql/driver"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
)

const staleInventory = `[{"kind":"cluster","id":"cluster/pool-a/c1","state":"ready"}]`

// inventoryEnvRow is an environment holding an agent-reported inventory.
func inventoryEnvRow(state string, live bool) *sqlmock.Rows {
	return sqlmock.NewRows(envRowCols).AddRow(
		envUUID, testEnvID, acctA, "cloud", "us-east-1", state, "", 0, "",
		0, 400, "", nil, "", "", "[]", live, keyA, time.Now(), KindAmp, staleInventory, nil, "BYOC", false,
	)
}

// CLOUDGA-37659: the agent goes with the data plane, so nothing would ever replace
// the inventory it last reported. Deregistering has to drop it and the live flag.
func TestAction_DeregisterClearsInventoryAndLive(t *testing.T) {
	svc, mock, done := newEnvSvc(t)
	defer done()

	expectScopedGet(mock, acctA, inventoryEnvRow(StateReady, true))
	args := anyEnvUpdateArgs()
	args[3] = driver.Value(StateDeregistered)
	args[14] = driver.Value(false)        // live
	args[16] = driver.Value([]byte("[]")) // resources
	mock.ExpectExec(`UPDATE meko_system\.environments SET`).
		WithArgs(args...).
		WillReturnResult(sqlmock.NewResult(0, 1))

	e, err := svc.Action(testEnvID, acctA, "deregister", false)
	if err != nil || e == nil {
		t.Fatalf("Action(deregister) = (%v, %v), want the environment", e, err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Error(err)
	}
}

// Deregistering clears live; a resumed environment's agent restores it by reporting,
// without having to re-enroll.
func TestApplyStatus_MarksTheEnvironmentLive(t *testing.T) {
	svc, mock, done := newEnvSvc(t)
	defer done()

	mock.ExpectQuery(`SELECT .* FROM meko_system\.environments e WHERE e\.uuid = \$1`).
		WithArgs(envUUID).
		WillReturnRows(inventoryEnvRow(StateReady, false))
	args := anyEnvUpdateArgs()
	args[14] = driver.Value(true)
	mock.ExpectExec(`UPDATE meko_system\.environments SET`).
		WithArgs(args...).
		WillReturnResult(sqlmock.NewResult(0, 1))

	if _, err := svc.ApplyStatus(envUUID, StatusReport{EnvironmentState: StateReady}); err != nil {
		t.Fatalf("ApplyStatus: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Error(err)
	}
}

// YBM may not have revoked a deregistered environment's key, and its agent may still
// be running. Every agent entry point must refuse it and write nothing, or the agent
// reports the environment back to life. sqlmock fails any write not expected here.
func TestAgentOfDeregisteredEnvironmentIsRefused(t *testing.T) {
	svc, mock, done := newEnvSvc(t)
	defer done()
	byKey := func() {
		mock.ExpectQuery(`FROM meko_system\.environment_api_keys WHERE api_key_id = \$1`).
			WithArgs(keyA).
			WillReturnRows(inventoryEnvRow(StateDeregistered, false))
	}
	byUUID := func() {
		mock.ExpectQuery(`SELECT .* FROM meko_system\.environments e WHERE e\.uuid = \$1`).
			WithArgs(envUUID).
			WillReturnRows(inventoryEnvRow(StateDeregistered, false))
	}

	byKey()
	if e, err := svc.ResolveAgent(keyA); err != nil || e != nil {
		t.Errorf("ResolveAgent = (%v, %v), want (nil, nil): the handler's 401", e, err)
	}

	byKey()
	if e, err := svc.EnrollByKey(keyA, "", KindAmp, ClusterFacts{}, nil); err != nil || e != nil {
		t.Errorf("EnrollByKey = (%v, %v), want (nil, nil): the handler's 401", e, err)
	}

	// A deregister landing between ResolveAgent and the write.
	byUUID()
	if res, err := svc.ApplyStatus(envUUID, StatusReport{EnvironmentState: StateReady}); err != nil || res.Found {
		t.Errorf("ApplyStatus = (%+v, %v), want not found", res, err)
	}

	byUUID()
	if found, err := svc.SetState(envUUID, StateProvisioning); err != nil || found {
		t.Errorf("SetState = (%v, %v), want not found", found, err)
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Error(err)
	}
}

// A deregister committing after the agent's write read the row: the write is guarded
// in SQL, matches nothing, and the report is answered as for an unknown environment.
func TestAgentWriteLosesToAConcurrentDeregister(t *testing.T) {
	svc, mock, done := newEnvSvc(t)
	defer done()
	readThenLose := func() {
		mock.ExpectQuery(`SELECT .* FROM meko_system\.environments e WHERE e\.uuid = \$1`).
			WithArgs(envUUID).
			WillReturnRows(inventoryEnvRow(StateReady, true))
		mock.ExpectExec(`UPDATE meko_system\.environments SET .* WHERE uuid = \$1 AND state <> 'DEREGISTERED'`).
			WithArgs(anyEnvUpdateArgs()...).
			WillReturnResult(sqlmock.NewResult(0, 0))
	}

	readThenLose()
	res, err := svc.ApplyStatus(envUUID, StatusReport{
		EnvironmentState: StateReady,
		Resources:        []ResourceStatus{{Kind: "cluster", ID: "cluster/pool-a/c1"}},
	})
	if err != nil || res.Found {
		t.Errorf("ApplyStatus = (%+v, %v), want not found", res, err)
	}

	readThenLose()
	if found, err := svc.SetState(envUUID, StateProvisioning); err != nil || found {
		t.Errorf("SetState = (%v, %v), want not found", found, err)
	}

	// Nothing after the lost write: no cluster-pool or task rows for a dead environment.
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Error(err)
	}
}
