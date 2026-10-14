package repository

import (
	"errors"
	"regexp"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/lib/pq"

	"meko/api_server/internal/models"
)

func permsForTest(grant string) []ObjectPermission {
	return []ObjectPermission{{ObjectType: "memory", Permission: grant}}
}

const (
	lockJoiner = "FROM meko_system.users WHERE user_id = $1 FOR UPDATE"
	// Shared: joins don't block each other, only a role change or reset.
	lockLink = "FOR SHARE OF l"
	// The caller's invite, locked: first by user id, then a pending invite by email.
	findByUser  = "AND datapack_id = $1 ORDER BY (status = 'accepted') DESC, updated_at DESC LIMIT 1 FOR UPDATE"
	findByEmail = "lower(recipient_email) = lower($2) ORDER BY updated_at DESC LIMIT 1 FOR UPDATE"
	grantAccess = "INSERT INTO meko_system.agent_access"
	upsertPerms = "INSERT INTO meko_system.agent_access_permissions"
	countLink   = "COUNT(*) FILTER (WHERE source = 'link' AND grant_role = $2)"
)

func expectLock(mock sqlmock.Sqlmock, linkGrant, owner string) {
	mock.ExpectBegin()
	mock.ExpectExec(regexp.QuoteMeta(lockJoiner)).WithArgs("u1").WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery(regexp.QuoteMeta(lockLink)).WithArgs("tok").
		WillReturnRows(sqlmock.NewRows([]string{"datapack_id", "grant_role", "user_id"}).AddRow("dp", linkGrant, owner))
}

func expectInvite(mock sqlmock.Sqlmock, rows *sqlmock.Rows) {
	mock.ExpectQuery(regexp.QuoteMeta(findByUser)).WithArgs("dp", "u1").WillReturnRows(rows)
}

// expectRemovalCheck answers whether an owner removed the caller, which a
// redeem asks only when the caller has no accepted or pending invite.
func expectRemovalCheck(mock sqlmock.Sqlmock, removed bool) {
	mock.ExpectQuery(regexp.QuoteMeta("status = 'revoked'")).WithArgs("dp", "u1", "removed").
		WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(removed))
}

// expectEmailInvite is the second lookup, run only when the caller has no
// invite under their user id.
func expectEmailInvite(mock sqlmock.Sqlmock, rows *sqlmock.Rows) {
	mock.ExpectQuery(regexp.QuoteMeta(findByUser)).WithArgs("dp", "u1").WillReturnRows(inviteRows())
	mock.ExpectQuery(regexp.QuoteMeta(findByEmail)).WithArgs("dp", "u1@x.com").WillReturnRows(rows)
}

func inviteRows() *sqlmock.Rows {
	return sqlmock.NewRows([]string{"invite_id", "grant_role", "status", "source"})
}

func expectJoin(mock sqlmock.Sqlmock) {
	mock.ExpectExec(regexp.QuoteMeta(grantAccess)).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(regexp.QuoteMeta(upsertPerms)).WillReturnResult(sqlmock.NewResult(0, 2))
	// Nothing is written to the link row: a join leaves it alone.
	mock.ExpectCommit()
}

