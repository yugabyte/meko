package migration

import (
	"fmt"
	"strconv"
	"strings"
)

// DeriveStatus returns the migration's current state from its persisted
// migration/run data and its optional Voyager progress projection. The order of
// the checks is intentional: an in-flight or successful migrate run takes
// precedence over an earlier failed assessment.
func DeriveStatus(m Migration) Status {
	assess, migrate := latestOperationRuns(m.Runs)

	if migrate != nil && migrate.Status.InFlight() {
		return Status{State: StateMigrating, Detail: migrateDetail(migrate, m.Voyager)}
	}
	if migrate != nil && migrate.Status == RunSucceeded {
		return Status{State: StateCompleted, Detail: "migration complete"}
	}
	if failed := latestFailedRun(assess, migrate); failed != nil {
		detail := "migration failed"
		if failed.Operation == OperationAssess {
			detail = "assessment failed"
		}
		if failed.ErrorSummary != nil && *failed.ErrorSummary != "" {
			detail = *failed.ErrorSummary
		}
		return Status{State: StateFailed, Detail: detail}
	}
	if assess != nil && assess.Status.InFlight() {
		if assess.Status == RunPending {
			return Status{State: StateAssessing, Detail: "starting assessment"}
		}
		return Status{State: StateAssessing, Detail: "assessing schema"}
	}
	if m.TargetTaskID != nil && m.Target == nil {
		return Status{State: StateProvisioningTarget, Detail: "provisioning PG compute"}
	}
	if m.Target != nil {
		return Status{State: StateTargetReady, Detail: "ready to migrate"}
	}
	if assess != nil && assess.Status == RunSucceeded {
		return Status{State: StateAssessed, Detail: "assessment complete"}
	}
	return Status{State: StateCreated, Detail: "awaiting next step"}
}

func latestOperationRuns(runs []Run) (assess, migrate *Run) {
	for i := range runs {
		run := &runs[i]
		switch run.Operation {
		case OperationAssess:
			if assess == nil || laterRun(run, assess) {
				assess = run
			}
		case OperationMigrate:
			if migrate == nil || laterRun(run, migrate) {
				migrate = run
			}
		}
	}
	return assess, migrate
}

func laterRun(candidate, current *Run) bool {
	if candidate.Attempt != current.Attempt {
		return candidate.Attempt > current.Attempt
	}
	return candidate.UpdatedAt.After(current.UpdatedAt)
}

func latestFailedRun(assess, migrate *Run) *Run {
	var failed *Run
	for _, run := range []*Run{assess, migrate} {
		if run == nil || run.Status != RunFailed {
			continue
		}
		if failed == nil || run.UpdatedAt.After(failed.UpdatedAt) {
			failed = run
		}
	}
	return failed
}

// migrateDetail follows the command sequence. Voyager's visualizer reports
// phase progress, while the runner reports whether the job itself is still
// pending/running.
func migrateDetail(run *Run, voyager *VoyagerStatus) string {
	if run.Status == RunPending {
		return "starting migration"
	}

	phases := make(map[PhaseNumber]PhaseStatus)
	if voyager != nil {
		for _, phase := range voyager.Phases {
			phases[phase.Phase] = phase.Status
		}
	}
	if phases[PhaseExportSchema] != PhaseCompleted {
		return "exporting schema"
	}
	if phases[PhaseImportSchema] != PhaseCompleted {
		return "importing schema"
	}
	if phases[PhaseExportData] != PhaseCompleted {
		return progressDetail("exporting data", dataPhase(voyager, PhaseExportData))
	}
	if phases[PhaseImportData] != PhaseCompleted {
		return progressDetail("importing data", dataPhase(voyager, PhaseImportData))
	}
	return "finalizing schema (indexes, triggers)"
}

func dataPhase(voyager *VoyagerStatus, phase PhaseNumber) *PhaseDataProgress {
	if voyager == nil || voyager.DataProgress == nil {
		return nil
	}
	switch phase {
	case PhaseExportData:
		return voyager.DataProgress.Export
	case PhaseImportData:
		return voyager.DataProgress.Import
	default:
		return nil
	}
}

func progressDetail(prefix string, progress *PhaseDataProgress) string {
	if progress == nil || progress.RowsTotal <= 0 {
		return prefix
	}
	return fmt.Sprintf("%s (%s/%s rows)", prefix,
		formatCount(progress.RowsCompleted), formatCount(progress.RowsTotal))
}

func formatCount(n int64) string {
	text := strconv.FormatInt(n, 10)
	start := 0
	if strings.HasPrefix(text, "-") {
		start = 1
	}
	for i := len(text) - 3; i > start; i -= 3 {
		text = text[:i] + "," + text[i:]
	}
	return text
}
