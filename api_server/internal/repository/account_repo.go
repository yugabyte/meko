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
type AccountRepo struct {
	db *sql.DB
}

// NewAccountRepo creates a new AccountRepo.
func NewAccountRepo(db *sql.DB) *AccountRepo {
	return &AccountRepo{db: db}
}

// Create inserts a new account into account_registry and returns the
// created account.
func (r *AccountRepo) Create(
	ctx context.Context,
	params CreateAccountParams,
) (*models.Account, error) {
	slog.Debug("creating account",
		"email", params.Email,
		"cognito_sub", params.CognitoSub,
	)

	var acct models.Account
	err := r.db.QueryRowContext(ctx,
		`INSERT INTO meko_system.account_registry
			(account_first_name, account_last_name,
			 account_email, cognito_sub)
		 VALUES ($1, $2, $3, $4)
		 RETURNING id, account_first_name, account_last_name,
			account_email, account_type, cognito_sub,
			email_verified_at, has_completed_onboarding, created_at, updated_at`,
		params.FirstName, params.LastName,
		params.Email, params.CognitoSub,
	).Scan(
		&acct.ID, &acct.FirstName, &acct.LastName,
		&acct.Email, &acct.AccountType, &acct.CognitoSub,
		&acct.EmailVerifiedAt, &acct.HasCompletedOnboarding, &acct.CreatedAt, &acct.UpdatedAt,
	)
	if err != nil {
		slog.Error("failed to create account",
			"email", params.Email, "error", err,
		)
		return nil, fmt.Errorf("failed to create account: %w", err)
	}

	slog.Info("account created",
		"account_id", acct.ID,
		"email", acct.Email,
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
		`SELECT id, account_first_name, account_last_name,
			account_email, account_type, cognito_sub,
			email_verified_at, has_completed_onboarding, created_at, updated_at
		 FROM meko_system.account_registry
		 WHERE cognito_sub = $1`,
		sub,
	).Scan(
		&acct.ID, &acct.FirstName, &acct.LastName,
		&acct.Email, &acct.AccountType, &acct.CognitoSub,
		&acct.EmailVerifiedAt, &acct.HasCompletedOnboarding, &acct.CreatedAt, &acct.UpdatedAt,
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
		`SELECT id, account_first_name, account_last_name,
			account_email, account_type, cognito_sub,
			email_verified_at, has_completed_onboarding, created_at, updated_at
		 FROM meko_system.account_registry
		 WHERE id = $1`,
		id,
	).Scan(
		&acct.ID, &acct.FirstName, &acct.LastName,
		&acct.Email, &acct.AccountType, &acct.CognitoSub,
		&acct.EmailVerifiedAt, &acct.HasCompletedOnboarding, &acct.CreatedAt, &acct.UpdatedAt,
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
// account-level MCP user.
type MCPCredentials struct {
	UserName string
	Password string
}

// GetMCPCredentials returns the account-level MCP YSQL role credentials.
// Returns nil when the account has no MCP user provisioned yet.
func (r *AccountRepo) GetMCPCredentials(
	ctx context.Context,
	accountID string,
) (*MCPCredentials, error) {
	slog.Debug("looking up account MCP credentials",
		"account_id", accountID,
	)

	var userName, password sql.NullString
	err := r.db.QueryRowContext(ctx,
		`SELECT mcp_user_name, mcp_user_password
		 FROM meko_system.account_registry
		 WHERE id = $1`,
		accountID,
	).Scan(&userName, &password)
	if err != nil {
		slog.Error("failed to look up MCP credentials",
			"account_id", accountID, "error", err,
		)
		return nil, fmt.Errorf("failed to look up MCP credentials: %w", err)
	}

	if !userName.Valid || userName.String == "" {
		slog.Debug("no MCP credentials set for account",
			"account_id", accountID,
		)
		return nil, nil
	}

	return &MCPCredentials{
		UserName: userName.String,
		Password: password.String,
	}, nil
}

// SetMCPCredentials stores MCP YSQL role credentials on the account.
func (r *AccountRepo) SetMCPCredentials(
	ctx context.Context,
	accountID, userName, password string,
) error {
	slog.Info("storing MCP credentials on account",
		"account_id", accountID, "mcp_user", userName,
	)

	res, err := r.db.ExecContext(ctx,
		`UPDATE meko_system.account_registry
		 SET mcp_user_name = $1, mcp_user_password = $2, updated_at = NOW()
		 WHERE id = $3`,
		userName, password, accountID,
	)
	if err != nil {
		slog.Error("failed to store MCP credentials",
			"account_id", accountID, "error", err,
		)
		return fmt.Errorf("failed to store MCP credentials: %w", err)
	}

	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("failed to check rows affected: %w", err)
	}
	if n == 0 {
		return fmt.Errorf("account %q not found", accountID)
	}
	return nil
}

// VerifyEmail sets the email_verified_at timestamp for the account
// matching the given email address.
func (r *AccountRepo) VerifyEmail(
	ctx context.Context,
	email string,
) error {
	slog.Debug("verifying email", "email", email)

	res, err := r.db.ExecContext(ctx,
		`UPDATE meko_system.account_registry
		 SET email_verified_at = NOW(), updated_at = NOW()
		 WHERE account_email = $1
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

// SetHasCompletedOnboarding updates onboarding completion status for an account.
func (r *AccountRepo) SetHasCompletedOnboarding(
	ctx context.Context,
	accountID string,
	completed bool,
) error {
	res, err := r.db.ExecContext(ctx,
		`UPDATE meko_system.account_registry
		 SET has_completed_onboarding = $1, updated_at = NOW()
		 WHERE id = $2`,
		completed, accountID,
	)
	if err != nil {
		return fmt.Errorf("failed to update onboarding completion status: %w", err)
	}

	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("failed to check rows affected: %w", err)
	}
	if n == 0 {
		return fmt.Errorf("account %q not found", accountID)
	}
	return nil
}
