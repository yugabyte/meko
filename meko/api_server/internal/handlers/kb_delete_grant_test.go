package handlers

import (
	"testing"

	"meko/api_server/internal/models"
)

// Who may delete a KB file: owners and maintainers any file, a contributor or a
// viewer (a demoted member) only one it uploaded.
func TestCanDeleteKBDocument(t *testing.T) {
	me := "user-1"
	other := "user-2"
	for _, tc := range []struct {
		uploader *string
		grant    string
		want     bool
	}{
		{&other, models.GrantOwner, true},
		{&other, string(models.GrantMaintainer), true},
		{&me, string(models.GrantContributor), true},
		{&other, string(models.GrantContributor), false},
		{&me, string(models.GrantViewer), true},
		{&other, string(models.GrantViewer), false},
		{nil, string(models.GrantContributor), false},
	} {
		if got := canDeleteKBDocument(tc.grant, me, tc.uploader); got != tc.want {
			t.Errorf("canDeleteKBDocument(%s, uploader=%v) = %v, want %v", tc.grant, tc.uploader, got, tc.want)
		}
	}
}
