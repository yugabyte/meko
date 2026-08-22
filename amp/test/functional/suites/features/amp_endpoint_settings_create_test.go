package features

import (
	"errors"
	"fmt"
	"testing"

	"github.com/yugabyte/amp/test/functional/framework/apps/controller"
)

// The settings API's contract is a synchronous 400 from the daemon key-set
// check. Any other rejection shape (a failed task, a 500) is a finding of its
// own, so the classifier must not accept it.
func TestSettingsRejectionProblem(t *testing.T) {
	for _, tc := range []struct {
		name   string
		err    error
		wantOK bool
	}{
		{"validation 400", &controller.APIError{StatusCode: 400}, true},
		{"wrapped validation 400", fmt.Errorf("create: %w", &controller.APIError{StatusCode: 400}), true},
		{"a 500 instead of validation", &controller.APIError{StatusCode: 500}, false},
		{"a conflict, not validation", &controller.APIError{StatusCode: 409}, false},
		{"a failed task, not a synchronous rejection", errors.New("waiting for task t1: task failed"), false},
	} {
		problem := settingsRejectionProblem(tc.err)
		if (problem == "") != tc.wantOK {
			t.Errorf("%s: settingsRejectionProblem(%v) = %q, want ok=%v", tc.name, tc.err, problem, tc.wantOK)
		}
	}
}
