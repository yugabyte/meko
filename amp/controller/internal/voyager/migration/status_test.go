package migration

import (
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestDeriveStatusPrecedence(t *testing.T) {
	now := time.Date(2026, time.July, 27, 0, 0, 0, 0, time.UTC)
	failedAssessment := Run{
		Operation: OperationAssess,
		Attempt:   1,
		Status:    RunFailed,
		UpdatedAt: now,
	}
	failedMigration := Run{
		Operation: OperationMigrate,
		Attempt:   1,
		Status:    RunFailed,
		UpdatedAt: now,
	}
	runningAssessment := Run{
		Operation: OperationAssess,
		Attempt:   1,
		Status:    RunRunning,
		UpdatedAt: now,
	}
	runningMigration := Run{
		Operation: OperationMigrate,
		Attempt:   1,
		Status:    RunRunning,
		UpdatedAt: now,
	}
	succeededAssessment := Run{
		Operation: OperationAssess,
		Attempt:   1,
		Status:    RunSucceeded,
		UpdatedAt: now,
	}
	succeededMigration := Run{
		Operation: OperationMigrate,
		Attempt:   1,
		Status:    RunSucceeded,
		UpdatedAt: now,
	}
	runningEnd := Run{
		Operation: OperationEnd,
		Attempt:   1,
		Status:    RunRunning,
		UpdatedAt: now,
	}
	succeededEnd := Run{
		Operation: OperationEnd,
		Attempt:   1,
		Status:    RunSucceeded,
		UpdatedAt: now,
	}
	failedEnd := Run{
		Operation: OperationEnd,
		Attempt:   1,
		Status:    RunFailed,
		UpdatedAt: now,
	}
	targetTaskID := uuid.New()

	tests := []struct {
		name string
		m    Migration
		want Status
	}{
		{
			name: "end in flight takes precedence over completed migration",
			m:    Migration{Runs: []Run{succeededMigration, runningEnd}},
			want: Status{State: StateEnding, Detail: "ending migration"},
		},
		{
			name: "in-flight migration takes precedence over failed end",
			m:    Migration{Runs: []Run{failedEnd, runningMigration}},
			want: Status{State: StateMigrating, Detail: "exporting schema"},
		},
		{
			name: "successful end is terminal",
			m:    Migration{Runs: []Run{succeededMigration, succeededEnd}},
			want: Status{State: StateEnded, Detail: "migration ended"},
		},
		{
			name: "failed end takes precedence over completed migration",
			m:    Migration{Runs: []Run{succeededMigration, failedEnd}},
			want: Status{State: StateFailed, Detail: "ending migration failed"},
		},
		{
			name: "in-flight migration takes precedence over a failed assessment",
			m:    Migration{Runs: []Run{failedAssessment, runningMigration}},
			want: Status{State: StateMigrating, Detail: "exporting schema"},
		},
		{
			name: "successful migration takes precedence over a failed assessment",
			m:    Migration{Runs: []Run{failedAssessment, succeededMigration}},
			want: Status{State: StateCompleted, Detail: "migration complete"},
		},
		{
			name: "latest failed migration",
			m:    Migration{Runs: []Run{failedMigration}},
			want: Status{State: StateFailed, Detail: "migration failed"},
		},
		{
			name: "failed assessment",
			m:    Migration{Runs: []Run{failedAssessment}},
			want: Status{State: StateFailed, Detail: "assessment failed"},
		},
		{
			name: "in-flight assessment",
			m:    Migration{Runs: []Run{runningAssessment}},
			want: Status{State: StateAssessing, Detail: "assessing schema"},
		},
		{
			name: "target provisioning",
			m:    Migration{TargetTaskID: &targetTaskID},
			want: Status{State: StateProvisioningTarget, Detail: "provisioning PG compute"},
		},
		{
			name: "attached target",
			m:    Migration{Target: &TargetConfig{}},
			want: Status{State: StateTargetReady, Detail: "ready to migrate"},
		},
		{
			name: "completed assessment",
			m:    Migration{Runs: []Run{succeededAssessment}},
			want: Status{State: StateAssessed, Detail: "assessment complete"},
		},
		{
			name: "new migration",
			m:    Migration{},
			want: Status{State: StateCreated, Detail: "awaiting next step"},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := DeriveStatus(test.m); got != test.want {
				t.Fatalf("DeriveStatus() = %+v, want %+v", got, test.want)
			}
		})
	}
}

func TestLatestOperationRunsIgnoresSliceOrder(t *testing.T) {
	end := Run{Operation: OperationEnd, Attempt: 1, Status: RunSucceeded}
	migrate := Run{Operation: OperationMigrate, Attempt: 1, Status: RunSucceeded}
	_, _, got := LatestOperationRuns([]Run{end, migrate})
	if got == nil || got.Operation != OperationEnd {
		t.Fatalf("end run = %+v, want end", got)
	}
}

