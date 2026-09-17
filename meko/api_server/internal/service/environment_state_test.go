package service

import (
	"database/sql/driver"
	"errors"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"

	"meko/api_server/internal/repository"
)

// A report with no state is refused before the environment is read, so it costs no
// query -- and, more to the point, cannot refresh the heartbeat. The nil repos are
// the proof: reaching one would panic.
func TestApplyStatus_RejectsAbsentState(t *testing.T) {
	svc := NewEnvironmentService(nil, nil)

	for name, state := range map[string]string{
		"empty":      "",
		"whitespace": "   ",
		"tab":        "\t",
	} {
		t.Run(name, func(t *testing.T) {
			_, err := svc.ApplyStatus(envUUID, StatusReport{EnvironmentState: state})
			if !errors.Is(err, ErrValidation) {
				t.Fatalf("ApplyStatus(%q) error = %v, want ErrValidation", state, err)
			}
		})
	}
}

// The compatibility rule REVIEW.md states for EnvState: the agent is upgraded on the
// customer's schedule, so it can report a state added after this binary shipped. It
// has to land -- refusing it would strand every environment on that agent version,
// since a failed report is only logged and staleness is the sole other signal.
func TestApplyStatus_StoresAStateItDoesNotKnow(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	svc := NewEnvironmentService(repository.NewEnvironmentRepo(db), nil)

	mock.ExpectQuery(`SELECT .* FROM meko_system\.environments e WHERE e\.uuid = \$1`).
		WithArgs(envUUID).
		WillReturnRows(envRow("acme-prod", KindMeko))
	args := anyEnvUpdateArgs()
	args[3] = driver.Value("MIGRATING") // stored verbatim, not mapped or dropped
	mock.ExpectExec(`UPDATE meko_system\.environments SET`).
		WithArgs(args...).
		WillReturnResult(sqlmock.NewResult(0, 1))

	res, err := svc.ApplyStatus(envUUID, StatusReport{EnvironmentState: "MIGRATING"})
	if err != nil {
		t.Fatalf("ApplyStatus with an unknown state = %v, want it accepted", err)
	}
	if !res.Found {
		t.Errorf("Found = false, want true")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("the unknown state did not reach the column: %v", err)
	}
}

// stateIsKnown is what decides whether the report is logged as unfamiliar, so it has
// to track envStates exactly -- a false negative is log noise on every heartbeat.
func TestStateIsKnown(t *testing.T) {
	for _, s := range []string{StateReady, StateDegraded, StatePending, StateDeregistered} {
		if !stateIsKnown(s) {
			t.Errorf("stateIsKnown(%q) = false, want true", s)
		}
	}
	for _, s := range []string{"MIGRATING", "READY_ISH", "", "ready"} {
		if stateIsKnown(s) {
			t.Errorf("stateIsKnown(%q) = true, want false", s)
		}
	}
}

// Every state the wire contract defines has to be accepted, or the control plane
// goes deaf to an agent reporting a legitimate transition. This is the test that
// catches the list drifting behind ycp/internal/api/types.go.
func TestApplyStatus_AcceptsEveryDefinedState(t *testing.T) {
	states := []string{
		StatePending, StateClusterProvisioning, StateEnrolling, StateAttesting,
		StateProvisioning, StateValidating, StateReady, StateDegraded,
		StateSuspended, StateDraining, StateDeregistered,
	}
	if len(states) != len(envStates) {
		t.Fatalf("this test knows %d states but envStates has %d", len(states), len(envStates))
	}
	for _, state := range states {
		t.Run(state, func(t *testing.T) {
			db, mock, err := sqlmock.New()
			if err != nil {
				t.Fatalf("sqlmock: %v", err)
			}
			defer db.Close()
			svc := NewEnvironmentService(repository.NewEnvironmentRepo(db), nil)
			mock.ExpectQuery(`SELECT .* FROM meko_system\.environments e WHERE e\.uuid = \$1`).
				WithArgs(envUUID).
				WillReturnRows(envRow("acme-prod", KindMeko))
			mock.ExpectExec(`UPDATE meko_system\.environments SET`).
				WithArgs(anyEnvUpdateArgs()...).
				WillReturnResult(sqlmock.NewResult(0, 1))

			res, err := svc.ApplyStatus(envUUID, StatusReport{EnvironmentState: state})
			if err != nil {
				t.Fatalf("ApplyStatus(%q) = %v, want accepted", state, err)
			}
			if !res.Found {
				t.Errorf("Found = false, want true")
			}
		})
	}
}

// Case is normalized rather than refused: the console filters on the column, so
// what lands there has to be the canonical spelling.
func TestApplyStatus_NormalizesStateCase(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	svc := NewEnvironmentService(repository.NewEnvironmentRepo(db), nil)

	mock.ExpectQuery(`SELECT .* FROM meko_system\.environments e WHERE e\.uuid = \$1`).
		WithArgs(envUUID).
		WillReturnRows(envRow("acme-prod", KindMeko))
	args := anyEnvUpdateArgs()
	args[3] = driver.Value(StateReady) // the state column
	mock.ExpectExec(`UPDATE meko_system\.environments SET`).
		WithArgs(args...).
		WillReturnResult(sqlmock.NewResult(0, 1))

	if _, err := svc.ApplyStatus(envUUID, StatusReport{EnvironmentState: " ready "}); err != nil {
		t.Fatalf("ApplyStatus: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("state did not reach the column as %q: %v", StateReady, err)
	}
}

// A bootstrap phase is an EnvState on the wire, so it gets the same check --
// without it this route wrote any string an installer chose to send.
func TestSetState_RejectsAnAbsentPhase(t *testing.T) {
	svc := NewEnvironmentService(nil, nil)

	for _, phase := range []string{"", "   ", "\t"} {
		if _, err := svc.SetState(envUUID, phase); !errors.Is(err, ErrValidation) {
			t.Errorf("SetState(%q) error = %v, want ErrValidation", phase, err)
		}
	}
}

func TestSetState_AcceptsADefinedPhase(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	svc := NewEnvironmentService(repository.NewEnvironmentRepo(db), nil)

	mock.ExpectQuery(`SELECT .* FROM meko_system\.environments e WHERE e\.uuid = \$1`).
		WithArgs(envUUID).
		WillReturnRows(envRow("acme-prod", KindMeko))
	args := anyEnvUpdateArgs()
	args[3] = driver.Value(StateClusterProvisioning)
	mock.ExpectExec(`UPDATE meko_system\.environments SET`).
		WithArgs(args...).
		WillReturnResult(sqlmock.NewResult(0, 1))

	found, err := svc.SetState(envUUID, StateClusterProvisioning)
	if err != nil || !found {
		t.Fatalf("SetState = (%v, %v), want (true, nil)", found, err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("the phase did not reach the column: %v", err)
	}
}
