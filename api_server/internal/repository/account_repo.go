package repository

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"

	"meko/api_server/internal/models"
)

// CreateAccountParams holds the input for creating a new account.
type CreateAccountParams struct {
	FirstName  string
	LastName   string
	Email      string
	CognitoSub string
}

// AccountRepo encapsulates database operations for user accounts.
//
// Backing table: meko_system.users.
type AccountRepo struct {
	db *sql.DB
}

// NewAccountRepo creates a new AccountRepo.
func NewAccountRepo(db *sql.DB) *AccountRepo {
	return &AccountRepo{db: db}
}

// Create inserts a new user row and the user's default agent in a
// single transaction, then returns the created account.
//
// The default agent is the only row ever inserted into
// meko_system.agents at signup. Subsequent datapack creates link an
// existing agent into meko_system.agent_datapacks but never write to
// meko_system.agents themselves.
func (r *AccountRepo) Create(
	ctx context.Context,
	params CreateAccountParams,
) (*models.Account, error) {
	slog.Debug("creating account",
		"email", params.Email,
		"cognito_sub", params.CognitoSub,
	)

	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to begin transaction: %w", err)
	}
	defer func() {
		if rbErr := tx.Rollback(); rbErr != nil && rbErr != sql.ErrTxDone {
			slog.Warn("rollback error",
				"email", params.Email, "error", rbErr)
		}
	}()

	var acct models.Account
	err = tx.QueryRowContext(ctx,
		`INSERT INTO meko_system.users
			(user_first_name, user_last_name,
			 user_email, cognito_sub)
		 VALUES ($1, $2, $3, $4)
		 RETURNING user_id, user_first_name, user_last_name,
			user_email, user_type, user_org, cognito_sub,
			email_verified_at, has_completed_onboarding,
			created_at, updated_at`,
		params.FirstName, params.LastName,
		params.Email, params.CognitoSub,
	).Scan(
		&acct.ID, &acct.FirstName, &acct.LastName,
		&acct.Email, &acct.AccountType, &acct.UserOrg, &acct.CognitoSub,
		&acct.EmailVerifiedAt, &acct.HasCompletedOnboarding,
		&acct.CreatedAt, &acct.UpdatedAt,
	)
	if err != nil {
		slog.Error("failed to create account",
			"email", params.Email, "error", err,
		)
		return nil, fmt.Errorf("failed to create account: %w", err)
	}

	if _, err = tx.ExecContext(ctx,
		`INSERT INTO meko_system.agents (user_id, agent_id, agent_details)
		 VALUES ($1, $2, '{}'::jsonb)
		 ON CONFLICT (user_id, agent_id) DO NOTHING`,
		acct.ID, DefaultAgentID,
	); err != nil {
		slog.Error("failed to insert default agent",
			"user_id", acct.ID, "agent_id", DefaultAgentID, "error", err,
		)
		return nil, fmt.Errorf("failed to insert default agent: %w", err)
	}

	if err = tx.Commit(); err != nil {
		return nil, fmt.Errorf("failed to commit account creation: %w", err)
	}

	slog.Info("account created with default agent",
		"user_id", acct.ID,
		"email", acct.Email,
		"default_agent_id", DefaultAgentID,
	)
	return &acct, nil
}

// GetByCognitoSub looks up an account by its Cognito user sub.
// Returns (nil, nil) if no matching account exists.
func (r *AccountRepo) GetByCognitoSub(
	ctx context.Context,
	sub string,
) (*models.Account, error) {
	slog.Debug("looking up account by cognito sub",
		"cognito_sub", sub,
	)

	var acct models.Account
	err := r.db.QueryRowContext(ctx,
		`SELECT user_id, user_first_name, user_last_name,
			user_email, user_type, user_org, cognito_sub,
			email_verified_at, has_completed_onboarding,
			created_at, updated_at
		 FROM meko_system.users
		 WHERE cognito_sub = $1`,
		sub,
	).Scan(
		&acct.ID, &acct.FirstName, &acct.LastName,
		&acct.Email, &acct.AccountType, &acct.UserOrg, &acct.CognitoSub,
		&acct.EmailVerifiedAt, &acct.HasCompletedOnboarding,
		&acct.CreatedAt, &acct.UpdatedAt,
	)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		slog.Error("failed to look up account",
			"cognito_sub", sub, "error", err,
		)
		return nil, fmt.Errorf(
			"failed to look up account by cognito sub: %w", err,
		)
	}

	return &acct, nil
}

// GetByID looks up an account by its internal UUID.
// Returns (nil, nil) if no matching account exists.
func (r *AccountRepo) GetByID(
	ctx context.Context,
	id string,
) (*models.Account, error) {
	var acct models.Account
	err := r.db.QueryRowContext(ctx,
		`SELECT user_id, user_first_name, user_last_name,
			user_email, user_type, user_org, cognito_sub,
			email_verified_at, has_completed_onboarding,
			created_at, updated_at
		 FROM meko_system.users
		 WHERE user_id = $1`,
		id,
	).Scan(
		&acct.ID, &acct.FirstName, &acct.LastName,
		&acct.Email, &acct.AccountType, &acct.UserOrg, &acct.CognitoSub,
		&acct.EmailVerifiedAt, &acct.HasCompletedOnboarding,
		&acct.CreatedAt, &acct.UpdatedAt,
	)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("failed to look up account by id: %w", err)
	}
	return &acct, nil
}

