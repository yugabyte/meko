package features

import (
	"net/http"
	"strings"
	"testing"

	"github.com/yugabyte/amp/test/functional/framework/apps/controller"
)

func TestPoolRaceVerdict(t *testing.T) {
	cases := []struct {
		name         string
		deleteStatus int
		deleted      bool
		create       string
		wantErr      string
	}{
		{"delete won and create failed", 0, true, controller.TaskFailed, ""},
		{"create won and delete refused", http.StatusConflict, false, controller.TaskSucceeded, ""},
		{"both committed", 0, true, controller.TaskSucceeded, "live cluster now sits under a tombstoned pool"},
		{"delete 404 on a fresh pool", http.StatusNotFound, false, controller.TaskSucceeded, "returned 404"},
		{"delete 500", http.StatusInternalServerError, false, controller.TaskFailed, "HTTP 500"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := poolRaceVerdict(tc.deleteStatus, tc.deleted, tc.create)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("err = %v, want it to contain %q", err, tc.wantErr)
			}
		})
	}
}
