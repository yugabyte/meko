package core

import (
	"errors"
	"strings"
	"testing"
)

// The conflict text is all an operator sees of a refused endpoint create, so it
// must say what to change, and still be ErrConflict for the task's error_code.
func TestEndpointConflictNamesTheCause(t *testing.T) {
	for _, tc := range []struct {
		role, name, want string
	}{
		{RoleReadOnly, "ro", `an endpoint named "ro"`},
		{RoleReadWrite, "", "already has a read_write endpoint"},
		{RoleReadWrite, "rw2", `already has a read_write endpoint, or an endpoint named "rw2"`},
	} {
		err := endpointConflict(tc.role, tc.name)
		if !errors.Is(err, ErrConflict) {
			t.Errorf("%s/%q: %v is not ErrConflict", tc.role, tc.name, err)
		}
		if !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s/%q: %q, want it to contain %q", tc.role, tc.name, err, tc.want)
		}
	}
}
