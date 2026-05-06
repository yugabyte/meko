package repository

import (
	"database/sql"
	"fmt"
	"log/slog"

	"github.com/lib/pq"

	"meko/api_server/internal/models"
)

var (
	ErrInviteExists   = fmt.Errorf("pending invite already exists for this email and datapack")
	ErrInviteNotFound = fmt.Errorf("invite not found")
)

// SharingRepo encapsulates database operations for datapack sharing.
type SharingRepo struct {
	db *sql.DB
}

// NewSharingRepo creates a new SharingRepo.
func NewSharingRepo(db *sql.DB) *SharingRepo {
	return &SharingRepo{db: db}
}

// CreateInviteParams holds the inputs for creating a new invite.
type CreateInviteParams struct {
	DatapackID      string
	OwnerUserID     string
	RecipientEmail  string
	RecipientUserID *string
	GrantRole       string
}

// CreateInvite inserts a new pending invite. Returns ErrInviteExists if
// a pending invite already exists for the same datapack + email.
func (r *SharingRepo) CreateInvite(p CreateInviteParams) (*models.DatapackInvite, error) {
	slog.Info("creating invite",
		"datapack_id", p.DatapackID,
		"recipient_email", p.RecipientEmail,
		"grant", p.GrantRole,
	)

	var inv models.DatapackInvite
	err := r.db.QueryRow(
		`INSERT INTO meko_system.datapack_invites
			(datapack_id, owner_user_id, recipient_email, recipient_user_id, grant_role)
		 VALUES ($1, $2, $3, $4, $5)
		 RETURNING invite_id, datapack_id, owner_user_id, recipient_email,
		           recipient_user_id, grant_role, status, created_at, updated_at`,
		p.DatapackID, p.OwnerUserID, p.RecipientEmail, p.RecipientUserID, p.GrantRole,
	).Scan(
		&inv.InviteID, &inv.DatapackID, &inv.OwnerUserID, &inv.RecipientEmail,
		&inv.RecipientUserID, &inv.GrantRole, &inv.Status, &inv.CreatedAt, &inv.UpdatedAt,
	)
	if err != nil {
		if isUniqueViolation(err) {
			return nil, fmt.Errorf("%w: email=%s datapack=%s", ErrInviteExists, p.RecipientEmail, p.DatapackID)
		}
		slog.Error("failed to create invite", "error", err)
		return nil, fmt.Errorf("failed to create invite: %w", err)
	}

	return &inv, nil
}

// GetPendingInvitesByEmail returns all pending invites for a recipient email,
// with the owner's name and datapack name joined in.
func (r *SharingRepo) GetPendingInvitesByEmail(email string) ([]models.IncomingShare, error) {
	rows, err := r.db.Query(
		`SELECT i.invite_id, i.owner_user_id,
		        CONCAT(u.user_first_name, ' ', u.user_last_name),
		        i.grant_role, i.datapack_id, d.datapack_name
		 FROM meko_system.datapack_invites i
		 JOIN meko_system.users u ON u.user_id = i.owner_user_id
		 JOIN meko_system.datapacks d ON d.datapack_id = i.datapack_id
		 WHERE i.recipient_email = $1 AND i.status = 'pending'
		 ORDER BY i.created_at DESC`,
		email,
	)
	if err != nil {
		return nil, fmt.Errorf("failed to list incoming shares: %w", err)
	}
	defer rows.Close()

	var result []models.IncomingShare
	for rows.Next() {
		var s models.IncomingShare
		if err := rows.Scan(
			&s.InviteID, &s.OwnerUserID, &s.OwnerName,
			&s.GrantRole, &s.DatapackID, &s.DatapackName,
		); err != nil {
			return nil, fmt.Errorf("failed to scan incoming share: %w", err)
		}
		result = append(result, s)
	}
	return result, rows.Err()
}

