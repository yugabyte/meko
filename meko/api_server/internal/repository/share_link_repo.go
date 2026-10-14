package repository

import (
	"cmp"
	"database/sql"
	"errors"
	"fmt"
	"math/rand/v2"
	"strings"
	"time"

	"meko/api_server/internal/models"
)

// ErrShareLinkNotFound covers a token that doesn't exist, is off, or has
// expired. The three collapse into one error so a caller can't probe which.
var ErrShareLinkNotFound = errors.New("share link not found")

// ErrRemovedFromDatapack means an owner removed the caller from the link's
// datapack. Only a new email invite brings them back.
var ErrRemovedFromDatapack = errors.New("removed from this datapack")

// ErrShareLinkOffRoleChange means a role change was asked of a link that is
// off. It would move everyone who ever joined through the link with nobody
// watching, so the link has to be turned on first (or in the same request).
var ErrShareLinkOffRoleChange = errors.New("share link is off")

// ErrShareLinkRace means a concurrent request created the datapack's link
// first; retrying updates the link it created.
var ErrShareLinkRace = errors.New("share link created concurrently")

// ErrShareLinkBusy is a redeem that kept losing to concurrent joins; trying again
// shortly is expected to work.
var ErrShareLinkBusy = errors.New("share link busy")

// ShareLinkRepo stores the one share link a datapack can have and redeems it.
type ShareLinkRepo struct {
	db *sql.DB
}

func NewShareLinkRepo(db *sql.DB) *ShareLinkRepo {
	return &ShareLinkRepo{db: db}
}

const shareLinkColumns = `datapack_id, token, grant_role, status,
	expires_at, created_by_user_id, created_at, updated_at`

func scanShareLink(row interface{ Scan(...any) error }) (*models.DatapackShareLink, error) {
	var l models.DatapackShareLink
	var expires sql.NullTime
	if err := row.Scan(&l.DatapackID, &l.Token, &l.GrantRole, &l.Status,
		&expires, &l.CreatedByUserID, &l.CreatedAt, &l.UpdatedAt); err != nil {
		return nil, err
	}
	if expires.Valid {
		l.ExpiresAt = &expires.Time
	}
	return &l, nil
}

// Get returns the datapack's link, or nil when it has never had one.
func (r *ShareLinkRepo) Get(datapackID string) (*models.DatapackShareLink, error) {
	l, err := scanShareLink(r.db.QueryRow(
		`SELECT `+shareLinkColumns+` FROM meko_system.datapack_share_links WHERE datapack_id = $1`,
		datapackID,
	))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get share link: %w", err)
	}
	if err = countLinkMembers(r.db, l); err != nil {
		return nil, err
	}
	return l, nil
}

type rowQuerier interface {
	QueryRow(query string, args ...any) *sql.Row
}

// countLinkMembers fills the link's member counts from the invites. Counted on
// read rather than kept on the link row, so a join writes nothing to that row and
// joins to one link don't queue behind each other.
func countLinkMembers(q rowQuerier, l *models.DatapackShareLink) error {
	if err := q.QueryRow(
		`SELECT COUNT(*),
		        COUNT(*) FILTER (WHERE source = 'link' AND grant_role = $2)
		 FROM   meko_system.datapack_invites
		 WHERE  datapack_id = $1 AND status = 'accepted' AND source IN ('link', 'link_override')`,
		l.DatapackID, l.GrantRole,
	).Scan(&l.RedemptionCount, &l.MembersAtLinkRole); err != nil {
		return fmt.Errorf("count link members: %w", err)
	}
	return nil
}

// ShareLinkUpdate is a saved link plus the members its role change moved.
type ShareLinkUpdate struct {
	Link        *models.DatapackShareLink
	OwnerUserID string
	Moved       []string
}