func TestDeriveStatusUsesLatestAttempt(t *testing.T) {
	got := DeriveStatus(Migration{Runs: []Run{
		{Operation: OperationAssess, Attempt: 1, Status: RunFailed},
		{Operation: OperationAssess, Attempt: 2, Status: RunRunning},
	}})
	want := Status{State: StateAssessing, Detail: "assessing schema"}
	if got != want {
		t.Fatalf("DeriveStatus() = %+v, want %+v", got, want)
	}
}

func TestDeriveStatusUsesErrorSummary(t *testing.T) {
	summary := "source database authentication failed"
	got := DeriveStatus(Migration{Runs: []Run{{
		Operation:    OperationAssess,
		Attempt:      1,
		Status:       RunFailed,
		ErrorSummary: &summary,
	}}})
	want := Status{State: StateFailed, Detail: summary}
	if got != want {
		t.Fatalf("DeriveStatus() = %+v, want %+v", got, want)
	}
}

func TestDeriveStatusPendingRuns(t *testing.T) {
	tests := []struct {
		name string
		run  Run
		want Status
	}{
		{
			name: "pending assessment",
			run:  Run{Operation: OperationAssess, Attempt: 1, Status: RunPending},
			want: Status{State: StateAssessing, Detail: "starting assessment"},
		},
		{
			name: "pending migration",
			run:  Run{Operation: OperationMigrate, Attempt: 1, Status: RunPending},
			want: Status{State: StateMigrating, Detail: "starting migration"},
		},
		{
			name: "pending end",
			run:  Run{Operation: OperationEnd, Attempt: 1, Status: RunPending},
			want: Status{State: StateEnding, Detail: "starting end migration"},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := DeriveStatus(Migration{Runs: []Run{test.run}}); got != test.want {
				t.Fatalf("DeriveStatus() = %+v, want %+v", got, test.want)
			}
		})
	}
}

func TestMigrateDetail(t *testing.T) {
	running := Run{Operation: OperationMigrate, Status: RunRunning}
	tests := []struct {
		name    string
		voyager *VoyagerStatus
		want    string
	}{
		{
			name: "export schema",
			want: "exporting schema",
		},
		{
			name: "import schema",
			voyager: &VoyagerStatus{Phases: []Phase{
				{Phase: PhaseExportSchema, Status: PhaseCompleted},
			}},
			want: "importing schema",
		},
		{
			name: "export data with row progress",
			voyager: &VoyagerStatus{
				Phases: []Phase{
					{Phase: PhaseExportSchema, Status: PhaseCompleted},
					{Phase: PhaseImportSchema, Status: PhaseCompleted},
				},
				DataProgress: &DataProgress{
					Export: &PhaseDataProgress{RowsCompleted: 4_103, RowsTotal: 6_704},
				},
			},
			want: "exporting data (4,103/6,704 rows)",
		},
		{
			name: "import data with row progress",
			voyager: &VoyagerStatus{
				Phases: []Phase{
					{Phase: PhaseExportSchema, Status: PhaseCompleted},
					{Phase: PhaseImportSchema, Status: PhaseCompleted},
					{Phase: PhaseExportData, Status: PhaseCompleted},
				},
				DataProgress: &DataProgress{
					Import: &PhaseDataProgress{RowsCompleted: 4_103, RowsTotal: 6_704},
				},
			},
			want: "importing data (4,103/6,704 rows)",
		},
		{
			name: "finalize schema",
			voyager: &VoyagerStatus{Phases: []Phase{
				{Phase: PhaseExportSchema, Status: PhaseCompleted},
				{Phase: PhaseImportSchema, Status: PhaseCompleted},
				{Phase: PhaseExportData, Status: PhaseCompleted},
				{Phase: PhaseImportData, Status: PhaseCompleted},
			}},
			want: "finalizing schema (indexes, triggers)",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := migrateDetail(&running, test.voyager); got != test.want {
				t.Fatalf("migrateDetail() = %q, want %q", got, test.want)
			}
		})
	}
}

func TestFormatCount(t *testing.T) {
	tests := map[int64]string{
		-6_704: "-6,704",
		-1:     "-1",
		0:      "0",
		1:      "1",
		999:    "999",
		1_000:  "1,000",
		6_704:  "6,704",
	}
	for input, want := range tests {
		if got := formatCount(input); got != want {
			t.Errorf("formatCount(%d) = %q, want %q", input, got, want)
		}
	}
}

func TestPhaseNumberName(t *testing.T) {
	tests := map[PhaseNumber]string{
		PhaseAssess:       "assess",
		PhaseExportSchema: "export-schema",
		PhaseAnalyze:      "analyze-schema",
		PhaseExportData:   "export-data",
		PhaseImportSchema: "import-schema",
		PhaseImportData:   "import-data",
		99:                "",
	}
	for phase, want := range tests {
		if got := phase.Name(); got != want {
			t.Errorf("PhaseNumber(%d).Name() = %q, want %q", phase, got, want)
		}
	}
}
