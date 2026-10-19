package service

import (
	"strings"
	"testing"

	"meko/api_server/internal/models"
)

const (
	callerID = "2a6624ef-26c1-410c-bff3-dc111f2e45a2"
	otherID  = "cdfc5c73-30b4-43bf-9d25-49c66f50b3d1"
)

func rows() []models.Memory {
	return []models.Memory{
		{ID: "m1", UserID: callerID, UserName: "Caller", UserEmail: "caller@example.com"},
		{ID: "m2", UserID: otherID, UserName: "Other", UserEmail: "other@example.com"},
	}
}

func TestApplyOwnerVisibilityFlagsOnlyTheCallers(t *testing.T) {
	got := rows()
	applyOwnerVisibility(got, callerID, false)

	if !got[0].IsMine {
		t.Error("the caller's own memory is not marked IsMine")
	}
	if got[1].IsMine {
		t.Error("another member's memory is marked IsMine")
	}
	if got[0].UserName != "Caller" {
		t.Error("attribution was dropped without hideOwners")
	}
}

// The ordering guard. Marking after the blanking would leave every viewer
// row IsMine false, which is the bug this flag exists to prevent, so this
// has to go through applyOwnerVisibility rather than re-stating the two
// steps itself.
func TestApplyOwnerVisibilityMarksBeforeHidingTheOwners(t *testing.T) {
	got := rows()
	applyOwnerVisibility(got, callerID, true)

	if !got[0].IsMine {
		t.Error("IsMine is false on the caller's own row with the owners hidden; " +
			"a demoted viewer would see no delete affordance on their own memory")
	}
	if got[1].IsMine {
		t.Error("another member's memory is marked IsMine")
	}
	if got[0].UserID != "" || got[0].UserName != "" || got[0].UserEmail != "" {
		t.Error("attribution leaked: IsMine must not keep the identity fields alive")
	}
	if got[1].UserID != "" || got[1].UserName != "" || got[1].UserEmail != "" {
		t.Error("another member's attribution was not hidden")
	}
}

// The ids are compared as uuids because DeleteByIDForUser does: it matches
// with meko_user_id = $3::uuid, which accepts any spelling. A string compare
// would hide the delete button on rows the delete would accept.
func TestApplyOwnerVisibilityComparesIDsAsUUIDsNotStrings(t *testing.T) {
	for _, spelling := range []struct{ name, caller string }{
		{"uppercase", strings.ToUpper(callerID)},
		{"mixed case", "2A6624ef-26C1-410c-BFF3-dc111f2e45a2"},
		{"surrounding space", " " + callerID + " "},
	} {
		t.Run(spelling.name, func(t *testing.T) {
			got := rows()
			applyOwnerVisibility(got, spelling.caller, true)

			if !got[0].IsMine {
				t.Errorf("caller %q did not match its own row stored as %q",
					spelling.caller, callerID)
			}
			if got[1].IsMine {
				t.Error("another member's memory is marked IsMine")
			}
		})
	}
}

// A caller id that is not a uuid at all marks nothing, rather than falling
// back to a string compare that could match a malformed row.
func TestApplyOwnerVisibilityWithAMalformedCallerMarksNothing(t *testing.T) {
	got := append(rows(), models.Memory{ID: "m3", UserID: "not-a-uuid"})
	applyOwnerVisibility(got, "not-a-uuid", false)

	for _, m := range got {
		if m.IsMine {
			t.Errorf("memory %s marked IsMine for a malformed caller id", m.ID)
		}
	}
}

// An unauthenticated or unresolved caller marks nothing, rather than
// matching every row whose UserID happens to be empty.
func TestApplyOwnerVisibilityWithNoCallerMarksNothing(t *testing.T) {
	got := append(rows(), models.Memory{ID: "m3"})
	applyOwnerVisibility(got, "", true)

	for _, m := range got {
		if m.IsMine {
			t.Errorf("memory %s marked IsMine with no caller id", m.ID)
		}
	}
}