// The repo applies whatever the service decides; these cases pin how each
// existing-invite state is written. The policy itself is tested in service.
func TestRedeem(t *testing.T) {
	cases := map[string]struct {
		decision RedeemDecision
		setup    func(sqlmock.Sqlmock)
		want     models.ShareLinkRedemption
	}{
		"stranger is written as a new member": {
			decision: RedeemDecision{Grant: "viewer", Source: "link"},
			setup: func(m sqlmock.Sqlmock) {
				expectLock(m, "viewer", "owner-1")
				expectEmailInvite(m, inviteRows())
				expectRemovalCheck(m, false)
				m.ExpectExec(regexp.QuoteMeta("VALUES ($1, $2, $3, $4, $5, 'accepted', $6)")).
					WithArgs("dp", "owner-1", "u1@x.com", "u1", "viewer", "link").WillReturnResult(sqlmock.NewResult(0, 1))
				expectJoin(m)
			},
			want: models.ShareLinkRedemption{DatapackID: "dp", Grant: "viewer", NewMember: true, OwnerUserID: "owner-1"},
		},
		"the owner changes nothing": {
			setup: func(m sqlmock.Sqlmock) {
				expectLock(m, "viewer", "u1")
				m.ExpectCommit()
			},
			want: models.ShareLinkRedemption{DatapackID: "dp", Grant: "owner", AlreadyMember: true, OwnerUserID: "u1"},
		},
		"an unchanged member gets its access row back": {
			decision: RedeemDecision{Grant: "maintainer", Source: "email"},
			setup: func(m sqlmock.Sqlmock) {
				expectLock(m, "viewer", "owner-1")
				expectInvite(m, inviteRows().AddRow("inv-1", "maintainer", "accepted", "email"))
				m.ExpectExec(regexp.QuoteMeta(grantAccess)).WithArgs("u1", sqlmock.AnyArg(), "dp").
					WillReturnResult(sqlmock.NewResult(0, 0))
				m.ExpectCommit()
			},
			want: models.ShareLinkRedemption{DatapackID: "dp", Grant: "maintainer", AlreadyMember: true, OwnerUserID: "owner-1"},
		},
		"a raised member is rewritten with its new source": {
			decision: RedeemDecision{Grant: "contributor", Source: "link", Change: true},
			setup: func(m sqlmock.Sqlmock) {
				expectLock(m, "contributor", "owner-1")
				expectInvite(m, inviteRows().AddRow("inv-1", "viewer", "accepted", "email"))
				m.ExpectExec(regexp.QuoteMeta("SET grant_role = $1, source = $2")).
					WithArgs("contributor", "link", "inv-1").WillReturnResult(sqlmock.NewResult(0, 1))
				m.ExpectExec(regexp.QuoteMeta(upsertPerms)).WillReturnResult(sqlmock.NewResult(0, 2))
				m.ExpectExec(regexp.QuoteMeta(grantAccess)).WillReturnResult(sqlmock.NewResult(0, 0))
				m.ExpectCommit()
			},
			want: models.ShareLinkRedemption{
				DatapackID: "dp", Grant: "contributor", AlreadyMember: true, Raised: true, OwnerUserID: "owner-1",
			},
		},
		"a pending invite sent to the caller's email is found by email": {
			decision: RedeemDecision{Grant: "viewer", Source: "link"},
			setup: func(m sqlmock.Sqlmock) {
				expectLock(m, "viewer", "owner-1")
				expectEmailInvite(m, inviteRows().AddRow("inv-2", "viewer", "pending", "email"))
				m.ExpectExec(regexp.QuoteMeta("SET status = 'accepted'")).
					WithArgs("u1", "viewer", "link", "inv-2").WillReturnResult(sqlmock.NewResult(0, 1))
				expectJoin(m)
			},
			want: models.ShareLinkRedemption{DatapackID: "dp", Grant: "viewer", NewMember: true, OwnerUserID: "owner-1"},
		},
		"a pending invite is accepted with the decided role and source": {
			decision: RedeemDecision{Grant: "contributor", Source: "email"},
			setup: func(m sqlmock.Sqlmock) {
				expectLock(m, "viewer", "owner-1")
				expectInvite(m, inviteRows().AddRow("inv-1", "contributor", "pending", "email"))
				m.ExpectExec(regexp.QuoteMeta("SET status = 'accepted'")).
					WithArgs("u1", "contributor", "email", "inv-1").WillReturnResult(sqlmock.NewResult(0, 1))
				expectJoin(m)
			},
			want: models.ShareLinkRedemption{DatapackID: "dp", Grant: "contributor", NewMember: true, OwnerUserID: "owner-1"},
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			db, mock, err := sqlmock.New()
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			tc.setup(mock)
			decide := func(ExistingInvite, string) RedeemDecision { return tc.decision }

			got, err := NewShareLinkRepo(db).Redeem("tok", "u1", "u1@x.com", decide, permsForTest)
			if err != nil {
				t.Fatalf("Redeem: %v", err)
			}
			if *got != tc.want {
				t.Errorf("got %+v, want %+v", *got, tc.want)
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Error(err)
			}
		})
	}
}