// Upsert creates the datapack's link with newToken, or updates the role and
// status of the one it has. The token of an existing link changes only when
// resetToken is set, which replaces it with newToken so the old URL stops
// working. An empty grantRole or status keeps the link's current value (viewer
// and active for a new link).
//
// The link's role is the role of everyone who joined through it, so a role
// change moves those members with it, in either direction. Only members still
// at the old role move: one whose role was changed individually keeps it.
// permsFor maps a grant to its agent_access_permissions rows.
func (r *ShareLinkRepo) Upsert(
	datapackID, newToken, grantRole, status, createdByUserID string,
	resetToken bool,
	permsFor func(grant string) []ObjectPermission,
) (*ShareLinkUpdate, error) {
	tx, err := r.db.Begin()
	if err != nil {
		return nil, fmt.Errorf("begin share link update: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // no-op after Commit

	out := &ShareLinkUpdate{}
	if err = tx.QueryRow(
		`SELECT user_id FROM meko_system.datapacks WHERE datapack_id = $1`, datapackID,
	).Scan(&out.OwnerUserID); err != nil {
		return nil, fmt.Errorf("look up datapack owner: %w", err)
	}
	// Locked so a concurrent redeem can't join at the old role after the move,
	// and so an omitted field keeps the value a concurrent update just wrote.
	var oldGrant, oldStatus string
	err = tx.QueryRow(
		`SELECT grant_role, status FROM meko_system.datapack_share_links WHERE datapack_id = $1 FOR UPDATE`, datapackID,
	).Scan(&oldGrant, &oldStatus)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("lock share link: %w", err)
	}
	if grantRole == "" {
		grantRole = cmp.Or(oldGrant, string(models.GrantViewer))
	}
	if status == "" {
		status = cmp.Or(oldStatus, models.ShareLinkStatusActive)
	}
	if oldGrant != "" && grantRole != oldGrant && status == models.ShareLinkStatusOff {
		return nil, ErrShareLinkOffRoleChange
	}

	// A plain UPDATE or INSERT, never INSERT ... ON CONFLICT: on YugabyteDB an
	// upsert of a row this transaction already locked FOR UPDATE waits on the
	// transaction's own lock and never returns.
	if oldGrant != "" {
		// token is set only on a reset, never as "ELSE token": an UPDATE keyed on
		// the whole primary key takes YugabyteDB's single-row fast path, which
		// doesn't read the row, so a SET that refers to the old value reads NULL.
		set, args := "", []any{datapackID, grantRole, status}
		if resetToken {
			set, args = "token = $4, ", append(args, newToken)
		}
		out.Link, err = scanShareLink(tx.QueryRow(
			`UPDATE meko_system.datapack_share_links
			 SET grant_role = $2, status = $3, `+set+`updated_at = now()
			 WHERE datapack_id = $1
			 RETURNING `+shareLinkColumns,
			args...,
		))
	} else {
		out.Link, err = scanShareLink(tx.QueryRow(
			`INSERT INTO meko_system.datapack_share_links
				(datapack_id, token, grant_role, status, created_by_user_id)
			 VALUES ($1, $2, $3, $4, $5)
			 RETURNING `+shareLinkColumns,
			datapackID, newToken, grantRole, status, createdByUserID,
		))
	}
	if err != nil {
		if isUniqueViolation(err) {
			return nil, ErrShareLinkRace
		}
		return nil, fmt.Errorf("save share link: %w", err)
	}

	if oldGrant != "" && oldGrant != grantRole {
		rows, qerr := tx.Query(
			`UPDATE meko_system.datapack_invites
			 SET grant_role = $1, updated_at = now()
			 WHERE datapack_id = $2 AND status = 'accepted' AND source = 'link' AND grant_role = $3
			 RETURNING recipient_user_id`,
			grantRole, datapackID, oldGrant,
		)
		if qerr != nil {
			return nil, fmt.Errorf("move link members to %s: %w", grantRole, qerr)
		}
		for rows.Next() {
			var uid string
			if err = rows.Scan(&uid); err != nil {
				rows.Close()
				return nil, fmt.Errorf("scan moved member: %w", err)
			}
			out.Moved = append(out.Moved, uid)
		}
		rows.Close()
		if err = rows.Err(); err != nil {
			return nil, fmt.Errorf("move link members: %w", err)
		}
		// One statement for every moved member: per-member writes took 21s at
		// 10k members, all while holding the link's lock.
		if err = upsertPermissionsForUsers(tx, out.Moved, datapackID, permsFor(grantRole)); err != nil {
			return nil, err
		}
	}
	if err = countLinkMembers(tx, out.Link); err != nil {
		return nil, err
	}
	return out, tx.Commit()
}

// Preview returns the name and description of the datapack behind an active
// link.
func (r *ShareLinkRepo) Preview(token string) (*models.ShareLinkPreview, error) {
	var p models.ShareLinkPreview
	err := r.db.QueryRow(
		`SELECT d.datapack_name, COALESCE(d.datapack_details->>'description', '')
		 FROM   meko_system.datapack_share_links l
		 JOIN   meko_system.datapacks d ON d.datapack_id = l.datapack_id
		 WHERE  l.token = $1 AND l.status = 'active'
		   AND  (l.expires_at IS NULL OR l.expires_at > now())`,
		token,
	).Scan(&p.DatapackName, &p.Description)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrShareLinkNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("preview share link: %w", err)
	}
	return &p, nil
}

