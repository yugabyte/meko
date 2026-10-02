package service_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"meko/api_server/internal/models"
	"meko/api_server/internal/repository"
	"meko/api_server/internal/service"
)

func TestSharingService_InviteToDatapack_RequiresEmail(t *testing.T) {
	svc := service.NewSharingService(nil, nil)
	_, err := svc.InviteToDatapack(
		context.Background(),
		"dp-1", "owner-1", "test-datapack", "A test datapack", "Test Owner", "owner@example.com",
		service.InviteRequest{
			RecipientEmail: "",
			GrantRole:      "viewer",
		},
	)
	if err == nil || !strings.Contains(err.Error(), "recipient_email is required") {
		t.Errorf("expected 'recipient_email is required' error, got %v", err)
	}
}

func TestSharingService_InviteToDatapack_RequiresValidGrant(t *testing.T) {
	svc := service.NewSharingService(nil, nil)
	cases := []struct {
		name  string
		grant string
	}{
		{"empty", ""},
		{"invalid", "superadmin"},
		{"old name", "can_view"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := svc.InviteToDatapack(
				context.Background(),
				"dp-1", "owner-1", "test-datapack", "A test datapack", "Test Owner", "owner@example.com",
				service.InviteRequest{
					RecipientEmail: "test@example.com",
					GrantRole:      c.grant,
				},
			)
			if err == nil || !strings.Contains(err.Error(), "grant is required") {
				t.Errorf("expected grant validation error, got %v", err)
			}
		})
	}
}

func TestSharingService_InviteToDatapack_BlocksOwnerInvite(t *testing.T) {
	svc := service.NewSharingService(nil, nil)
	cases := []struct {
		name      string
		owner     string
		recipient string
	}{
		{"exact", "owner@example.com", "owner@example.com"},
		{"different case", "Owner@Example.com", "owner@example.com"},
		{"recipient whitespace", "owner@example.com", "  owner@example.com  "},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := svc.InviteToDatapack(
				context.Background(),
				"dp-1", "owner-1", "test-datapack", "A test datapack", "Test Owner", c.owner,
				service.InviteRequest{
					RecipientEmail: c.recipient,
					GrantRole:      "viewer",
				},
			)
			if !errors.Is(err, service.ErrCannotInviteOwner) {
				t.Errorf("expected ErrCannotInviteOwner, got %v", err)
			}
		})
	}
}

func TestSharingService_InviteToDatapack_AcceptsValidGrants(t *testing.T) {
	for _, grant := range []string{"viewer", "contributor", "maintainer"} {
		t.Run(grant, func(t *testing.T) {
			if !models.ValidGrantRole(grant) {
				t.Errorf("grant %q should be valid", grant)
			}
		})
	}
}

// Bulk invite: only the pre-repo branches are exercised here. Rows that reach
// the repo can't run against a nil repo, so per-row repo classifications
// (already_member / invite_exists) are covered by the handler integration test.

func TestSharingService_BulkInvite_RequiresValidGrant(t *testing.T) {
	svc := service.NewSharingService(nil, nil)
	for _, grant := range []string{"", "superadmin", "can_view"} {
		t.Run("grant="+grant, func(t *testing.T) {
			_, err := svc.BulkInviteToDatapack(
				context.Background(),
				"dp-1", "owner-1", "test-datapack", "A test datapack", "Test Owner", "owner@example.com",
				service.BulkInviteRequest{Emails: []string{"a@example.com"}, GrantRole: grant},
			)
			if err == nil || !strings.Contains(err.Error(), "grant is required") {
				t.Errorf("expected grant validation error, got %v", err)
			}
		})
	}
}

func TestSharingService_BulkInvite_RequiresEmails(t *testing.T) {
	svc := service.NewSharingService(nil, nil)
	for name, emails := range map[string][]string{"nil": nil, "empty": {}} {
		t.Run(name, func(t *testing.T) {
			_, err := svc.BulkInviteToDatapack(
				context.Background(),
				"dp-1", "owner-1", "test-datapack", "A test datapack", "Test Owner", "owner@example.com",
				service.BulkInviteRequest{Emails: emails, GrantRole: "viewer"},
			)
			if err == nil || !strings.Contains(err.Error(), "emails is required") {
				t.Errorf("expected emails validation error, got %v", err)
			}
		})
	}
}

