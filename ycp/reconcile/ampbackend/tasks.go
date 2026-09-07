package ampbackend

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"time"

	"meko/ycp/internal/ampclient"
	"meko/ycp/internal/api"
)

// backfillWindow is how far back the first sweep reaches. The feed is ordered
// updated_at ASC, so a zero watermark would start at the OLDEST task in the log and
// crawl forward — the opposite of what a console opening on recent history needs.
// Starting one window back gets recent tasks immediately and, more importantly,
// covers the tasks that moved while the agent was down: a resize that failed during
// a restart is exactly the event an operator goes looking for.
//
// The control plane keeps its own history, so this is a gap-filler on restart, not
// the history itself. Overlapping what the control plane already holds is free — the
// upsert is idempotent.
const backfillWindow = 24 * time.Hour

// feedPageLimit is one page of the change feed. The controller clamps limit to
// 1..1000; this asks for the ceiling because a tick's delta is normally a handful of
// rows and a large page costs nothing when it comes back nearly empty.
const feedPageLimit = 1000

// maxFeedPages bounds one sweep so a long backfill cannot stall a reconcile tick.
// Stopping early loses nothing: the feed is ascending, so the unread rows all sort
// at or after the watermark this sweep advanced to, and the next tick resumes there.
// What was truncated is logged rather than dropped silently.
const maxFeedPages = 10

// TaskCollector reads the amp controller's task log as a change feed and returns,
// each tick, the tasks whose status has moved since the previous one.
//
// It exists because the task log is the only place that knows why an operation is
// slow or has failed, and it lives inside the customer's cluster. The agent already
// relays inventory; this relays the operations behind it.
//
// Why a change feed rather than polling non-terminal tasks: a task created and
// finished inside one reconcile interval is never observed while pending or running,
// so watching only in-flight tasks would never see it at all — not even as
// succeeded. `create_branch` and `add_branch_extensions` routinely finish that fast.
// Reading by updated_at catches them, because a task that changed at all changed its
// updated_at.
//
// Not safe for concurrent use: one collector belongs to one reconcile loop, which is
// single-threaded per tick.
type TaskCollector struct {
	api *ampclient.Client
	now func() time.Time

	// watermark is the greatest updated_at relayed so far. Advanced only over rows
	// actually returned, so a failed sweep is retried rather than skipped.
	watermark time.Time
	started   bool
	// reported suppresses a re-read of the inclusive bound's boundary rows. Bounded
	// by how many distinct tasks one environment produces over the agent's lifetime;
	// a long-lived agent in a very busy environment is the case to revisit.
	reported map[string]taskFingerprint
	// feedSupported goes false once the controller is seen to ignore updated_since.
	feedSupported bool
	probed        bool

	logf func(string, ...any)
}

type taskFingerprint struct {
	status    string
	updatedAt time.Time
}

// NewTaskCollector returns a collector over the same controller b talks to, so it
// inherits b's base URL and credentials.
func NewTaskCollector(b *Backend, logf func(string, ...any)) *TaskCollector {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	return &TaskCollector{
		api:           b.api,
		now:           time.Now,
		reported:      map[string]taskFingerprint{},
		feedSupported: true,
		logf:          logf,
	}
}

