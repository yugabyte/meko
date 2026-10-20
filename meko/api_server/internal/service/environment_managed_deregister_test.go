package service

import (
	"errors"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
)

// managedEnvRow is testEnvID as a READY MANAGED amp environment. accountID is empty for
// the usual operator registration; an operator may also stamp one with an account.
func managedEnvRow(accountID string) *sqlmock.Rows {
	return sqlmock.NewRows(envRowCols).AddRow(
		envUUID, testEnvID, accountID, "cloud", "us-east-1", "READY", "", 0, "",
		0, 400, "", nil, "", "", "[]", true, keyA, time.Now(), KindAmp, "[]", nil, TypeManaged, false,
	)
}

// A refused case registers only the lookup, so the strict mock fails it if anything is
// written.
func TestAction_ManagedDeregisterGuard(t *testing.T) {
	cases := []struct {
		wantErr   error
		name      string
		scope     AccountScope
		action    string
		wantState string
		managed   bool
		force     bool
	}{
		{name: "managed, no force", managed: true, action: "deregister", wantErr: ErrManagedNeedsForce},
		{name: "managed, force", managed: true, action: "deregister", force: true, wantState: StateDeregistered},
		{
			name: "managed, scoped caller with force", scope: acctA, managed: true, action: "deregister",
			force: true, wantErr: ErrManagedRequiresOperator,
		},
		{
			name: "managed, scoped caller without force", scope: acctA, managed: true, action: "deregister",
			wantErr: ErrManagedRequiresOperator,
		},
		{name: "byoc needs no force", action: "deregister", wantState: StateDeregistered},
		{name: "managed drain needs no force", managed: true, action: "drain", wantState: StateDraining},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			svc, mock, done := newEnvSvc(t)
			defer done()

			rows := envRows()
			if tc.managed {
				rows = managedEnvRow(string(tc.scope))
			}
			if tc.scope == "" {
				mock.ExpectQuery(`FROM meko_system\.environments e WHERE e\.id = \$1 LIMIT 2`).
					WithArgs(testEnvID).
					WillReturnRows(rows)
			} else {
				expectScopedGet(mock, string(tc.scope), rows)
			}
			if tc.wantErr == nil {
				mock.ExpectExec(`UPDATE meko_system\.environments SET`).
					WillReturnResult(sqlmock.NewResult(0, 1))
			}

			e, err := svc.Action(testEnvID, tc.scope, tc.action, tc.force)
			switch {
			case tc.wantErr != nil:
				if !errors.Is(err, tc.wantErr) || e != nil {
					t.Fatalf("Action = (%v, %v), want (nil, %v)", e, err, tc.wantErr)
				}
			case err != nil || e == nil || e.State != tc.wantState:
				t.Fatalf("Action = (%v, %v), want state %s", e, err, tc.wantState)
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Errorf("unmet or unexpected statements: %v", err)
			}
		})
	}
}