// Malformed addresses and the owner's own email are classified per-row and
// never reach the repo, so a nil-repo service can process a whole batch of them.
func TestSharingService_BulkInvite_ClassifiesPreRepoRows(t *testing.T) {
	svc := service.NewSharingService(nil, nil)
	resp, err := svc.BulkInviteToDatapack(
		context.Background(),
		"dp-1", "owner-1", "test-datapack", "A test datapack", "Test Owner", "dev@localhost",
		service.BulkInviteRequest{
			Emails: []string{
				"not-an-email",
				"missing@tld",
				"",
				"DEV@localhost", // owner, case-insensitive; domain is dotless
			},
			GrantRole: "viewer",
		},
	)
	if err != nil {
		t.Fatalf("expected partial success (nil error), got %v", err)
	}
	want := []string{
		service.BulkStatusInvalidEmail,
		service.BulkStatusInvalidEmail,
		service.BulkStatusInvalidEmail,
		service.BulkStatusIsOwner,
	}
	if len(resp.Results) != len(want) {
		t.Fatalf("expected %d results, got %d (%+v)", len(want), len(resp.Results), resp.Results)
	}
	for i, w := range want {
		if resp.Results[i].Status != w {
			t.Errorf("result[%d] (%q) status = %q, want %q",
				i, resp.Results[i].Email, resp.Results[i].Status, w)
		}
	}
	if got := resp.Summary[service.BulkStatusInvalidEmail]; got != 3 {
		t.Errorf("summary[invalid_email] = %d, want 3", got)
	}
	if got := resp.Summary[service.BulkStatusIsOwner]; got != 1 {
		t.Errorf("summary[is_owner] = %d, want 1", got)
	}
}

// Repeated addresses collapse to one result regardless of capitalisation, so a
// pasted CSV with duplicates doesn't invite the same person twice.
func TestSharingService_BulkInvite_DeduplicatesCaseInsensitively(t *testing.T) {
	svc := service.NewSharingService(nil, nil)
	resp, err := svc.BulkInviteToDatapack(
		context.Background(),
		"dp-1", "owner-1", "test-datapack", "A test datapack", "Test Owner", "owner@example.com",
		service.BulkInviteRequest{
			Emails:    []string{"bad email", "BAD EMAIL", "  bad email  "},
			GrantRole: "viewer",
		},
	)
	if err != nil {
		t.Fatalf("expected partial success (nil error), got %v", err)
	}
	if len(resp.Results) != 1 {
		t.Fatalf("expected 1 de-duplicated result, got %d (%+v)", len(resp.Results), resp.Results)
	}
	if resp.Summary[service.BulkStatusInvalidEmail] != 1 {
		t.Errorf("summary[invalid_email] = %d, want 1", resp.Summary[service.BulkStatusInvalidEmail])
	}
}

func TestSharingService_HandleInviteAction_RequiresInviteID(t *testing.T) {
	svc := service.NewSharingService(nil, nil)
	_, err := svc.HandleInviteAction(service.InviteActionRequest{
		InviteID:        "",
		RecipientUserID: "user-1",
		UserAction:      "accept",
	})
	if err == nil || !strings.Contains(err.Error(), "invite_id is required") {
		t.Errorf("expected 'invite_id is required' error, got %v", err)
	}
}

func TestSharingService_HandleInviteAction_RequiresRecipientUserID(t *testing.T) {
	svc := service.NewSharingService(nil, nil)
	_, err := svc.HandleInviteAction(service.InviteActionRequest{
		InviteID:        "inv-1",
		RecipientUserID: "",
		UserAction:      "accept",
	})
	if err == nil || !strings.Contains(err.Error(), "recipient_user_id is required") {
		t.Errorf("expected 'recipient_user_id is required' error, got %v", err)
	}
}

func TestSharingService_HandleInviteAction_RequiresValidAction(t *testing.T) {
	svc := service.NewSharingService(nil, nil)
	cases := []struct {
		name   string
		action string
	}{
		{"empty", ""},
		{"invalid", "maybe"},
		{"typo", "acept"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := svc.HandleInviteAction(service.InviteActionRequest{
				InviteID:        "inv-1",
				RecipientUserID: "user-1",
				UserAction:      c.action,
			})
			if err == nil || !strings.Contains(err.Error(), "user_action is required") {
				t.Errorf("expected user_action validation error, got %v", err)
			}
		})
	}
}

func TestSharingService_UpdateMemberGrant_RequiresValidGrant(t *testing.T) {
	svc := service.NewSharingService(nil, nil)
	_, err := svc.UpdateMemberGrant("dp-1", "owner-1", "user-1", service.UpdateGrantRequest{
		GrantRole: "invalid",
	})
	if err == nil || !strings.Contains(err.Error(), "grant is required") {
		t.Errorf("expected grant validation error, got %v", err)
	}
}

func TestErrInviteExists_IsSentinel(t *testing.T) {
	err := repository.ErrInviteExists
	if err == nil {
		t.Fatal("ErrInviteExists is nil")
	}
	if !strings.Contains(err.Error(), "already exists") {
		t.Errorf("ErrInviteExists message = %q", err.Error())
	}
}

func TestErrInviteNotFound_IsSentinel(t *testing.T) {
	err := repository.ErrInviteNotFound
	if err == nil {
		t.Fatal("ErrInviteNotFound is nil")
	}
	if !strings.Contains(err.Error(), "not found") {
		t.Errorf("ErrInviteNotFound message = %q", err.Error())
	}
}