// Collect returns the tasks whose status moved since the last call — a delta, not a
// snapshot, so a quiet tick returns nil — and a commit func the caller MUST call
// once those tasks have been accepted by the control plane, and must NOT call
// otherwise.
//
// The two-phase shape exists because a delta is only delivered once. Advancing the
// watermark (and the seen-fingerprints, which is what suppresses a re-read) at
// collect time would drop the delta whenever the status POST that carries it fails:
// the next sweep re-reads those rows and dedupes them straight back out, so a
// transient control-plane outage would permanently lose every task transition inside
// it. Committing only on success turns that into a re-send.
//
// commit is nil-safe to call more than once, and a no-op on an empty delta.
//
// An error reading the log is returned but should not fail the tick — the caller
// reports inventory regardless, because losing task visibility must not also lose
// the convergence report.
func (c *TaskCollector) Collect(ctx context.Context) (tasks []api.TaskStatus, commit func(), err error) {
	if !c.feedSupported {
		return nil, func() {}, nil
	}
	if !c.started {
		c.watermark = c.now().UTC().Add(-backfillWindow)
		c.started = true
	}

	var (
		out      []api.TaskStatus
		newMark  = c.watermark
		pending  = map[string]taskFingerprint{}
		offset   int
		pages    int
		total    int
		returned int
	)
	commitAt := func(mark time.Time, fps map[string]taskFingerprint) func() {
		return func() {
			for id, fp := range fps {
				c.reported[id] = fp
			}
			c.watermark = mark
		}
	}

	for {
		page, err := c.page(ctx, offset)
		if err != nil {
			// The commit covers only what was read: the watermark stops at the last row
			// returned, so the rows behind the error are re-read next tick.
			return out, commitAt(newMark, pending), err
		}
		if !c.probed {
			c.probed = true
			// A controller predating updated_since ignores it and says so by omitting
			// ordered_by. Advancing a watermark over a created_at-ordered page would
			// silently skip every task the reader has not already seen, so refuse the
			// feed outright rather than trust it. Both images ship from this repo and
			// deploy together, so this guards a mixed rollout; it is not a supported mode.
			if page.OrderedBy == nil || *page.OrderedBy != ampclient.UpdatedAtAsc {
				c.feedSupported = false
				c.logf("amp task feed unavailable: the controller ignored updated_since " +
					"(ordered_by absent or not updated_at_asc). Task reporting is DISABLED for " +
					"the life of this agent; inventory reporting is unaffected")
				return nil, func() {}, nil
			}
		}
		total = page.Total
		returned += len(page.Tasks)

		for _, t := range page.Tasks {
			if t.UpdatedAt.After(newMark) {
				newMark = t.UpdatedAt
			}
			ts := taskToStatus(t)
			// The feed's bound is inclusive, so the boundary rows come back on every
			// sweep. The fingerprint is what makes that a no-op rather than a re-report.
			fp := taskFingerprint{status: ts.Status, updatedAt: t.UpdatedAt}
			if prev, seen := c.reported[ts.ID]; seen && prev == fp {
				continue
			}
			pending[ts.ID] = fp
			out = append(out, ts)
		}

		pages++
		if len(page.Tasks) < feedPageLimit {
			break
		}
		if pages >= maxFeedPages {
			c.logf("amp task feed: stopped after %d pages (%d of %d matching rows); "+
				"the remainder follows on the next tick", pages, returned, total)
			break
		}
		offset += len(page.Tasks)
	}

	return out, commitAt(newMark, pending), nil
}

func (c *TaskCollector) page(ctx context.Context, offset int) (*ampclient.TaskList, error) {
	limit := feedPageLimit
	since := c.watermark.UTC()
	resp, err := c.api.ListTasks(ctx, &ampclient.ListTasksParams{
		Limit:        &limit,
		Offset:       &offset,
		UpdatedSince: &since,
	})
	if err != nil {
		return nil, fmt.Errorf("list amp tasks: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read amp task list: %w", err)
	}
	if resp.StatusCode >= 300 {
		return nil, fmt.Errorf("list amp tasks: %w", httpErr(resp.StatusCode, body))
	}
	var list ampclient.TaskList
	if err := json.Unmarshal(body, &list); err != nil {
		return nil, fmt.Errorf("decode amp task list: %w", err)
	}
	return &list, nil
}

// taskToStatus maps a controller task onto the wire type. Params is dropped: it is
// the whole originating request and would ride every heartbeat.
func taskToStatus(t ampclient.Task) api.TaskStatus {
	ts := api.TaskStatus{
		ID:            t.Id.String(),
		Kind:          t.Kind,
		Status:        string(t.Status),
		YCPGeneration: t.YcpGeneration,
		CreatedAt:     t.CreatedAt,
		UpdatedAt:     t.UpdatedAt,
	}
	if t.ClusterId != nil {
		ts.ClusterID = t.ClusterId.String()
	}
	if t.TargetRef != nil {
		ts.TargetRef = *t.TargetRef
	}
	if t.Error != nil {
		ts.Error = *t.Error
	}
	if t.Result != nil {
		if raw, err := json.Marshal(t.Result); err == nil {
			ts.Result = raw
		}
	}
	return ts
}
