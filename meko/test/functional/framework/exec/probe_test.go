package exec

import (
	"context"
	"strings"
	"testing"
)

type scriptedExec struct {
	results []Result
	calls   int
}

func (s *scriptedExec) Run(context.Context, string) (Result, error) {
	r := s.results[min(s.calls, len(s.results)-1)]
	s.calls++
	return r, nil
}
func (s *scriptedExec) Copy(context.Context, string, string) error { return nil }

func TestHasCommand(t *testing.T) {
	if ok, err := HasCommand(context.Background(), &scriptedExec{results: []Result{{Stdout: "probe-rc=0\n"}}}, "x"); !ok || err != nil {
		t.Errorf("present: %v %v", ok, err)
	}
	if ok, err := HasCommand(context.Background(), &scriptedExec{results: []Result{{Stdout: "probe-rc=1\n"}}}, "x"); ok || err != nil {
		t.Errorf("absent is an answer, not an error: %v %v", ok, err)
	}
	flaky := &scriptedExec{results: []Result{{ExitCode: 1, Stderr: "error: unable to upgrade connection"}, {Stdout: "probe-rc=0\n"}}}
	if ok, err := HasCommand(context.Background(), flaky, "x"); !ok || err != nil || flaky.calls != 2 {
		t.Errorf("a transport failure is retried, not read as absent: %v %v after %d calls", ok, err, flaky.calls)
	}
	dead := &scriptedExec{results: []Result{{ExitCode: 1, Stderr: "error: pod not found"}}}
	if _, err := HasCommand(context.Background(), dead, "x"); err == nil || !strings.Contains(err.Error(), "pod not found") {
		t.Errorf("a target that never answers is an error naming why: %v", err)
	}
}
