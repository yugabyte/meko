package service

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"

	"meko/api_server/internal/auth"
	"meko/api_server/internal/repository"
)

// inviteCognito is a CognitoInviter for the confirmed-without-account paths.
type inviteCognito struct {
	status       auth.UserStatus
	statusErr    error
	createCalled bool
	resetCalled  bool
}

func (c *inviteCognito) CreateUser(context.Context, string, string) (string, error) {
	c.createCalled = true
	return "sub", nil
}

func (c *inviteCognito) ResetTempPassword(context.Context, string, string) error {
	c.resetCalled = true
	return nil
}

func (c *inviteCognito) GetUserStatus(context.Context, string) (auth.UserStatus, error) {
	return c.status, c.statusErr
}

type captureInviteEmailer struct {
	params InviteEmailParams
}

func (e *captureInviteEmailer) SendInviteEmail(_ context.Context, p InviteEmailParams) error {
	e.params = p
	return nil
}

func TestConfirmedWithoutAccount(t *testing.T) {
	cases := []struct {
		name string
		cog  *inviteCognito
		want bool
	}{
		{"confirmed", &inviteCognito{status: auth.UserStatusConfirmed}, true},
		{"force change", &inviteCognito{status: auth.UserStatusForceChangePassword}, false},
		{"not found", &inviteCognito{statusErr: auth.ErrUserNotFound}, false},
		{"lookup error", &inviteCognito{statusErr: errors.New("timeout")}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := &SharingService{cognito: tc.cog}
			if got := s.confirmedWithoutAccount(context.Background(), "user@example.com"); got != tc.want {
				t.Errorf("confirmedWithoutAccount = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestInviteToDatapack_ConfirmedWithoutAccount_NoCreateUser(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })

	mock.ExpectQuery(`SELECT user_id FROM meko_system.users`).
		WithArgs("confirmed@example.com").
		WillReturnError(sql.ErrNoRows)
	mock.ExpectQuery(`SELECT status`).
		WithArgs("dp-1", "confirmed@example.com").
		WillReturnError(sql.ErrNoRows)
	now := time.Now()
	mock.ExpectQuery(`INSERT INTO meko_system.datapack_invites`).
		WillReturnRows(sqlmock.NewRows([]string{
			"invite_id", "datapack_id", "owner_user_id", "recipient_email",
			"recipient_user_id", "grant_role", "status", "created_at", "updated_at",
		}).AddRow("inv-1", "dp-1", "owner-1", "confirmed@example.com", nil, "viewer", "pending", now, now))
	mock.ExpectQuery(`SELECT COUNT`).
		WillReturnRows(sqlmock.NewRows([]string{"n"}).AddRow(1))

	cog := &inviteCognito{status: auth.UserStatusConfirmed}
	mailer := &captureInviteEmailer{}
	svc := NewSharingService(repository.NewSharingRepo(db), cog)
	svc.SetEmailer(mailer)

	inv, err := svc.InviteToDatapack(
		context.Background(),
		"dp-1", "owner-1", "pack", "desc", "Owner", "owner@example.com",
		InviteRequest{RecipientEmail: "confirmed@example.com", GrantRole: "viewer", Source: "api"},
	)
	if err != nil {
		t.Fatalf("InviteToDatapack: %v", err)
	}
	if inv.IsNewUser {
		t.Error("IsNewUser must be false when CreateUser did not run")
	}
	if cog.createCalled {
		t.Error("CreateUser must not run for a CONFIRMED Cognito user")
	}
	if !mailer.params.IsExistingUser || mailer.params.TempPassword != "" {
		t.Errorf("expected existing-user email with no temp password, got %+v", mailer.params)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Error(err)
	}
}

func TestResendInvite_ConfirmedWithoutAccount_NoResetTempPassword(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })

	now := time.Now()
	mock.ExpectQuery(`SELECT invite_id, datapack_id, owner_user_id, recipient_email`).
		WithArgs("inv-1").
		WillReturnRows(sqlmock.NewRows([]string{
			"invite_id", "datapack_id", "owner_user_id", "recipient_email",
			"recipient_user_id", "grant_role", "status", "created_at", "updated_at",
		}).AddRow("inv-1", "dp-1", "owner-1", "confirmed@example.com", nil, "viewer", "pending", now, now))

	cog := &inviteCognito{status: auth.UserStatusConfirmed}
	mailer := &captureInviteEmailer{}
	svc := NewSharingService(repository.NewSharingRepo(db), cog)
	svc.SetEmailer(mailer)

	if _, err := svc.ResendInvite(
		context.Background(),
		"dp-1", "owner-1", "pack", "desc", "Owner", "owner@example.com", "inv-1",
	); err != nil {
		t.Fatalf("ResendInvite: %v", err)
	}
	if cog.resetCalled {
		t.Error("ResetTempPassword must not run for a CONFIRMED Cognito user")
	}
	if !mailer.params.IsExistingUser || mailer.params.TempPassword != "" {
		t.Errorf("expected existing-user email with no temp password, got %+v", mailer.params)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Error(err)
	}
}