// EnsureAccount returns the existing account for the given Cognito sub,
// or auto-creates one when no local account exists yet (e.g. users who
// signed up through the Cognito Hosted UI).
func (r *AccountRepo) EnsureAccount(
	ctx context.Context,
	sub, email string,
) (*models.Account, error) {
	acct, err := r.GetByCognitoSub(ctx, sub)
	if err != nil {
		return nil, err
	}
	if acct != nil {
		return acct, nil
	}

	slog.Info("auto-provisioning local account",
		"cognito_sub", sub, "email", email,
	)
	return r.Create(ctx, CreateAccountParams{
		Email:      email,
		CognitoSub: sub,
	})
}

// MCPCredentials holds the YSQL role name and password for the
// user-level MCP role. Stored on meko_system.users.user_details JSONB
// under the `mcp_credentials` key.
type MCPCredentials struct {
	UserName string
	Password string
}

// GetMCPCredentials returns the user-level MCP YSQL role credentials.
// Returns (nil, nil) when the user has no MCP credentials yet (both
// fields null in the JSONB document).
func (r *AccountRepo) GetMCPCredentials(
	ctx context.Context,
	userID string,
) (*MCPCredentials, error) {
	slog.Debug("looking up user MCP credentials",
		"user_id", userID,
	)

	var userName, password sql.NullString
	err := r.db.QueryRowContext(ctx,
		`SELECT user_details->'mcp_credentials'->>'mcp_user_name',
		        user_details->'mcp_credentials'->>'mcp_user_password'
		 FROM meko_system.users
		 WHERE user_id = $1`,
		userID,
	).Scan(&userName, &password)
	if err != nil {
		slog.Error("failed to look up MCP credentials",
			"user_id", userID, "error", err,
		)
		return nil, fmt.Errorf("failed to look up MCP credentials: %w", err)
	}

	if !userName.Valid || userName.String == "" {
		slog.Debug("no MCP credentials set for user",
			"user_id", userID,
		)
		return nil, nil
	}

	return &MCPCredentials{
		UserName: userName.String,
		Password: password.String,
	}, nil
}

// SetMCPCredentials stores MCP YSQL role credentials on the user's
// user_details JSONB column.
func (r *AccountRepo) SetMCPCredentials(
	ctx context.Context,
	userID, userName, password string,
) error {
	slog.Info("storing MCP credentials on user",
		"user_id", userID, "mcp_user", userName,
	)

	res, err := r.db.ExecContext(ctx,
		`UPDATE meko_system.users
		 SET user_details = jsonb_set(
		         jsonb_set(
		             COALESCE(user_details, '{}'::jsonb),
		             '{mcp_credentials,mcp_user_name}',
		             to_jsonb($1::text)
		         ),
		         '{mcp_credentials,mcp_user_password}',
		         to_jsonb($2::text)
		     ),
		     updated_at = NOW()
		 WHERE user_id = $3`,
		userName, password, userID,
	)
	if err != nil {
		slog.Error("failed to store MCP credentials",
			"user_id", userID, "error", err,
		)
		return fmt.Errorf("failed to store MCP credentials: %w", err)
	}

	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("failed to check rows affected: %w", err)
	}
	if n == 0 {
		return fmt.Errorf("user %q not found", userID)
	}
	return nil
}

// VerifyEmail sets the email_verified_at timestamp for the user matching
// the given email address.
func (r *AccountRepo) VerifyEmail(
	ctx context.Context,
	email string,
) error {
	slog.Debug("verifying email", "email", email)

	res, err := r.db.ExecContext(ctx,
		`UPDATE meko_system.users
		 SET email_verified_at = NOW(), updated_at = NOW()
		 WHERE user_email = $1
		   AND email_verified_at IS NULL`,
		email,
	)
	if err != nil {
		slog.Error("failed to verify email",
			"email", email, "error", err,
		)
		return fmt.Errorf("failed to verify email: %w", err)
	}

	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("failed to check rows affected: %w", err)
	}
	if n == 0 {
		slog.Warn("verify email matched no rows",
			"email", email,
		)
		return fmt.Errorf(
			"no unverified account found for email %q", email,
		)
	}

	slog.Info("email verified", "email", email)
	return nil
}

// SetHasCompletedOnboarding updates onboarding completion status for a user.
func (r *AccountRepo) SetHasCompletedOnboarding(
	ctx context.Context,
	userID string,
	completed bool,
) error {
	res, err := r.db.ExecContext(ctx,
		`UPDATE meko_system.users
		 SET has_completed_onboarding = $1, updated_at = NOW()
		 WHERE user_id = $2`,
		completed, userID,
	)
	if err != nil {
		return fmt.Errorf("failed to update onboarding completion status: %w", err)
	}

	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("failed to check rows affected: %w", err)
	}
	if n == 0 {
		return fmt.Errorf("user %q not found", userID)
	}
	return nil
}