// An unknown, off or expired token all fail the same way and write nothing.
func TestRedeem_UnavailableLink(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	mock.ExpectBegin()
	mock.ExpectExec(regexp.QuoteMeta(lockJoiner)).WithArgs("u1").WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery(regexp.QuoteMeta(lockLink)).WithArgs("tok").
		WillReturnRows(sqlmock.NewRows([]string{"datapack_id", "grant_role", "user_id"}))
	mock.ExpectRollback()

	if _, err := NewShareLinkRepo(db).Redeem("tok", "u1", "u1@x.com", nil, permsForTest); err != ErrShareLinkNotFound {
		t.Fatalf("got %v, want ErrShareLinkNotFound", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Error(err)
	}
}

var shareLinkRow = []string{"datapack_id", "token", "grant_role", "status",
	"expires_at", "created_by_user_id", "created_at", "updated_at"}

// countRows answers the link-member count query: all link joiners, and those
// still at the link's role.
func countRows(joined, atRole int) *sqlmock.Rows {
	return sqlmock.NewRows([]string{"joined", "at_role"}).AddRow(joined, atRole)
}

func expectUpsert(mock sqlmock.Sqlmock, oldGrant, newGrant string) {
	mock.ExpectBegin()
	mock.ExpectQuery(regexp.QuoteMeta("SELECT user_id FROM meko_system.datapacks")).WithArgs("dp").
		WillReturnRows(sqlmock.NewRows([]string{"user_id"}).AddRow("owner-1"))
	lock := sqlmock.NewRows([]string{"grant_role", "status"})
	if oldGrant != "" {
		lock.AddRow(oldGrant, "active")
	}
	mock.ExpectQuery(regexp.QuoteMeta("FOR UPDATE")).WithArgs("dp").WillReturnRows(lock)
	save := "INSERT INTO meko_system.datapack_share_links"
	if oldGrant != "" {
		save = "UPDATE meko_system.datapack_share_links"
	}
	mock.ExpectQuery(regexp.QuoteMeta(save)).
		WillReturnRows(sqlmock.NewRows(shareLinkRow).
			AddRow("dp", "tok", newGrant, "active", nil, "owner-1", time.Now(), time.Now()))
}

// A role change moves the link's members still at the old role, both ways.
func TestUpsert_RoleChangeMovesLinkMembers(t *testing.T) {
	for _, tc := range []struct{ from, to string }{{"viewer", "contributor"}, {"contributor", "viewer"}} {
		t.Run(tc.from+"_to_"+tc.to, func(t *testing.T) {
			db, mock, err := sqlmock.New()
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			expectUpsert(mock, tc.from, tc.to)
			mock.ExpectQuery(regexp.QuoteMeta("AND source = 'link' AND grant_role = $3")).
				WithArgs(tc.to, "dp", tc.from).
				WillReturnRows(sqlmock.NewRows([]string{"recipient_user_id"}).AddRow("u1").AddRow("u2"))
			// Both moved members in one statement, not one per member.
			mock.ExpectExec(regexp.QuoteMeta("FROM unnest($1::uuid[]) AS u(user_id)")).
				WithArgs(pq.Array([]string{"u1", "u2"}), DefaultAgentID, "dp", sqlmock.AnyArg(), sqlmock.AnyArg()).
				WillReturnResult(sqlmock.NewResult(0, 2))
			mock.ExpectQuery(regexp.QuoteMeta(countLink)).
				WithArgs("dp", tc.to).WillReturnRows(countRows(3, 2))
			mock.ExpectCommit()

			got, err := NewShareLinkRepo(db).Upsert("dp", "new-tok", tc.to, "active", "owner-1", false, permsForTest)
			if err != nil {
				t.Fatal(err)
			}
			if got.Link.RedemptionCount != 3 {
				t.Errorf("joined via link = %d, want the 3 the invites count", got.Link.RedemptionCount)
			}
			if len(got.Moved) != 2 || got.OwnerUserID != "owner-1" || got.Link.MembersAtLinkRole != 2 {
				t.Errorf("got moved %v owner %q members %d, want u1,u2 / owner-1 / 2",
					got.Moved, got.OwnerUserID, got.Link.MembersAtLinkRole)
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Error(err)
			}
		})
	}
}

// Turning the link off or on, or creating it, moves nobody.
func TestUpsert_SameRoleMovesNobody(t *testing.T) {
	for name, old := range map[string]string{"status change": "viewer", "new link": ""} {
		t.Run(name, func(t *testing.T) {
			db, mock, err := sqlmock.New()
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			expectUpsert(mock, old, "viewer")
			mock.ExpectQuery(regexp.QuoteMeta(countLink)).
				WithArgs("dp", "viewer").WillReturnRows(countRows(0, 0))
			mock.ExpectCommit()

			got, err := NewShareLinkRepo(db).Upsert("dp", "new-tok", "viewer", "off", "owner-1", false, permsForTest)
			if err != nil || len(got.Moved) != 0 {
				t.Fatalf("got %+v, %v, want nobody moved", got, err)
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Error(err)
			}
		})
	}
}

// A first-time PUT that loses the race to create the link reports
// ErrShareLinkRace, so the service can retry through the UPDATE path.
func TestUpsert_ConcurrentCreateReportsTheRace(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	mock.ExpectBegin()
	mock.ExpectQuery(regexp.QuoteMeta("SELECT user_id FROM meko_system.datapacks")).
		WillReturnRows(sqlmock.NewRows([]string{"user_id"}).AddRow("owner-1"))
	mock.ExpectQuery(regexp.QuoteMeta("FOR UPDATE")).WillReturnRows(sqlmock.NewRows([]string{"grant_role", "status"}))
	mock.ExpectQuery(regexp.QuoteMeta("INSERT INTO meko_system.datapack_share_links")).
		WillReturnError(errors.New(`pq: duplicate key value violates unique constraint "datapack_share_links_pkey"`))
	mock.ExpectRollback()

	_, err = NewShareLinkRepo(db).Upsert("dp", "tok", "viewer", "active", "owner-1", false, permsForTest)
	if !errors.Is(err, ErrShareLinkRace) {
		t.Fatalf("got %v, want ErrShareLinkRace", err)
	}
}

func TestRemoveMember(t *testing.T) {
	t.Run("revokes the invite, cancels pending ones and deletes access", func(t *testing.T) {
		db, mock, err := sqlmock.New()
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close()
		mock.ExpectBegin()
		// Marked removed, so the share link won't let them back in.
		mock.ExpectQuery(regexp.QuoteMeta("SET status = 'revoked', source = $3")).WithArgs("dp", "u1", "removed").
			WillReturnRows(sqlmock.NewRows([]string{"recipient_email"}).AddRow("u1@x.com"))
		// Pending invites sent before the removal are canceled, by user id and by
		// email, so neither the invite nor the link lets them back in.
		mock.ExpectExec(regexp.QuoteMeta("status = 'pending' AND recipient_user_id = $2")).WithArgs("dp", "u1").
			WillReturnResult(sqlmock.NewResult(0, 1))
		mock.ExpectExec(regexp.QuoteMeta("status = 'pending' AND lower(recipient_email) = lower($2)")).
			WithArgs("dp", "u1@x.com").WillReturnResult(sqlmock.NewResult(0, 0))
		// agent_id is in the predicate so the delete uses the primary key.
		mock.ExpectExec(regexp.QuoteMeta("DELETE FROM meko_system.agent_access_permissions")).
			WithArgs("u1", DefaultAgentID, "dp").
			WillReturnResult(sqlmock.NewResult(0, 2))
		mock.ExpectExec(regexp.QuoteMeta("DELETE FROM meko_system.agent_access WHERE")).WithArgs("u1", "dp").
			WillReturnResult(sqlmock.NewResult(0, 1))
		mock.ExpectCommit()

		email, err := NewSharingRepo(db).RemoveMember("dp", "u1")
		if err != nil || email != "u1@x.com" {
			t.Fatalf("got (%q, %v), want (u1@x.com, nil)", email, err)
		}
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Error(err)
		}
	})
	t.Run("someone with no accepted invite is not found and nothing is deleted", func(t *testing.T) {
		db, mock, err := sqlmock.New()
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close()
		mock.ExpectBegin()
		mock.ExpectQuery(regexp.QuoteMeta("SET status = 'revoked'")).
			WillReturnRows(sqlmock.NewRows([]string{"recipient_email"}))
		mock.ExpectRollback()

		if _, err := NewSharingRepo(db).RemoveMember("dp", "u1"); !errors.Is(err, ErrMemberNotFound) {
			t.Fatalf("got %v, want ErrMemberNotFound", err)
		}
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Error(err)
		}
	})
}

