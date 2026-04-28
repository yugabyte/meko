package service_test

import (
	"context"
	"strings"
	"testing"

	"meko/api_server/internal/models"
	"meko/api_server/internal/repository"
	"meko/api_server/internal/service"
)

func TestSharingService_InviteToDatapack_RequiresEmail(t *testing.T) {
	svc := service.NewSharingService(nil, nil)
	_, err := svc.InviteToDatapack(context.Background(), "dp-1", "owner-1", service.InviteRequest{
		RecipientEmail: "",
		GrantRole:      "viewer",
	})
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
			_, err := svc.InviteToDatapack(context.Background(), "dp-1", "owner-1", service.InviteRequest{
				RecipientEmail: "test@example.com",
				GrantRole:      c.grant,
			})
			if err == nil || !strings.Contains(err.Error(), "grant is required") {
				t.Errorf("expected grant validation error, got %v", err)
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
	_, err := svc.UpdateMemberGrant("dp-1", "user-1", service.UpdateGrantRequest{
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