// GetInviteByID returns a single invite by its ID.
func (r *SharingRepo) GetInviteByID(inviteID string) (*models.DatapackInvite, error) {
	var inv models.DatapackInvite
	err := r.db.QueryRow(
		`SELECT invite_id, datapack_id, owner_user_id, recipient_email,
		        recipient_user_id, grant_role, status, created_at, updated_at
		 FROM meko_system.datapack_invites
		 WHERE invite_id = $1`,
		inviteID,
	).Scan(
		&inv.InviteID, &inv.DatapackID, &inv.OwnerUserID, &inv.RecipientEmail,
		&inv.RecipientUserID, &inv.GrantRole, &inv.Status, &inv.CreatedAt, &inv.UpdatedAt,
	)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("failed to get invite: %w", err)
	}
	return &inv, nil
}

// AcceptInvite marks the invite as accepted and creates agent_access +
// agent_access_permissions rows for the recipient's default agent in a
// single transaction. grantPerms is the pre-computed list of
// (object_type, permission) pairs derived from the grant role.
func (r *SharingRepo) AcceptInvite(
	inviteID, recipientUserID string,
	datapackID string,
	grantPerms []ObjectPermission,
) error {
	tx, err := r.db.Begin()
	if err != nil {
		return fmt.Errorf("failed to begin transaction: %w", err)
	}
	defer func() {
		if rbErr := tx.Rollback(); rbErr != nil && rbErr != sql.ErrTxDone {
			slog.Warn("rollback error", "invite_id", inviteID, "error", rbErr)
		}
	}()

	res, err := tx.Exec(
		`UPDATE meko_system.datapack_invites
		 SET status = 'accepted', recipient_user_id = $2, updated_at = NOW()
		 WHERE invite_id = $1 AND status = 'pending'`,
		inviteID, recipientUserID,
	)
	if err != nil {
		return fmt.Errorf("failed to update invite status: %w", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return fmt.Errorf("%w: invite_id=%s is not pending", ErrInviteNotFound, inviteID)
	}

	if _, err = tx.Exec(
		`INSERT INTO meko_system.agent_access
			(user_id, agent_id, datapack_id)
		 VALUES ($1, $2, $3)
		 ON CONFLICT DO NOTHING`,
		recipientUserID, DefaultAgentID, datapackID,
	); err != nil {
		return fmt.Errorf("failed to grant agent access: %w", err)
	}

	if err = upsertPermissions(tx, recipientUserID, datapackID, grantPerms); err != nil {
		return err
	}

	return tx.Commit()
}

// upsertPermissions batch-inserts permission rows for a user+datapack
// in a single statement using unnest arrays.
func upsertPermissions(tx *sql.Tx, userID, datapackID string, perms []ObjectPermission) error {
	if len(perms) == 0 {
		return nil
	}

	objTypes := make([]string, len(perms))
	permissions := make([]string, len(perms))
	for i, p := range perms {
		objTypes[i] = p.ObjectType
		permissions[i] = p.Permission
	}

	if _, err := tx.Exec(
		`INSERT INTO meko_system.agent_access_permissions
			(user_id, agent_id, datapack_id, object_type, permission,
			 datapack_shared_status)
		 SELECT $1, $2, $3,
		        unnest($4::text[])::meko_system.access_object_type,
		        unnest($5::text[])::meko_system.access_permission,
		        'active'
		 ON CONFLICT (user_id, agent_id, datapack_id, object_type)
		 DO UPDATE SET permission = EXCLUDED.permission,
		              datapack_shared_status = 'active',
		              updated_at = NOW()`,
		userID, DefaultAgentID, datapackID,
		pq.Array(objTypes), pq.Array(permissions),
	); err != nil {
		return fmt.Errorf("failed to upsert permission rows: %w", err)
	}
	return nil
}

// ObjectPermission is a (object_type, permission) pair that maps to
// one row in agent_access_permissions.
type ObjectPermission struct {
	ObjectType string
	Permission string
}

// DeclineInvite marks a pending invite as declined.
func (r *SharingRepo) DeclineInvite(inviteID string) error {
	res, err := r.db.Exec(
		`UPDATE meko_system.datapack_invites
		 SET status = 'declined', updated_at = NOW()
		 WHERE invite_id = $1 AND status = 'pending'`,
		inviteID,
	)
	if err != nil {
		return fmt.Errorf("failed to decline invite: %w", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return fmt.Errorf("%w: invite_id=%s is not pending", ErrInviteNotFound, inviteID)
	}
	return nil
}

// RevokeInvite marks a pending invite as revoked. Only the owner can
// revoke; ownership is enforced by the service layer.
func (r *SharingRepo) RevokeInvite(inviteID string) error {
	res, err := r.db.Exec(
		`UPDATE meko_system.datapack_invites
		 SET status = 'revoked', updated_at = NOW()
		 WHERE invite_id = $1 AND status = 'pending'`,
		inviteID,
	)
	if err != nil {
		return fmt.Errorf("failed to revoke invite: %w", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return fmt.Errorf("%w: invite_id=%s is not pending", ErrInviteNotFound, inviteID)
	}
	return nil
}

// GetOutgoingMembers returns users who have accepted an invite and have
// active access to the datapack. The owner is also included with
// is_owner=true.
func (r *SharingRepo) GetOutgoingMembers(datapackID, ownerUserID string) ([]models.OutgoingShareMember, error) {
	rows, err := r.db.Query(
		`SELECT DISTINCT ON (i.recipient_user_id)
		        u.user_id,
		        CONCAT(u.user_first_name, ' ', u.user_last_name),
		        u.user_email,
		        i.grant_role
		 FROM meko_system.datapack_invites i
		 JOIN meko_system.users u ON u.user_id = i.recipient_user_id
		 WHERE i.datapack_id = $1 AND i.status = 'accepted'
		 ORDER BY i.recipient_user_id, i.updated_at DESC`,
		datapackID,
	)
	if err != nil {
		return nil, fmt.Errorf("failed to list outgoing members: %w", err)
	}
	defer rows.Close()

	var ownerName, ownerEmail string
	err = r.db.QueryRow(
		`SELECT CONCAT(user_first_name, ' ', user_last_name), user_email
		 FROM meko_system.users WHERE user_id = $1`,
		ownerUserID,
	).Scan(&ownerName, &ownerEmail)
	if err != nil {
		return nil, fmt.Errorf("failed to look up owner: %w", err)
	}

	result := []models.OutgoingShareMember{
		{
			UserID:    ownerUserID,
			IsOwner:   true,
			UserName:  ownerName,
			UserEmail: ownerEmail,
			GrantRole: string(models.GrantMaintainer),
		},
	}

	for rows.Next() {
		var m models.OutgoingShareMember
		if err := rows.Scan(&m.UserID, &m.UserName, &m.UserEmail, &m.GrantRole); err != nil {
			return nil, fmt.Errorf("failed to scan outgoing member: %w", err)
		}
		if m.UserID == ownerUserID {
			continue
		}
		result = append(result, m)
	}
	return result, rows.Err()
}

// GetOutgoingPendingInvites returns pending invites for a datapack.
func (r *SharingRepo) GetOutgoingPendingInvites(datapackID string) ([]models.OutgoingSharePendingInvite, error) {
	rows, err := r.db.Query(
		`SELECT i.invite_id, i.grant_role, i.recipient_user_id,
		        CASE WHEN u.user_id IS NOT NULL
		             THEN CONCAT(u.user_first_name, ' ', u.user_last_name)
		             ELSE NULL END,
		        i.recipient_email
		 FROM meko_system.datapack_invites i
		 LEFT JOIN meko_system.users u ON u.user_email = i.recipient_email
		 WHERE i.datapack_id = $1 AND i.status = 'pending'
		 ORDER BY i.created_at DESC`,
		datapackID,
	)
	if err != nil {
		return nil, fmt.Errorf("failed to list pending invites: %w", err)
	}
	defer rows.Close()

	var result []models.OutgoingSharePendingInvite
	for rows.Next() {
		var p models.OutgoingSharePendingInvite
		if err := rows.Scan(
			&p.InviteID, &p.GrantRole, &p.RecipientUserID,
			&p.RecipientUserName, &p.RecipientEmailID,
		); err != nil {
			return nil, fmt.Errorf("failed to scan pending invite: %w", err)
		}
		result = append(result, p)
	}
	return result, rows.Err()
}

// UpdateMemberGrant updates the grant (permission rows) for an active
// member. Replaces the existing agent_access_permissions rows with the
// new permission set. Also updates the grant_role on the accepted invite.
func (r *SharingRepo) UpdateMemberGrant(
	datapackID, memberUserID, newGrantRole string,
	newPerms []ObjectPermission,
) error {
	tx, err := r.db.Begin()
	if err != nil {
		return fmt.Errorf("failed to begin transaction: %w", err)
	}
	defer func() {
		if rbErr := tx.Rollback(); rbErr != nil && rbErr != sql.ErrTxDone {
			slog.Warn("rollback error", "error", rbErr)
		}
	}()

	var exists bool
	if err = tx.QueryRow(
		`SELECT EXISTS (
		     SELECT 1 FROM meko_system.agent_access
		     WHERE user_id = $1 AND datapack_id = $2
		 )`,
		memberUserID, datapackID,
	).Scan(&exists); err != nil {
		return fmt.Errorf("failed to check member access: %w", err)
	}
	if !exists {
		return fmt.Errorf("member not found for datapack")
	}

	if err = upsertPermissions(tx, memberUserID, datapackID, newPerms); err != nil {
		return err
	}

	if _, err = tx.Exec(
		`UPDATE meko_system.datapack_invites
		 SET grant_role = $1, updated_at = NOW()
		 WHERE datapack_id = $2 AND recipient_user_id = $3 AND status = 'accepted'`,
		newGrantRole, datapackID, memberUserID,
	); err != nil {
		return fmt.Errorf("failed to update invite grant_role: %w", err)
	}

	return tx.Commit()
}

// BackfillRecipientUserID sets recipient_user_id on all pending invites
// that match the given email but have a NULL recipient_user_id. Called
// after a new user signs up so their pending invites become actionable.
func (r *SharingRepo) BackfillRecipientUserID(email, userID string) (int64, error) {
	res, err := r.db.Exec(
		`UPDATE meko_system.datapack_invites
		 SET recipient_user_id = $1, updated_at = NOW()
		 WHERE recipient_email = $2 AND recipient_user_id IS NULL AND status = 'pending'`,
		userID, email,
	)
	if err != nil {
		return 0, fmt.Errorf("failed to backfill recipient_user_id: %w", err)
	}
	n, _ := res.RowsAffected()
	if n > 0 {
		slog.Info("backfilled recipient_user_id on pending invites",
			"email", email, "user_id", userID, "count", n,
		)
	}
	return n, nil
}

// LookupUserByEmail returns a user's ID if they exist, nil otherwise.
func (r *SharingRepo) LookupUserByEmail(email string) (*string, error) {
	var id string
	err := r.db.QueryRow(
		`SELECT user_id FROM meko_system.users WHERE user_email = $1`,
		email,
	).Scan(&id)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("failed to look up user by email: %w", err)
	}
	return &id, nil
}

// UserIdentity carries the minimal profile fields needed by sharing sync flows.
type UserIdentityForShareSync struct {
	UserID    string
	FirstName string
	LastName  string
	Email     string
}

// GetUserIdentity returns first_name/last_name/email for a given user_id.
func (r *SharingRepo) GetUserIdentityForShareSync(userID string) (*UserIdentityForShareSync, error) {
	var out UserIdentityForShareSync
	err := r.db.QueryRow(
		`SELECT user_id, user_first_name, user_last_name, user_email
		 FROM meko_system.users
		 WHERE user_id = $1`,
		userID,
	).Scan(&out.UserID, &out.FirstName, &out.LastName, &out.Email)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("failed to look up user identity: %w", err)
	}
	return &out, nil
}

// isUniqueViolation checks if the error is a Postgres unique violation (23505).
func isUniqueViolation(err error) bool {
	if err == nil {
		return false
	}
	return contains(err.Error(), "duplicate key value") ||
		contains(err.Error(), "23505")
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && searchString(s, sub)
}

func searchString(s, sub string) bool {
	for i := 0; i <= len(s)-len(sub); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
