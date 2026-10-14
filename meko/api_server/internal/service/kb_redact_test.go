package service

import (
	"strings"
	"testing"

	"meko/api_server/internal/models"
	"meko/api_server/internal/repository"
)

func TestRedactDocUploaders(t *testing.T) {
	own, other := "viewer-1", "owner-1"
	email := func(s string) *string { return &s }
	docs := func() []repository.KnowledgeBaseDocument {
		return []repository.KnowledgeBaseDocument{
			{ID: "mine", UploadedByUserID: &own, UploadedByEmail: email("v@x.com")},
			{ID: "theirs", UploadedByUserID: &other, UploadedByEmail: email("o@x.com")},
		}
	}
	caller := &models.Account{ID: own}

	got := docs()
	redactDocUploaders(got, string(models.GrantViewer), caller)
	if got[0].UploadedByUserID == nil || got[0].UploadedByEmail == nil {
		t.Errorf("a viewer's own upload lost its uploader: %+v", got[0])
	}
	if got[1].UploadedByUserID != nil || got[1].UploadedByEmail != nil {
		t.Errorf("a viewer is told who uploaded someone else's file: %+v", got[1])
	}

	got = docs()
	redactDocUploaders(got, string(models.GrantContributor), caller)
	if got[1].UploadedByEmail == nil {
		t.Errorf("a contributor lost the uploader of someone else's file")
	}
}

func TestRedactOthersUploaderEmail(t *testing.T) {
	email := func(s string) *string { return &s }
	items := []repository.ActivityLogItem{
		{UploadedByEmail: email("Viewer@X.com")},
		{UploadedByEmail: email("owner@x.com")},
	}
	for i := range items {
		redactOthersUploaderEmail(&items[i], "viewer@x.com")
	}
	if items[0].UploadedByEmail == nil {
		t.Error("a viewer lost the uploader of their own file (emails differ only in case)")
	}
	if items[1].UploadedByEmail != nil {
		t.Error("a viewer is told who uploaded someone else's file")
	}

	anon := []repository.ActivityLogItem{{UploadedByEmail: email("owner@x.com")}}
	redactOthersUploaderEmail(&anon[0], "")
	if anon[0].UploadedByEmail != nil {
		t.Error("with no caller email the uploader must be hidden")
	}
}

func TestHideConversationOwners(t *testing.T) {
	conv := Conversation{ID: "c1", UserID: "u1", UserEmail: "u1@x.com"}
	hideConversationOwner(&conv)
	if conv.UserID != "" || conv.UserEmail != "" || conv.ID != "c1" {
		t.Errorf("got %+v, want the owner gone and the conversation kept", conv)
	}

	steps := []repository.ProcessingStep{{
		ToolArgs: map[string]interface{}{"user_id": "u2", "limit": 5},
		ConversationChunks: []repository.ConversationChunk{
			{Content: "an excerpt", ConversationID: "c2", UserID: "u2"},
		},
	}}
	hideStepOwners(steps)
	if got := steps[0].ConversationChunks[0]; got.UserID != "" || got.Content != "an excerpt" {
		t.Errorf("got %+v, want the excerpt kept without its owner", got)
	}
	if _, ok := steps[0].ToolArgs["user_id"]; ok || steps[0].ToolArgs["limit"] != 5 {
		t.Errorf("tool args %v, want user_id dropped and the rest kept", steps[0].ToolArgs)
	}
}

func TestScrubOwnerKeys(t *testing.T) {
	in := `{"results":[{"id":"m1","memory":"a fact","user_id":"u1","metadata":{"email":"a@x.com"}}]}`
	got := scrubOwnerKeys(in)
	if strings.Contains(got, "u1") || strings.Contains(got, "a@x.com") || !strings.Contains(got, "a fact") {
		t.Errorf("got %s, want the memory kept and its owner gone", got)
	}
	if plain := "not json, user_id=u1"; scrubOwnerKeys(plain) != plain {
		t.Errorf("a non-JSON blob must come back unchanged")
	}
}