// ExistingInvite is the caller's current invite on a datapack, as a redeem
// finds it under the link's lock. Status is "" when there is none.
type ExistingInvite struct {
	Grant, Status, Source string
}

// RedeemDecision is the role and source a redeem leaves the caller with.
// Change says an accepted member's row must be rewritten.
type RedeemDecision struct {
	Grant, Source string
	Change        bool
}

// Redeem joins userID to the datapack behind an active link, in one
// transaction that locks the link row so concurrent redeems of one link
// serialize. decide is the service's policy for which role the caller ends
// with; this method only applies it:
//
//   - the owner: nothing changes;
//   - an accepted member: their row is rewritten when decide says so, and
//     their agent_access row is restored if it was unlinked;
//   - a pending email invite: accepted with the decided role and source;
//   - anyone else: an accepted invite is written.
//
// The last two count as new members. permsFor maps a grant to its
// agent_access_permissions rows.
//
// Many people joining one link at once can fail with a serialization error
// once YugabyteDB's own retries run out; the whole transaction is retried.
func (r *ShareLinkRepo) Redeem(
	token, userID, email string,
	decide func(existing ExistingInvite, linkGrant string) RedeemDecision,
	permsFor func(grant string) []ObjectPermission,
) (*models.ShareLinkRedemption, error) {
	var (
		out *models.ShareLinkRedemption
		err error
	)
	for attempt := 1; attempt <= redeemAttempts; attempt++ {
		out, err = r.redeemOnce(token, userID, email, decide, permsFor)
		if !isSerializationFailure(err) {
			return out, err
		}
		if attempt < redeemAttempts {
			// Jittered, so joins that collided once don't collide again on the same schedule.
			backoff := time.Duration(attempt*attempt) * redeemRetryBackoff
			time.Sleep(backoff/2 + rand.N(backoff)) //nolint:gosec // jitter, not security
		}
	}
	return nil, fmt.Errorf("%w: %w", ErrShareLinkBusy, err)
}

const (
	redeemAttempts     = 4
	redeemRetryBackoff = 20 * time.Millisecond
)

