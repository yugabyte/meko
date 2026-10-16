package features

import "testing"

// The test's verdicts on every switch rest on reading the supervisor's
// remote-auth outcome; a misread body must fail, not pass as some state.
func TestParseRemoteAuth(t *testing.T) {
	for _, tc := range []struct {
		out, want string
		err       bool
	}{
		{`{"state":"already_set"}`, "already_set", false},
		{`{"state":"applied","lsn":"0/16B3748"}` + "\n", "applied", false},
		{"no-api\n", "no-api", false},
		{`{"lsn":null}`, "", true},
		{"HTTP/1.0 404 Not Found", "", true},
		{"", "", true},
	} {
		got, err := parseRemoteAuth(tc.out)
		if (err != nil) != tc.err || got != tc.want {
			t.Errorf("parseRemoteAuth(%q) = %q, %v; want %q, error %t", tc.out, got, err, tc.want, tc.err)
		}
	}
}
