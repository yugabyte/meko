package migration

import (
	"fmt"
	"strconv"
	"strings"
)

// DeriveStatus returns the migration's current state from its persisted
// migration/run data and its optional Voyager progress projection. The order of
// the checks is intentional: any in-flight run (assess, migrate, or end) takes
// precedence over terminal facts, so get_migration always reflects work that is
// actually happening. Terminal end facts (ended/failed) outrank a completed
// migrate, but a new in-flight assess or migrate — which the handler blocks
// once an end run exists — would still surface correctly.
func DeriveStatus(m Migration) Status {
	assess, migrate, endRun := LatestOperationRuns(m.Runs)

	// Any in-flight run takes precedence over everything else, regardless of
	// operation, so polling always reflects work actually happening.
	if endRun != nil && endRun.Status.InFlight() {
		if endRun.Status == RunPending {
			return Status{State: StateEnding, Detail: "starting end migration"}
		}
		return Status{State: StateEnding, Detail: "ending migration"}
	}
	if migrate != nil && migrate.Status.InFlight() {
		return Status{State: StateMigrating, Detail: migrateDetail(migrate, m.Voyager)}
	}
	if assess != nil && assess.Status.InFlight() {
		if assess.Status == RunPending {
			return Status{State: StateAssessing, Detail: "starting assessment"}
		}
		return Status{State: StateAssessing, Detail: "assessing schema"}
	}

	// Terminal end facts outrank a completed migrate — once an end run reaches
	// a terminal state, that is the authoritative answer.
	if endRun != nil && endRun.Status == RunSucceeded {
		return Status{State: StateEnded, Detail: "migration ended"}
	}
	if endRun != nil && endRun.Status == RunFailed {
		return failedRunStatus(endRun)
	}

	if migrate != nil && migrate.Status == RunSucceeded {
		return Status{State: StateCompleted, Detail: "migration complete"}
	}
	if failed := latestFailedRun(assess, migrate); failed != nil {
		return failedRunStatus(failed)
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

// LatestOperationRuns returns the newest assess, migrate, and end run in runs.
// A missing operation is nil. Selection follows attempt, then UpdatedAt, so the
// result does not depend on slice order.
func LatestOperationRuns(runs []Run) (assess, migrate, endRun *Run) {
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
		case OperationEnd:
			if endRun == nil || laterRun(run, endRun) {
				endRun = run
			}
		}
	}
	return assess, migrate, endRun
}

func laterRun(candidate, current *Run) bool {
	if candidate.Attempt != current.Attempt {
		return candidate.Attempt > current.Attempt
	}
	return candidate.UpdatedAt.After(current.UpdatedAt)
}

func latestFailedRun(runs ...*Run) *Run {
	var failed *Run
	for _, run := range runs {
		if run == nil || run.Status != RunFailed {
			continue
		}
		if failed == nil || run.UpdatedAt.After(failed.UpdatedAt) {
			failed = run
		}
	}
	return failed
}

func failedRunStatus(run *Run) Status {
	detail := "migration failed"
	if run.Operation == OperationAssess {
		detail = "assessment failed"
	} else if run.Operation == OperationEnd {
		detail = "ending migration failed"
	}
	if run.ErrorSummary != nil && *run.ErrorSummary != "" {
		detail = *run.ErrorSummary
	}
	return Status{State: StateFailed, Detail: detail}
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
