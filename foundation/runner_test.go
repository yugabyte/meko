package foundation

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// TestExecTreatsNonZeroAsAnOutcome pins the contract the callers depend on: a
// command that runs and exits non-zero is data, not an error.
//
// It matters because "absent" and "broken" are both non-zero, and only the
// caller knows which it asked about — a `kubectl get` on a CR that does not
// exist yet is the normal case during a wait, and an Exec that returned an
// error there would turn every first poll into a failure.
func TestExecTreatsNonZeroAsAnOutcome(t *testing.T) {
	res, err := Exec(context.Background(), nil, []string{"sh", "-c", "echo out; echo err >&2; exit 3"})
	if err != nil {
		t.Fatalf("Exec returned an error for a non-zero exit: %v", err)
	}
	if res.ExitCode != 3 {
		t.Errorf("ExitCode = %d, want 3", res.ExitCode)
	}
	if strings.TrimSpace(res.Stdout) != "out" || strings.TrimSpace(res.Stderr) != "err" {
		t.Errorf("streams not captured separately: %+v", res)
	}
}

// A binary that is not on PATH could not run at all, which is a different thing
// from running and failing — that one is an error.
func TestExecErrorsWhenTheBinaryIsMissing(t *testing.T) {
	if _, err := Exec(context.Background(), nil, []string{"definitely-not-a-real-binary-xyz"}); err == nil {
		t.Error("Exec succeeded for a binary that does not exist")
	}
	if _, err := Exec(context.Background(), nil, nil); err == nil {
		t.Error("Exec succeeded for an empty argv")
	}
}

func TestExecPassesStdin(t *testing.T) {
	res, err := Exec(context.Background(), []byte("hello"), []string{"cat"})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if res.Stdout != "hello" {
		t.Errorf("Stdout = %q, want hello", res.Stdout)
	}
}

func TestExecHonoursContextCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := Exec(ctx, nil, []string{"sleep", "30"}); err == nil {
		t.Error("Exec ran a command under a cancelled context")
	}
}

// TestRecorderMatchesAndRecords covers the fake the deployer tests inject:
// substring matching in declaration order, and Once so a test can script a
// poll that changes answer between ticks (pending, then Ready).
func TestRecorderMatchesAndRecords(t *testing.T) {
	rec := &Recorder{Responses: []Response{
		{Match: "get ybuniverse", Result: Result{Stdout: `{"universeState":"Creating"}`}, Once: true},
		{Match: "get ybuniverse", Result: Result{Stdout: `{"universeState":"Ready"}`}},
	}}
	run := rec.Runner()
	ctx := context.Background()

	first, err := run(ctx, nil, []string{"kubectl", "get", "ybuniverse", "foundation"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(first.Stdout, "Creating") {
		t.Errorf("first poll = %q, want the Once response", first.Stdout)
	}
	second, err := run(ctx, nil, []string{"kubectl", "get", "ybuniverse", "foundation"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(second.Stdout, "Ready") {
		t.Errorf("second poll = %q, want the fallthrough response", second.Stdout)
	}

	// An unmatched command succeeds silently, which is the right default for an
	// apply: a test only has to describe the commands it asserts on.
	if res, err := run(ctx, []byte("manifest"), []string{"kubectl", "apply", "-f", "-"}); err != nil || res.ExitCode != 0 {
		t.Errorf("unmatched command = %+v, %v; want a silent success", res, err)
	}

	if got := len(rec.Calls); got != 3 {
		t.Fatalf("recorded %d calls, want 3", got)
	}
	if rec.Calls[2].Stdin != "manifest" {
		t.Errorf("stdin not recorded: %q", rec.Calls[2].Stdin)
	}
	if got := rec.Argvs()[2]; got != "kubectl apply -f -" {
		t.Errorf("Argvs()[2] = %q", got)
	}
}

func TestRecorderReturnsCannedErrors(t *testing.T) {
	want := errors.New("cluster unreachable")
	rec := &Recorder{Responses: []Response{{Match: "helm", Err: want}}}
	if _, err := rec.Run(context.Background(), nil, []string{"helm", "upgrade"}); !errors.Is(err, want) {
		t.Errorf("err = %v, want %v", err, want)
	}
}
