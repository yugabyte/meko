package features

import (
	"errors"
	"fmt"
	"testing"

	"github.com/yugabyte/amp/test/functional/framework/apps/controller"
)

// An extension the deployed controller's catalog does not know is the one
// condition a DEPTH test may skip on (the image predates it); any other
// rejection must fail it.
func TestUnknownExtensionName(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want string
	}{
		{"unknown name", &controller.APIError{StatusCode: 400, Message: `unknown extension "age"`}, "age"},
		{"wrapped", fmt.Errorf("create endpoint: %w", &controller.APIError{StatusCode: 400, Message: `unknown extension "hexabridge"`}), "hexabridge"},
		{"catalog absent entirely", &controller.APIError{StatusCode: 400, Message: "externalized extensions are not supported on this controller"}, ""},
		{"a 500 is not skew", &controller.APIError{StatusCode: 500, Message: `unknown extension "age"`}, ""},
		{"not an api error", errors.New(`unknown extension "age"`), ""},
	} {
		if got := unknownExtensionName(tc.err); got != tc.want {
			t.Errorf("%s: unknownExtensionName = %q, want %q", tc.name, got, tc.want)
		}
	}
}

// shared_preload_libraries matching must be by whole token: substring matching
// would let "pg_cron_extra" satisfy "pg_cron".
func TestContainsToken(t *testing.T) {
	for _, tc := range []struct {
		csv, want string
		ok        bool
	}{
		{"yb_amp,pg_stat_statements,pg_cron", "pg_cron", true},
		{`yb_amp, "pg_cron"`, "pg_cron", true},
		{"yb_amp,pg_cron_extra", "pg_cron", false},
		{"", "pg_cron", false},
		{"hexabridge", "hexabridge", true},
	} {
		if got := containsToken(tc.csv, tc.want); got != tc.ok {
			t.Errorf("containsToken(%q, %q) = %v, want %v", tc.csv, tc.want, got, tc.ok)
		}
	}
}
