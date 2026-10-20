package task

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/yugabyte/amp/controller/internal/core"
)

// endpointCreateTasksContract is EndpointCreateTasks' behaviour, asserted against
// both stores (TestMemEndpointCreateTasks, TestPgEndpointCreateTasks). It is what
// the endpoint API reports as create_task, and what the YCP agent turns into an
// endpoint's `error` state, so a store that matched the wrong step — a resize, a
// delete — would report a healthy endpoint as failed, or a failed one as fine.
func endpointCreateTasksContract(t *testing.T, s Store) {
	ctx := t.Context()
	const owner = "contract-owner"

	create := func(kind Kind, steps ...StepSpec) *Task {
		t.Helper()
		tk, err := s.Create(ctx, TaskSpec{Kind: kind, Steps: steps})
		if err != nil {
			t.Fatalf("create %s: %v", kind, err)
		}
		// Distinct created_at for "newest wins": two tasks in one clock tick would
		// leave the order to the id tiebreak.
		time.Sleep(2 * time.Millisecond)
		return tk
	}
	step := func(kind string, endpointID any) StepSpec {
		args, err := json.Marshal(map[string]any{"endpoint_id": endpointID})
		if err != nil {
			t.Fatalf("marshal args: %v", err)
		}
		return StepSpec{Kind: kind, Args: args}
	}
	claim := func(tk *Task) {
		t.Helper()
		if got, _, err := s.Claim(ctx, tk.ID, owner); err != nil || got == nil {
			t.Fatalf("claim %s: task=%v err=%v", tk.ID, got, err)
		}
	}
	fail := func(tk *Task, cause error) {
		t.Helper()
		claim(tk)
		if err := s.FailStep(ctx, tk.ID, tk.StepIDs[len(tk.StepIDs)-1], owner, cause); err != nil {
			t.Fatalf("fail %s: %v", tk.ID, err)
		}
	}
	succeed := func(tk *Task) {
		t.Helper()
		claim(tk)
		for _, sid := range tk.StepIDs {
			if err := s.CompleteStep(ctx, tk.ID, sid, owner, nil); err != nil {
				t.Fatalf("complete step: %v", err)
			}
		}
		if err := s.CompleteTask(ctx, tk.ID, owner); err != nil {
			t.Fatalf("complete %s: %v", tk.ID, err)
		}
	}
	other := func() StepSpec { return StepSpec{Kind: "record_branch_recipe", Args: json.RawMessage(`{}`)} }

	var (
		standalone, clusterWriter, branchWriter = uuid.New(), uuid.New(), uuid.New()
		pending, running, retried               = uuid.New(), uuid.New(), uuid.New()
		resizedOnly, deletedLater, legacy       = uuid.New(), uuid.New(), uuid.New()
		unrequested                             = uuid.New()
	)

	standaloneTask := create(KindCreateEndpoint, step(StepCreateEndpoint, standalone))
	fail(standaloneTask, errors.New("deploy endpoint: 0/6 nodes are available: 3 Insufficient ephemeral-storage"))

	// A writer created by create_cluster: the create_endpoint step is the last of
	// several, and the endpoint id is in no target_ref.
	clusterWriterTask := create(KindCreateCluster, other(), step(StepCreateEndpoint, clusterWriter))
	fail(clusterWriterTask, fmt.Errorf("ensure endpoint record: %w", core.ErrConflict))

	branchWriterTask := create(KindBranch, other(), step(StepCreateEndpoint, branchWriter))
	succeed(branchWriterTask)

	pendingTask := create(KindCreateEndpoint, step(StepCreateEndpoint, pending))
	runningTask := create(KindCreateEndpoint, step(StepCreateEndpoint, running))
	claim(runningTask)

	// Two tasks name one endpoint: the newer decides, whatever the older says.
	fail(create(KindCreateEndpoint, step(StepCreateEndpoint, retried)), errors.New("first attempt"))
	retriedTask := create(KindCreateEndpoint, step(StepCreateEndpoint, retried))

	// Other steps carry endpoint_id too, and must never be read as the create.
	fail(create(KindResizeEndpoint, step("resize_endpoint", resizedOnly)), errors.New("resize failed"))
	deletedLaterCreate := create(KindCreateEndpoint, step(StepCreateEndpoint, deletedLater))
	succeed(deletedLaterCreate)
	fail(create(KindDeleteEndpoint, step("delete_endpoint", deletedLater)), errors.New("delete failed"))

	// Args that name no endpoint, or not as a uuid: skipped, not an error.
	create(KindCreateEndpoint, StepSpec{Kind: StepCreateEndpoint, Args: json.RawMessage(`{}`)})
	create(KindCreateEndpoint, step(StepCreateEndpoint, "not-a-uuid"))

	create(KindCreateEndpoint, step(StepCreateEndpoint, unrequested))

	got, err := s.EndpointCreateTasks(ctx, []uuid.UUID{
		standalone, clusterWriter, branchWriter, pending, running, retried, resizedOnly, deletedLater, legacy,
	})
	if err != nil {
		t.Fatalf("EndpointCreateTasks: %v", err)
	}

	type want struct {
		task   uuid.UUID
		kind   string
		status Status
		err    string
		code   string
	}
	for name, tc := range map[string]struct {
		id   uuid.UUID
		want *want
	}{
		"standalone create that failed": {standalone, &want{standaloneTask.ID, "create_endpoint", StatusFailed,
			"deploy endpoint: 0/6 nodes are available: 3 Insufficient ephemeral-storage", ""}},
		"writer of a create_cluster, failed with a code": {clusterWriter, &want{clusterWriterTask.ID, "create_cluster",
			StatusFailed, "ensure endpoint record: conflict", ErrorCodeConflict}},
		"writer of a branch, succeeded": {branchWriter, &want{branchWriterTask.ID, "branch", StatusSucceeded, "", ""}},
		"create not yet claimed":        {pending, &want{pendingTask.ID, "create_endpoint", StatusPending, "", ""}},
		"create running":                {running, &want{runningTask.ID, "create_endpoint", StatusRunning, "", ""}},
		"newest of two creates":         {retried, &want{retriedTask.ID, "create_endpoint", StatusPending, "", ""}},
		"only a resize names it":        {resizedOnly, nil},
		"a later failed delete":         {deletedLater, &want{deletedLaterCreate.ID, "create_endpoint", StatusSucceeded, "", ""}},
		"no task at all (predates log)": {legacy, nil},
	} {
		t.Run(name, func(t *testing.T) {
			tk, ok := got[tc.id]
			if tc.want == nil {
				if ok {
					t.Fatalf("got task %s (%s), want none", tk.ID, tk.Kind)
				}
				return
			}
			if !ok {
				t.Fatal("no task returned")
			}
			if tk.ID != tc.want.task || tk.Kind != tc.want.kind || tk.Status != tc.want.status {
				t.Errorf("got %s %s %s, want %s %s %s", tk.ID, tk.Kind, tk.Status,
					tc.want.task, tc.want.kind, tc.want.status)
			}
			if gotErr := deref(tk.Error); gotErr != tc.want.err {
				t.Errorf("error = %q, want %q", gotErr, tc.want.err)
			}
			if gotCode := deref(tk.ErrorCode); gotCode != tc.want.code {
				t.Errorf("error_code = %q, want %q", gotCode, tc.want.code)
			}
		})
	}
	if _, ok := got[unrequested]; ok {
		t.Error("returned an endpoint that was not asked for")
	}
	if len(got) != 7 {
		t.Errorf("returned %d endpoints, want 7", len(got))
	}

	for _, ids := range [][]uuid.UUID{nil, {}} {
		if got, err := s.EndpointCreateTasks(ctx, ids); err != nil || len(got) != 0 {
			t.Errorf("EndpointCreateTasks(%v) = %v, %v; want nothing", ids, got, err)
		}
	}
}