func (r *ShareLinkRepo) redeemOnce(
	token, userID, email string,
	decide func(existing ExistingInvite, linkGrant string) RedeemDecision,
	permsFor func(grant string) []ObjectPermission,
) (*models.ShareLinkRedemption, error) {
	tx, err := r.db.Begin()
	if err != nil {
		return nil, fmt.Errorf("begin redeem: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // no-op after Commit

	// One join at a time per person, so a double click makes one member; joins
	// by different people don't wait on each other.
	if _, err = tx.Exec(`SELECT 1 FROM meko_system.users WHERE user_id = $1 FOR UPDATE`, userID); err != nil {
		return nil, fmt.Errorf("lock joiner: %w", err)
	}
	// Shared: joins only read the link, while a role change or reset takes it
	// exclusively, so no one joins at the old role mid-change.
	var datapackID, linkGrant, ownerUserID string
	err = tx.QueryRow(
		`SELECT l.datapack_id, l.grant_role, d.user_id
		 FROM   meko_system.datapack_share_links l
		 JOIN   meko_system.datapacks d ON d.datapack_id = l.datapack_id
		 WHERE  l.token = $1 AND l.status = 'active'
		   AND  (l.expires_at IS NULL OR l.expires_at > now())
		 FOR SHARE OF l`,
		token,
	).Scan(&datapackID, &linkGrant, &ownerUserID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrShareLinkNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("lock share link: %w", err)
	}

	out := &models.ShareLinkRedemption{DatapackID: datapackID, OwnerUserID: ownerUserID}
	if ownerUserID == userID {
		out.Grant, out.AlreadyMember = models.GrantOwner, true
		return out, tx.Commit()
	}

	// Locked: a removal or revoke committing between this read and the writes
	// below would otherwise be overwritten with an accepted membership.
	// Two lookups, each on an index: the caller's own invites by user id
	// (idx_invites_recipient_status), then a pending invite sent to their email
	// (idx_invites_datapack_lower_email_pending). One query with an OR read every
	// member's invite.
	var inviteID string
	var existing ExistingInvite
	err = tx.QueryRow(
		`SELECT invite_id, grant_role, status, COALESCE(source, 'email')
		 FROM   meko_system.datapack_invites
		 WHERE  recipient_user_id = $2
		   AND  status IN ('accepted', 'pending')
		   AND  datapack_id = $1
		 ORDER  BY (status = 'accepted') DESC, updated_at DESC
		 LIMIT  1
		 FOR UPDATE`,
		datapackID, userID,
	).Scan(&inviteID, &existing.Grant, &existing.Status, &existing.Source)
	if errors.Is(err, sql.ErrNoRows) {
		err = tx.QueryRow(
			`SELECT invite_id, grant_role, status, COALESCE(source, 'email')
			 FROM   meko_system.datapack_invites
			 WHERE  datapack_id = $1
			   AND  status = 'pending'
			   AND  lower(recipient_email) = lower($2)
			 ORDER  BY updated_at DESC
			 LIMIT  1
			 FOR UPDATE`,
			datapackID, email,
		).Scan(&inviteID, &existing.Grant, &existing.Status, &existing.Source)
	}
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("look up existing invite: %w", err)
	}
	if existing.Status == "" {
		var removed bool
		if err = tx.QueryRow(
			`SELECT EXISTS (
			     SELECT 1 FROM meko_system.datapack_invites
			     WHERE  recipient_user_id = $2 AND status = 'revoked'
			       AND  datapack_id = $1 AND source = $3)`,
			datapackID, userID, models.InviteSourceRemoved,
		).Scan(&removed); err != nil {
			return nil, fmt.Errorf("check for a removal: %w", err)
		}
		if removed {
			return nil, ErrRemovedFromDatapack
		}
	}

	d := decide(existing, linkGrant)
	out.Grant = d.Grant

	switch existing.Status {
	case "accepted":
		out.AlreadyMember = true
		if d.Change {
			out.Raised = true
			if _, err = tx.Exec(
				`UPDATE meko_system.datapack_invites SET grant_role = $1, source = $2, updated_at = now() WHERE invite_id = $3`,
				d.Grant, d.Source, inviteID,
			); err != nil {
				return nil, fmt.Errorf("raise member grant: %w", err)
			}
			if err = upsertPermissions(tx, userID, datapackID, permsFor(d.Grant)); err != nil {
				return nil, err
			}
		}
		// An accepted invite can outlive its access row (unlinking an agent
		// deletes agent_access and keeps the invite); restore it.
		if err = grantAgentAccess(tx, userID, datapackID); err != nil {
			return nil, err
		}
		return out, tx.Commit()
	case "pending":
		_, err = tx.Exec(
			`UPDATE meko_system.datapack_invites
			 SET status = 'accepted', recipient_user_id = $1, grant_role = $2, source = $3, updated_at = now()
			 WHERE invite_id = $4`,
			userID, d.Grant, d.Source, inviteID,
		)
	default:
		_, err = tx.Exec(
			`INSERT INTO meko_system.datapack_invites
				(datapack_id, owner_user_id, recipient_email, recipient_user_id, grant_role, status, source)
			 VALUES ($1, $2, $3, $4, $5, 'accepted', $6)`,
			datapackID, ownerUserID, strings.ToLower(strings.TrimSpace(email)), userID, d.Grant, d.Source,
		)
	}
	if err != nil {
		return nil, fmt.Errorf("record link membership: %w", err)
	}
	if err = grantAgentAccess(tx, userID, datapackID); err != nil {
		return nil, err
	}
	if err = upsertPermissions(tx, userID, datapackID, permsFor(d.Grant)); err != nil {
		return nil, err
	}
	out.NewMember = true
	return out, tx.Commit()
}

func grantAgentAccess(tx *sql.Tx, userID, datapackID string) error {
	if _, err := tx.Exec(
		`INSERT INTO meko_system.agent_access (user_id, agent_id, datapack_id)
		 VALUES ($1, $2, $3) ON CONFLICT DO NOTHING`,
		userID, DefaultAgentID, datapackID,
	); err != nil {
		return fmt.Errorf("grant agent access: %w", err)
	}
	return nil
}
