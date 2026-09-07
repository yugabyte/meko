package features

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/yugabyte/amp/test/functional/framework/apps/controller"
)

func TestCheckRollReport(t *testing.T) {
	ids := []string{"a", "b"}
	good := controller.RollReport{Version: "v1", Upgraded: 2, Endpoints: []controller.RollLine{{EndpointID: "a", Outcome: "upgraded"}, {EndpointID: "b", Outcome: "upgraded"}}}
	if err := checkRollReport(good, "v1", ids, controller.RollUpgraded); err != nil {
		t.Fatalf("good report rejected: %v", err)
	}
	cases := []struct {
		name    string
		report  controller.RollReport
		wantErr string
	}{
		{"wrong version", controller.RollReport{Version: "v2", Upgraded: 2, Endpoints: good.Endpoints}, "names version"},
		{"one endpoint skipped", controller.RollReport{Version: "v1", Upgraded: 1, Skipped: 1, Endpoints: []controller.RollLine{{EndpointID: "a", Outcome: "upgraded"}, {EndpointID: "b", Outcome: "skipped", Reason: "already at target"}}}, `ended "skipped"`},
		{"a line for a stranger", controller.RollReport{Version: "v1", Upgraded: 2, Endpoints: []controller.RollLine{{EndpointID: "a", Outcome: "upgraded"}, {EndpointID: "zz", Outcome: "upgraded"}}}, "never named"},
		{"tally disagrees with lines", controller.RollReport{Version: "v1", Upgraded: 1, Endpoints: good.Endpoints}, "tallies upgraded=1"},
		{"missing line", controller.RollReport{Version: "v1", Upgraded: 1, Endpoints: good.Endpoints[:1]}, "1 endpoint lines, want 2"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := checkRollReport(tc.report, "v1", ids, controller.RollUpgraded)
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("err = %v, want it to contain %q", err, tc.wantErr)
			}
		})
	}
}

func TestCheckRollSubtasks(t *testing.T) {
	sub := func(id, status string) controller.Subtask {
		return controller.Subtask{Kind: "upgrade_endpoint", Status: status, Args: json.RawMessage(`{"endpoint_id":"` + id + `","version":"v1"}`)}
	}
	if err := checkRollSubtasks([]controller.Subtask{sub("a", "succeeded"), sub("b", "succeeded")}, []string{"a", "b"}); err != nil {
		t.Fatalf("good subtasks rejected: %v", err)
	}
	if err := checkRollSubtasks([]controller.Subtask{sub("a", "succeeded")}, []string{"a", "b"}); err == nil || !strings.Contains(err.Error(), "1 subtask(s) for 2") {
		t.Errorf("missing child not caught: %v", err)
	}
	msg := "helm wait timed out"
	failed := sub("b", "failed")
	failed.Error = &msg
	if err := checkRollSubtasks([]controller.Subtask{sub("a", "succeeded"), failed}, []string{"a", "b"}); err == nil || !strings.Contains(err.Error(), "helm wait timed out") {
		t.Errorf("failed child not surfaced with its error: %v", err)
	}
	if err := checkRollSubtasks([]controller.Subtask{sub("a", "succeeded"), sub("a", "succeeded")}, []string{"a", "b"}); err == nil || !strings.Contains(err.Error(), "no subtask rolled endpoint b") {
		t.Errorf("duplicate child for one endpoint not caught: %v", err)
	}
}

func TestRollReportDecodesTaskResult(t *testing.T) {
	task := controller.Task{Result: json.RawMessage(`{"version":"0.1","endpoints":[{"endpoint_id":"a","outcome":"upgraded","subtask_id":"s1"}],"upgraded":1,"skipped":0,"failed":0,"unattempted":0}`)}
	r, ok := task.RollReport()
	if !ok || r.Upgraded != 1 || len(r.Endpoints) != 1 || r.Endpoints[0].SubtaskID != "s1" {
		t.Fatalf("decoded %+v ok=%v", r, ok)
	}
	if _, ok := (controller.Task{Result: json.RawMessage(`{"in_place_resize":"completed"}`)}).RollReport(); ok {
		t.Error("a resize result decoded as a roll report")
	}
	if _, ok := (controller.Task{}).RollReport(); ok {
		t.Error("an empty result decoded as a roll report")
	}
}

func TestImageTag(t *testing.T) {
	for in, want := range map[string]string{
		"amp-endpoint:0.260904.1-ba9ba7a76":                        "0.260904.1-ba9ba7a76",
		"quay.io/yugabyte/amp-endpoint:custom-abc":                 "custom-abc",
		"localhost:5000/amp-endpoint":                              "localhost:5000/amp-endpoint",
		"0.260904.1-ba9ba7a76":                                     "0.260904.1-ba9ba7a76",
		" quay.io:443/yugabyte/amp-endpoint:0.260904.1-ba9ba7a76 ": "0.260904.1-ba9ba7a76",
	} {
		if got := imageTag(in); got != want {
			t.Errorf("imageTag(%q) = %q, want %q", in, got, want)
		}
	}
}