// A field the request leaves out keeps the value on the locked row, so a
// concurrent update of the other field isn't reverted.
func TestUpsert_OmittedFieldsKeepTheLockedValues(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	mock.ExpectBegin()
	mock.ExpectQuery(regexp.QuoteMeta("SELECT user_id FROM meko_system.datapacks")).
		WillReturnRows(sqlmock.NewRows([]string{"user_id"}).AddRow("owner-1"))
	mock.ExpectQuery(regexp.QuoteMeta("FOR UPDATE")).
		WillReturnRows(sqlmock.NewRows([]string{"grant_role", "status"}).AddRow("contributor", "active"))
	// The token isn't in the SET list at all: "ELSE token" reads NULL on
	// YugabyteDB's single-row update path and fails the NOT NULL constraint.
	mock.ExpectQuery(regexp.QuoteMeta("SET grant_role = $2, status = $3, updated_at = now()")).
		WithArgs("dp", "contributor", "off").
		WillReturnRows(sqlmock.NewRows(shareLinkRow).
			AddRow("dp", "tok", "contributor", "off", nil, "owner-1", time.Now(), time.Now()))
	mock.ExpectQuery(regexp.QuoteMeta(countLink)).
		WillReturnRows(countRows(0, 0))
	mock.ExpectCommit()

	if _, err := NewShareLinkRepo(db).Upsert("dp", "tok", "", "off", "owner-1", false, permsForTest); err != nil {
		t.Fatal(err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Error(err)
	}
}

// Changing a link member's role by hand marks the row link_override, so later
// link role changes and redeems leave it; an email member keeps its source. The
// invite is written before the permissions, the order a link role change uses,
// so the two can't deadlock.
func TestUpdateMemberGrant_MarksALinkMemberAsOverridden(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	mock.ExpectBegin()
	mock.ExpectQuery(regexp.QuoteMeta("FROM meko_system.agent_access")).
		WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(true))
	mock.ExpectExec(regexp.QuoteMeta(
		"source = CASE WHEN source IN ('link', 'link_override') THEN 'link_override' ELSE source END")).
		WithArgs("viewer", "dp", "u1").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(regexp.QuoteMeta(upsertPerms)).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	if err := NewSharingRepo(db).UpdateMemberGrant("dp", "u1", "viewer", permsForTest("viewer")); err != nil {
		t.Fatal(err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Error(err)
	}
}

// A redeem that loses a serialization race is retried as a whole.
func TestRedeem_RetriesASerializationFailure(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	mock.ExpectBegin()
	mock.ExpectExec(regexp.QuoteMeta(lockJoiner)).WithArgs("u1").WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery(regexp.QuoteMeta(lockLink)).
		WillReturnError(&pq.Error{Code: "40001", Message: "could not serialize access due to concurrent update"})
	mock.ExpectRollback()
	expectLock(mock, "viewer", "owner-1")
	expectInvite(mock, inviteRows().AddRow("inv-1", "viewer", "accepted", "link"))
	mock.ExpectExec(regexp.QuoteMeta(grantAccess)).WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectCommit()

	decide := func(ex ExistingInvite, _ string) RedeemDecision {
		return RedeemDecision{Grant: ex.Grant, Source: ex.Source}
	}
	got, err := NewShareLinkRepo(db).Redeem("tok", "u1", "u1@x.com", decide, permsForTest)
	if err != nil || !got.AlreadyMember {
		t.Fatalf("got %+v, %v; want the retry to succeed as an existing member", got, err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Error(err)
	}
}

// A redeem that loses every attempt reports ErrShareLinkBusy, so the handler can
// answer 503 rather than 500.
func TestRedeem_ExhaustedRetriesAreBusy(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for range redeemAttempts {
		mock.ExpectBegin()
		mock.ExpectExec(regexp.QuoteMeta(lockJoiner)).WithArgs("u1").WillReturnResult(sqlmock.NewResult(0, 1))
		mock.ExpectQuery(regexp.QuoteMeta(lockLink)).
			WillReturnError(&pq.Error{Code: "40001", Message: "could not serialize access due to concurrent update"})
		mock.ExpectRollback()
	}

	decide := func(ex ExistingInvite, _ string) RedeemDecision { return RedeemDecision{Grant: ex.Grant} }
	_, err = NewShareLinkRepo(db).Redeem("tok", "u1", "u1@x.com", decide, permsForTest)
	if !errors.Is(err, ErrShareLinkBusy) || !isSerializationFailure(err) {
		t.Fatalf("got %v, want ErrShareLinkBusy wrapping the serialization failure", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Error(err)
	}
}

// Reset replaces the token of an existing link, so a leaked URL stops working.
func TestUpsert_ResetReplacesTheToken(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	mock.ExpectBegin()
	mock.ExpectQuery(regexp.QuoteMeta("SELECT user_id FROM meko_system.datapacks")).
		WillReturnRows(sqlmock.NewRows([]string{"user_id"}).AddRow("owner-1"))
	mock.ExpectQuery(regexp.QuoteMeta("FOR UPDATE")).
		WillReturnRows(sqlmock.NewRows([]string{"grant_role", "status"}).AddRow("viewer", "active"))
	mock.ExpectQuery(regexp.QuoteMeta("SET grant_role = $2, status = $3, token = $4, updated_at = now()")).
		WithArgs("dp", "viewer", "active", "fresh-tok").
		WillReturnRows(sqlmock.NewRows(shareLinkRow).
			AddRow("dp", "fresh-tok", "viewer", "active", nil, "owner-1", time.Now(), time.Now()))
	mock.ExpectQuery(regexp.QuoteMeta(countLink)).
		WillReturnRows(countRows(0, 0))
	mock.ExpectCommit()

	got, err := NewShareLinkRepo(db).Upsert("dp", "fresh-tok", "", "", "owner-1", true, permsForTest)
	if err != nil {
		t.Fatal(err)
	}
	if got.Link.Token != "fresh-tok" {
		t.Errorf("token %q after a reset, want the new one", got.Link.Token)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Error(err)
	}
}

// Someone an owner removed can't come back through the link.
func TestRedeem_RefusesARemovedMember(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	expectLock(mock, "viewer", "owner-1")
	expectEmailInvite(mock, inviteRows())
	expectRemovalCheck(mock, true)
	mock.ExpectRollback()

	decide := func(ExistingInvite, string) RedeemDecision { return RedeemDecision{Grant: "viewer", Source: "link"} }
	_, err = NewShareLinkRepo(db).Redeem("tok", "u1", "u1@x.com", decide, permsForTest)
	if !errors.Is(err, ErrRemovedFromDatapack) {
		t.Fatalf("got %v, want ErrRemovedFromDatapack", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Error(err)
	}
}

// A role change on a link that is off is refused: it would move every past
// joiner while the link isn't in use.
func TestUpsert_RefusesARoleChangeWhileOff(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	mock.ExpectBegin()
	mock.ExpectQuery(regexp.QuoteMeta("SELECT user_id FROM meko_system.datapacks")).
		WillReturnRows(sqlmock.NewRows([]string{"user_id"}).AddRow("owner-1"))
	mock.ExpectQuery(regexp.QuoteMeta("FOR UPDATE")).
		WillReturnRows(sqlmock.NewRows([]string{"grant_role", "status"}).AddRow("viewer", "off"))
	mock.ExpectRollback()

	_, err = NewShareLinkRepo(db).Upsert("dp", "tok", "contributor", "", "owner-1", false, permsForTest)
	if !errors.Is(err, ErrShareLinkOffRoleChange) {
		t.Fatalf("got %v, want ErrShareLinkOffRoleChange", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Error(err)
	}
}
