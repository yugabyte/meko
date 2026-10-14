package service

import (
	"errors"
	"regexp"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"

	"meko/api_server/internal/repository"
)

func strPtr(s string) *string { return &s }

// A link may grant viewer or contributor only, and is either active or off.
// A rejected request never writes.
func TestShareLinkUpdate_RejectsWhatALinkCannotGrant(t *testing.T) {
	for name, req := range map[string]UpdateShareLinkRequest{
		"maintainer":     {GrantRole: strPtr("maintainer")},
		"owner":          {GrantRole: strPtr("owner")},
		"unknown status": {Status: strPtr("paused")},
	} {
		t.Run(name, func(t *testing.T) {
			db, mock, err := sqlmock.New()
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()

			svc := NewShareLinkService(repository.NewShareLinkRepo(db), nil)
			if _, err := svc.Update("dp", "u1", req); !errors.Is(err, ErrInvalidShareLink) {
				t.Fatalf("got %v, want ErrInvalidShareLink", err)
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Error(err)
			}
		})
	}
}

// A datapack's first link is an active viewer link.
func TestShareLinkUpdate_NewLinkDefaults(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	mock.ExpectBegin()
	mock.ExpectQuery(regexp.QuoteMeta("SELECT user_id FROM meko_system.datapacks")).
		WillReturnRows(sqlmock.NewRows([]string{"user_id"}).AddRow("owner-1"))
	mock.ExpectQuery(regexp.QuoteMeta("FOR UPDATE")).WillReturnRows(sqlmock.NewRows(nil))
	mock.ExpectQuery(regexp.QuoteMeta("INSERT INTO meko_system.datapack_share_links")).
		WithArgs("dp", sqlmock.AnyArg(), "viewer", "active", "u1").
		WillReturnError(errors.New("stop here"))
	mock.ExpectRollback()

	svc := NewShareLinkService(repository.NewShareLinkRepo(db), nil)
	_, _ = svc.Update("dp", "u1", UpdateShareLinkRequest{}) //nolint:errcheck // only the INSERT args are under test
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Error(err)
	}
}

func TestNewShareLinkToken(t *testing.T) {
	a, err := newShareLinkToken()
	if err != nil {
		t.Fatal(err)
	}
	b, _ := newShareLinkToken() //nolint:errcheck // checked above
	if len(a) != 22 || a == b {
		t.Errorf("tokens %q, %q: want two distinct 22-char tokens", a, b)
	}
	if !regexp.MustCompile(`^[A-Za-z0-9_-]+$`).MatchString(a) {
		t.Errorf("token %q is not URL-safe", a)
	}
}

func TestRemoveMember_RefusesTheOwner(t *testing.T) {
	svc := NewSharingService(nil, nil)
	if err := svc.RemoveMember("dp", "owner-1", "owner-1"); !errors.Is(err, ErrCannotRemoveOwner) {
		t.Fatalf("got %v, want ErrCannotRemoveOwner", err)
	}
}

func TestDecideRedemption(t *testing.T) {
	cases := []struct {
		name      string
		existing  repository.ExistingInvite
		linkGrant string
		want      repository.RedeemDecision
	}{
		{
			name:      "stranger takes the link's role",
			existing:  repository.ExistingInvite{},
			linkGrant: "viewer",
			want:      repository.RedeemDecision{Grant: "viewer", Source: "link"},
		},
		{
			name:      "a maintainer clicking a viewer link stays maintainer",
			existing:  repository.ExistingInvite{Grant: "maintainer", Status: "accepted", Source: "email"},
			linkGrant: "viewer",
			want:      repository.RedeemDecision{Grant: "maintainer", Source: "email"},
		},
		{
			name:      "an email viewer raised by a contributor link becomes a link member",
			existing:  repository.ExistingInvite{Grant: "viewer", Status: "accepted", Source: "email"},
			linkGrant: "contributor",
			want:      repository.RedeemDecision{Grant: "contributor", Source: "link", Change: true},
		},
		{
			name:      "a hand-set role is never raised by the link",
			existing:  repository.ExistingInvite{Grant: "viewer", Status: "accepted", Source: "link_override"},
			linkGrant: "contributor",
			want:      repository.RedeemDecision{Grant: "viewer", Source: "link_override"},
		},
		{
			name:      "a pending invite above the link keeps its role as an email member",
			existing:  repository.ExistingInvite{Grant: "contributor", Status: "pending", Source: "email"},
			linkGrant: "viewer",
			want:      repository.RedeemDecision{Grant: "contributor", Source: "email"},
		},
		{
			name:      "a pending invite at or below the link joins as a link member",
			existing:  repository.ExistingInvite{Grant: "viewer", Status: "pending", Source: "email"},
			linkGrant: "contributor",
			want:      repository.RedeemDecision{Grant: "contributor", Source: "link"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := decideRedemption(tc.existing, tc.linkGrant); got != tc.want {
				t.Errorf("got %+v, want %+v", got, tc.want)
			}
		})
	}
}