// endpointCreateTasksEdgeContract is the rest of the contract: inputs and task
// shapes the main cases do not reach, each of which a store could get wrong in a
// way only the other store would not.
func endpointCreateTasksEdgeContract(t *testing.T, s Store) {
	ctx := t.Context()
	const owner = "edge-owner"
	createSteps := func(kind Kind, steps ...StepSpec) *Task {
		t.Helper()
		tk, err := s.Create(ctx, TaskSpec{Kind: kind, Steps: steps})
		if err != nil {
			t.Fatalf("create %s: %v", kind, err)
		}
		time.Sleep(2 * time.Millisecond)
		return tk
	}
	createStep := func(endpointID any) StepSpec {
		args, _ := json.Marshal(map[string]any{"endpoint_id": endpointID})
		return StepSpec{Kind: StepCreateEndpoint, Args: args}
	}
	claim := func(tk *Task) {
		t.Helper()
		if got, _, err := s.Claim(ctx, tk.ID, owner); err != nil || got == nil {
			t.Fatalf("claim: %v %v", got, err)
		}
	}
	failAt := func(tk *Task, step int, cause error) {
		t.Helper()
		claim(tk)
		for _, sid := range tk.StepIDs[:step] {
			if err := s.CompleteStep(ctx, tk.ID, sid, owner, nil); err != nil {
				t.Fatalf("complete step: %v", err)
			}
		}
		if err := s.FailStep(ctx, tk.ID, tk.StepIDs[step], owner, cause); err != nil {
			t.Fatalf("fail step: %v", err)
		}
	}
	read := func(ids ...uuid.UUID) map[uuid.UUID]*Task {
		t.Helper()
		got, err := s.EndpointCreateTasks(ctx, ids)
		if err != nil {
			t.Fatalf("EndpointCreateTasks: %v", err)
		}
		return got
	}

	t.Run("an id written in upper case is not the endpoint", func(t *testing.T) {
		// Steps are written by marshalling a uuid.UUID, which is lower case. Both
		// stores compare that text exactly, so they agree on any other spelling.
		id := uuid.New()
		createSteps(KindCreateEndpoint, createStep(strings.ToUpper(id.String())))
		if got := read(id); len(got) != 0 {
			t.Errorf("got %v, want nothing", got)
		}
	})

	t.Run("an id asked for twice is answered once", func(t *testing.T) {
		id := uuid.New()
		tk := createSteps(KindCreateEndpoint, createStep(id))
		got := read(id, id, id)
		if len(got) != 1 || got[id] == nil || got[id].ID != tk.ID {
			t.Errorf("got %v, want one entry for task %s", got, tk.ID)
		}
	})

	t.Run("one task creating two endpoints answers for both", func(t *testing.T) {
		a, b := uuid.New(), uuid.New()
		tk := createSteps(KindCreateCluster, createStep(a), createStep(b))
		got := read(a, b)
		if got[a] == nil || got[b] == nil || got[a].ID != tk.ID || got[b].ID != tk.ID {
			t.Errorf("got %v, want both endpoints on task %s", got, tk.ID)
		}
	})

	for name, tc := range map[string]struct {
		cause error
		code  string
	}{
		"a cancelled create":         {fmt.Errorf("stopped: %w", ErrCancelled), ErrorCodeCancelled},
		"a create whose branch went": {fmt.Errorf("ensure endpoint record: %w", core.ErrNotFound), ErrorCodeNotFound},
	} {
		t.Run(name+" carries its code", func(t *testing.T) {
			id := uuid.New()
			tk := createSteps(KindCreateEndpoint, createStep(id))
			failAt(tk, 0, tc.cause)
			got := read(id)[id]
			if got == nil || got.Status != StatusFailed || deref(got.ErrorCode) != tc.code {
				t.Errorf("got %+v, want failed with code %q", got, tc.code)
			}
		})
	}

	t.Run("a later step failing fails the task the create step is in", func(t *testing.T) {
		// The endpoint was deployed — its step succeeded — and something after it
		// failed. The task is what is reported; whether the endpoint is usable is
		// for its address and health to say.
		id := uuid.New()
		tk := createSteps(KindCreateCluster, createStep(id),
			StepSpec{Kind: "record_branch_recipe", Args: json.RawMessage(`{}`)})
		failAt(tk, 1, errors.New("record recipe failed"))
		got := read(id)[id]
		if got == nil || got.ID != tk.ID || got.Status != StatusFailed || deref(got.Error) != "record recipe failed" {
			t.Errorf("got %+v, want task %s failed with the later step's error", got, tk.ID)
		}
	})

	t.Run("a page's worth of endpoints in one call", func(t *testing.T) {
		const n = 250
		ids := make([]uuid.UUID, n)
		tasks := map[uuid.UUID]uuid.UUID{}
		for i := range ids {
			ids[i] = uuid.New()
			tk, err := s.Create(ctx, TaskSpec{Kind: KindCreateEndpoint, Steps: []StepSpec{createStep(ids[i])}})
			if err != nil {
				t.Fatalf("create: %v", err)
			}
			tasks[ids[i]] = tk.ID
		}
		got := read(ids...)
		if len(got) != n {
			t.Fatalf("got %d entries, want %d", len(got), n)
		}
		for id, want := range tasks {
			if got[id] == nil || got[id].ID != want {
				t.Fatalf("endpoint %s answered with %v, want task %s", id, got[id], want)
			}
		}
	})

	t.Run("what is returned is the caller's to change", func(t *testing.T) {
		id := uuid.New()
		createSteps(KindCreateEndpoint, createStep(id))
		read(id)[id].Status = StatusFailed
		if got := read(id)[id]; got.Status != StatusPending {
			t.Errorf("status after mutating a returned copy = %s, want pending", got.Status)
		}
	})
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

func TestMemEndpointCreateTasks(t *testing.T) {
	endpointCreateTasksContract(t, NewMemStore())
	endpointCreateTasksEdgeContract(t, NewMemStore())
}

func TestPgEndpointCreateTasks(t *testing.T) {
	pool, _ := migratedPool(t)
	t.Cleanup(pool.Close)
	resetTasks(t, pool)
	endpointCreateTasksContract(t, NewPgStore(pool))
	endpointCreateTasksEdgeContract(t, NewPgStore(pool))
}
