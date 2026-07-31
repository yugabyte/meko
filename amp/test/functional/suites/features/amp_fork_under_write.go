package features

import (
	"context"
	"fmt"
	"time"

	"github.com/yugabyte/amp/test/functional/framework/core"
)

const (
	fuwTableA = "amp_fuw_settled"
	fuwTableB = "amp_fuw_stream"
	// fuwSettledRows is the fully-committed dataset the child must see exactly.
	fuwSettledRows = 500
	// fuwMinStreamed is the minimum committed stream rows before the fork LSN is
	// captured, so the prefix assertion is non-trivial.
	fuwMinStreamed = 25
)

func init() {
	core.Register(core.Test{
		Name:        "features/amp_fork_under_write",
		Suite:       "features",
		Category:    core.Stress,
		Env:         core.EnvSpec{Sharing: core.Exclusive, Shape: core.SharedControlPlane},
		Tags:        []string{"regression", "nightly"},
		OwningTeam:  core.TeamControlPlane,
		Description: "Fork a branch while a writer streams sequential commits; the child must see the settled table exactly and a clean prefix (count == max id) of the stream, never a torn set.",
		Params:      []string{"functional", "env.ampstack", "apps.ybastack", "apps.yba", "apps.ybk8s"},
		Run:         runAmpForkUnderWrite,
	})
}

func runAmpForkUnderWrite(t *core.T) error {
	return runAmpFeature(t, func(a *ampFeatureRun) []phase {
		return []phase{{"fork_under_write", a.forkUnderWrite}}
	})
}

func (a *ampFeatureRun) forkUnderWrite(ctx context.Context) error {
	cl, err := a.f.controllerClient(ctx)
	if err != nil {
		return err
	}
	return forkUnderWriteSteps(ctx, cl, a.proj, "fork-under-write-"+a.f.suffix(), a.f.endpointSQL)
}

type streamResult struct {
	committed int
	err       error
}

func forkUnderWriteSteps(ctx context.Context, cl ampCtl, proj ampProject, childName string, connect endpointConnector) error {
	baseEP, err := createEndpoint(ctx, cl, proj.poolID, proj.projID, proj.branchID)
	if err != nil {
		return fmt.Errorf("create base endpoint: %w", err)
	}
	base, err := connect(ctx, baseEP)
	if err != nil {
		return fmt.Errorf("connect base endpoint: %w", err)
	}
	defer base.Close()

	if _, err := base.Exec(ctx, "CREATE TABLE "+fuwTableA+" (id int primary key)"); err != nil {
		return fmt.Errorf("create settled table: %w", err)
	}
	if _, err := base.Exec(ctx, fmt.Sprintf("INSERT INTO %s SELECT generate_series(1, %d)", fuwTableA, fuwSettledRows)); err != nil {
		return fmt.Errorf("seed settled table: %w", err)
	}
	if _, err := base.Exec(ctx, "CREATE TABLE "+fuwTableB+" (id int primary key)"); err != nil {
		return fmt.Errorf("create stream table: %w", err)
	}

	// Stream sequential single-statement commits on a SECOND connection.
	writer, err := connect(ctx, baseEP)
	if err != nil {
		return fmt.Errorf("connect writer: %w", err)
	}
	defer writer.Close()
	stop := make(chan struct{})
	done := make(chan streamResult, 1)
	go func() {
		id := 0
		for {
			select {
			case <-stop:
				done <- streamResult{committed: id}
				return
			default:
			}
			if _, err := writer.Exec(ctx, "INSERT INTO "+fuwTableB+" (id) VALUES ($1)", id+1); err != nil {
				done <- streamResult{committed: id, err: err}
				return
			}
			id++
		}
	}()
	stopWriter := func() streamResult {
		select {
		case <-stop:
		default:
			close(stop)
		}
		return <-done
	}

	// Capture the fork LSN mid-stream, once enough rows are committed.
	if err := pollUntil(ctx, 60*time.Second, 200*time.Millisecond, func() (bool, error) {
		n, err := base.QueryInt(ctx, "SELECT count(*) FROM "+fuwTableB)
		return n >= fuwMinStreamed, err
	}); err != nil {
		res := stopWriter()
		if res.err != nil {
			return fmt.Errorf("stream writer failed: %w", res.err)
		}
		return fmt.Errorf("stream never reached %d rows: %w", fuwMinStreamed, err)
	}
	forkLSN, err := currentLSN(ctx, base)
	if err != nil {
		stopWriter()
		return err
	}

	child, err := forkChildAt(ctx, cl, proj, proj.branchID, forkLSN, childName, connect)
	res := stopWriter()
	if err != nil {
		return err
	}
	defer child.Close()
	if res.err != nil {
		return fmt.Errorf("stream writer failed: %w", res.err)
	}

	// The settled table is exact on the child.
	if n, err := child.QueryInt(ctx, "SELECT count(*) FROM "+fuwTableA); err != nil {
		return fmt.Errorf("read settled table on child: %w", err)
	} else if n != fuwSettledRows {
		return fmt.Errorf("child sees %d rows in the fully-committed settled table %s, want exactly %d (all of it was committed before the fork, so the fork must copy it exactly)", n, fuwTableA, fuwSettledRows)
	}

	// The stream table is a clean prefix: count == max(id), bounded by what the
	// writer had committed at LSN capture and in total.
	count, err := child.QueryInt(ctx, "SELECT count(*) FROM "+fuwTableB)
	if err != nil {
		return fmt.Errorf("count stream on child: %w", err)
	}
	maxID, err := child.QueryInt(ctx, "SELECT coalesce(max(id), 0) FROM "+fuwTableB)
	if err != nil {
		return fmt.Errorf("max stream id on child: %w", err)
	}
	if count != maxID {
		return fmt.Errorf("fork saw a torn write on %s: row count=%d but max(id)=%d, want them equal (ids are inserted 1..N with no gaps, so a mismatch means the child captured a partially-applied write instead of a clean prefix of the stream)", fuwTableB, count, maxID)
	}
	if count < fuwMinStreamed {
		return fmt.Errorf("fork lost committed stream rows: child sees %d rows on %s, but at least %d were committed on the writer before the fork LSN was captured", count, fuwTableB, fuwMinStreamed)
	}
	if count > int64(res.committed) {
		return fmt.Errorf("fork sees %d rows on %s, more than the writer's %d total committed rows (the child must never see writes beyond what was actually committed)", count, fuwTableB, res.committed)
	}
	return nil
}
