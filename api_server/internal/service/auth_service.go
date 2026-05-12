package service

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"meko/api_server/internal/auth"
	"meko/api_server/internal/models"
	"meko/api_server/internal/repository"
)

// CognitoProvider abstracts the Cognito operations used by AuthService,
// enabling test doubles.
type CognitoProvider interface {
	SignUp(
		ctx context.Context,
		email, password, firstName, lastName string,
	) (string, error)
	ConfirmSignUp(ctx context.Context, email, code string) error
	ResendConfirmationCode(ctx context.Context, email string) error
	ForgotPassword(ctx context.Context, email string) error
	ConfirmForgotPassword(ctx context.Context, email, code, newPassword string) error
	Login(
		ctx context.Context, email, password string,
	) (*auth.Result, error)
	CompleteNewPassword(
		ctx context.Context,
		email, newPassword, session, firstName, lastName string,
	) (*auth.Result, error)
	Logout(ctx context.Context, accessToken string) error
	ChangePassword(
		ctx context.Context,
		accessToken, previousPassword, proposedPassword string,
	) error
}

// ErrForgotPasswordNoAccount is returned when Cognito has no user for this email
// on the forgot-password flow (mapped to HTTP 404 in the auth handler).
var ErrForgotPasswordNoAccount = errors.New("no account exists for this email address")

// Password validation errors for the forgot-password / set-password flows.
// They are returned as typed sentinels so the handler can map them to
// HTTP 400 VALIDATION_ERROR explicitly instead of falling through to a
// generic 500.
var (
	ErrPasswordTooShort = errors.New("password must be more than 6 characters")
	ErrPasswordMismatch = errors.New("password and confirmation do not match")
)

// AccountStore abstracts the account repository operations used by
// AuthService, enabling test doubles.
type AccountStore interface {
	Create(
		ctx context.Context, params repository.CreateAccountParams,
	) (*models.Account, error)
	GetByCognitoSub(
		ctx context.Context, sub string,
	) (*models.Account, error)
	VerifyEmail(ctx context.Context, email string) error
	SetHasCompletedOnboarding(ctx context.Context, accountID string, completed bool) error
}

// Provisioner abstracts post-signup account provisioning (default
// datapack + CRD creation), enabling test doubles.
type Provisioner interface {
	ProvisionNewAccount(ctx context.Context, accountID string)
}

// InviteBackfiller sets recipient_user_id on pending datapack_invites
// that match the new user's email. Called after account creation so
// the new user can see and accept their pending invites.
type InviteBackfiller interface {
	BackfillRecipientUserID(email, userID string) (int64, error)
}

// AuthService orchestrates user authentication operations: signup,
// email confirmation, login, and logout.
type AuthService struct {
	cognito          CognitoProvider
	accounts         AccountStore
	provisioner      Provisioner
	inviteBackfiller InviteBackfiller
}

// AuthServiceOption configures optional AuthService dependencies.
type AuthServiceOption func(*AuthService)

// WithProvisioner sets the provisioning service that is triggered when
// a new account is JIT-created via EnsureAccountFromIDToken.
func WithProvisioner(p Provisioner) AuthServiceOption {
	return func(s *AuthService) { s.provisioner = p }
}

// WithInviteBackfiller sets the invite backfiller that updates pending
// invites with the new user's ID after account creation.
func WithInviteBackfiller(b InviteBackfiller) AuthServiceOption {
	return func(s *AuthService) { s.inviteBackfiller = b }
}

// NewAuthService creates a new AuthService.
func NewAuthService(
	cognito CognitoProvider,
	accounts AccountStore,
	opts ...AuthServiceOption,
) *AuthService {
	s := &AuthService{
		cognito:  cognito,
		accounts: accounts,
	}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

// SignupRequest holds the input for user registration.
type SignupRequest struct {
	Email     string `json:"email"`
	Password  string `json:"password"`
	FirstName string `json:"first_name"`
	LastName  string `json:"last_name"`
}

// SignupResponse holds the result of a successful signup.
type SignupResponse struct {
	AccountID string `json:"account_id"`
	Email     string `json:"email"`
	Message   string `json:"message"`
}

// Signup registers a new user with Cognito and creates a local account.
func (s *AuthService) Signup(
	ctx context.Context, req SignupRequest,
) (*SignupResponse, error) {
	if err := validateSignupRequest(req); err != nil {
		return nil, err
	}

	slog.Info("starting user signup", "email", req.Email)

	userSub, err := s.cognito.SignUp(
		ctx, req.Email, req.Password, req.FirstName, req.LastName,
	)
	if err != nil {
		return nil, fmt.Errorf("cognito signup: %w", err)
	}

	acct, err := s.accounts.Create(ctx, repository.CreateAccountParams{
		FirstName:  req.FirstName,
		LastName:   req.LastName,
		Email:      req.Email,
		CognitoSub: userSub,
	})
	if err != nil {
		slog.Error("failed to create local account after cognito signup",
			"email", req.Email,
			"cognito_sub", userSub,
			"error", err,
		)
		return nil, fmt.Errorf("creating account: %w", err)
	}

	slog.Info("user signup completed",
		"account_id", acct.ID,
		"email", req.Email,
	)

	s.backfillInvites(acct.Email, acct.ID)

	if s.provisioner != nil {
		slog.Info("Triggering post-signup provisioning",
			"account_id", acct.ID,
		)
		go s.provisioner.ProvisionNewAccount(context.Background(), acct.ID)
	}

	return &SignupResponse{
		AccountID: acct.ID,
		Email:     acct.Email,
		Message: fmt.Sprintf(
			"confirmation code sent to %s", req.Email,
		),
	}, nil
}

// ConfirmSignupRequest holds the input for email confirmation.
type ConfirmSignupRequest struct {
	Email string `json:"email"`
	Code  string `json:"code"`
}

// ConfirmSignup verifies a user's email with the Cognito confirmation
// code and marks the local account as verified.
func (s *AuthService) ConfirmSignup(
	ctx context.Context, req ConfirmSignupRequest,
) error {
	if req.Email == "" {
		return fmt.Errorf("email is required")
	}
	if req.Code == "" {
		return fmt.Errorf("confirmation code is required")
	}

	slog.Info("confirming user email", "email", req.Email)

	if err := s.cognito.ConfirmSignUp(ctx, req.Email, req.Code); err != nil {
		return fmt.Errorf("cognito confirm signup: %w", err)
	}

	if err := s.accounts.VerifyEmail(ctx, req.Email); err != nil {
		slog.Error("failed to mark email verified in db",
			"email", req.Email, "error", err,
		)
		return fmt.Errorf("verifying email in database: %w", err)
	}

	slog.Info("user email confirmed", "email", req.Email)
	return nil
}

// ResendConfirmationRequest is the body for POST /auth/resend-confirmation.
type ResendConfirmationRequest struct {
	Email string `json:"email"`
}

// ResendConfirmationEmail asks Cognito to send another verification code
// for an unconfirmed user. Used when sign-in fails with USER_NOT_CONFIRMED.
func (s *AuthService) ResendConfirmationEmail(
	ctx context.Context, req ResendConfirmationRequest,
) error {
	email := strings.TrimSpace(req.Email)
	if email == "" {
		return fmt.Errorf("email is required")
	}
	if err := s.cognito.ResendConfirmationCode(ctx, email); err != nil {
		return fmt.Errorf("cognito resend confirmation: %w", err)
	}
	return nil
}

// ForgotPasswordStartRequest is the body for POST /auth/forgot-password.
type ForgotPasswordStartRequest struct {
	Email string `json:"email"`
}

// ForgotPasswordSendCode triggers Cognito ForgotPassword (sends reset code to email).
func (s *AuthService) ForgotPasswordSendCode(
	ctx context.Context, req ForgotPasswordStartRequest,
) error {
	email := strings.TrimSpace(req.Email)
	if email == "" {
		return fmt.Errorf("email is required")
	}
	if err := s.cognito.ForgotPassword(ctx, email); err != nil {
		if errors.Is(err, auth.ErrUserNotFound) {
			return ErrForgotPasswordNoAccount
		}
		return fmt.Errorf("cognito forgot password: %w", err)
	}
	return nil
}

// ForgotPasswordConfirmRequest is the body for POST /auth/forgot-password/confirm.
type ForgotPasswordConfirmRequest struct {
	Email           string `json:"email"`
	Code            string `json:"code"`
	NewPassword     string `json:"new_password"`
	ConfirmPassword string `json:"confirm_password"`
}

// minForgotPasswordLen is client+server minimum; Cognito pool policy may require more.
const minForgotPasswordLen = 7 // strictly more than 6 characters

// ConfirmForgotPasswordReset validates input and calls Cognito ConfirmForgotPassword.
func (s *AuthService) ConfirmForgotPasswordReset(
	ctx context.Context, req ForgotPasswordConfirmRequest,
) error {
	email := strings.TrimSpace(req.Email)
	code := strings.TrimSpace(req.Code)
	newPw := req.NewPassword
	confirm := req.ConfirmPassword
	if email == "" {
		return fmt.Errorf("email is required")
	}
	if code == "" {
		return fmt.Errorf("verification code is required")
	}
	if len(newPw) < minForgotPasswordLen {
		return ErrPasswordTooShort
	}
	if newPw != confirm {
		return ErrPasswordMismatch
	}
	if err := s.cognito.ConfirmForgotPassword(ctx, email, code, newPw); err != nil {
		return fmt.Errorf("cognito confirm forgot password: %w", err)
	}
	return nil
}

// LoginRequest holds the input for user login.
type LoginRequest struct {
	Email    string `json:"email"`
	Password string `json:"password"`
}

// ChallengeNewPasswordRequired is returned in LoginResponse.Challenge
// when the authenticated user must set a permanent password before
// receiving tokens (i.e. accounts created via AdminCreateUser that
// are still in FORCE_CHANGE_PASSWORD state).
const ChallengeNewPasswordRequired = "NEW_PASSWORD_REQUIRED"

// LoginResponse holds the tokens returned by a successful login. When
// Cognito requires the user to set a new password before issuing
// tokens, the token fields are empty and Challenge/Session/Email are
// populated instead. The client must then call POST
// /auth/complete-new-password with the session to finish the login.
type LoginResponse struct {
	AccessToken  string `json:"access_token,omitempty"`
	IDToken      string `json:"id_token,omitempty"`
	RefreshToken string `json:"refresh_token,omitempty"`
	ExpiresIn    int32  `json:"expires_in,omitempty"`

	Challenge string `json:"challenge,omitempty"`
	Session   string `json:"session,omitempty"`
	Email     string `json:"email,omitempty"`
}

// CompleteNewPasswordRequest carries the input for completing the
// NEW_PASSWORD_REQUIRED challenge issued by Login.
//
// FirstName / LastName are required whenever the Cognito User Pool
// marks given_name / family_name as required attributes and the
// admin-created user does not already have them set. They are
// forwarded to Cognito as `userAttributes.given_name` /
// `userAttributes.family_name` in the challenge response, and they
// are what JIT account creation reads from the resulting ID token to
// populate the local account.
type CompleteNewPasswordRequest struct {
	Email           string `json:"email"`
	NewPassword     string `json:"new_password"`
	ConfirmPassword string `json:"confirm_password"`
	Session         string `json:"session"`
	FirstName       string `json:"first_name"`
	LastName        string `json:"last_name"`
}

// UpdateOnboardingRequest holds the input for updating onboarding completion.
type UpdateOnboardingRequest struct {
	HasCompletedOnboarding bool `json:"has_completed_onboarding"`
}

// Login authenticates a user via Cognito and returns tokens. If the
// user signed up through the Cognito Hosted UI and has no local
// account yet, one is created automatically from the ID token claims.
func (s *AuthService) Login(
	ctx context.Context, req LoginRequest,
) (*LoginResponse, error) {
	if req.Email == "" {
		return nil, fmt.Errorf("email is required")
	}
	if req.Password == "" {
		return nil, fmt.Errorf("password is required")
	}

	slog.Info("user login attempt", "email", req.Email)

	result, err := s.cognito.Login(ctx, req.Email, req.Password)
	if err != nil {
		return nil, fmt.Errorf("cognito login: %w", err)
	}

	// Admin-created accounts in FORCE_CHANGE_PASSWORD state get a
	// challenge instead of tokens; pass the session back to the
	// client so it can complete the flow via /auth/complete-new-password.
	// Skip JIT account creation here: there is no ID token yet, and
	// EnsureAccountFromIDToken runs once the challenge is completed.
	if result.ChallengeName == ChallengeNewPasswordRequired {
		slog.Info("user login requires new password", "email", req.Email)
		return &LoginResponse{
			Challenge: ChallengeNewPasswordRequired,
			Session:   result.Session,
			Email:     req.Email,
		}, nil
	}

	s.EnsureAccountFromIDToken(ctx, result.IDToken)

	slog.Info("user login succeeded", "email", req.Email)

	return &LoginResponse{
		AccessToken:  result.AccessToken,
		IDToken:      result.IDToken,
		RefreshToken: result.RefreshToken,
		ExpiresIn:    result.ExpiresIn,
	}, nil
}

// CompleteNewPassword finishes the NEW_PASSWORD_REQUIRED challenge
// returned by Login for users in FORCE_CHANGE_PASSWORD state. On
// success it returns the same shape as Login (tokens) and JIT-creates
// the local account from the freshly issued ID token claims.
func (s *AuthService) CompleteNewPassword(
	ctx context.Context, req CompleteNewPasswordRequest,
) (*LoginResponse, error) {
	email := strings.TrimSpace(req.Email)
	if email == "" {
		return nil, fmt.Errorf("email is required")
	}
	if req.Session == "" {
		return nil, fmt.Errorf("session is required")
	}
	if len(req.NewPassword) < minForgotPasswordLen {
		return nil, ErrPasswordTooShort
	}
	if req.NewPassword != req.ConfirmPassword {
		return nil, ErrPasswordMismatch
	}

	slog.Info("complete new password attempt", "email", email)

	result, err := s.cognito.CompleteNewPassword(
		ctx,
		email, req.NewPassword, req.Session,
		req.FirstName, req.LastName,
	)
	if err != nil {
		return nil, fmt.Errorf("cognito complete new password: %w", err)
	}

	s.EnsureAccountFromIDToken(ctx, result.IDToken)

	slog.Info("complete new password succeeded", "email", email)

	return &LoginResponse{
		AccessToken:  result.AccessToken,
		IDToken:      result.IDToken,
		RefreshToken: result.RefreshToken,
		ExpiresIn:    result.ExpiresIn,
	}, nil
}

// Logout globally signs the user out by invalidating all tokens.
func (s *AuthService) Logout(
	ctx context.Context, accessToken string,
) error {
	if accessToken == "" {
		return fmt.Errorf("access token is required")
	}

	slog.Info("user logout attempt")

	if err := s.cognito.Logout(ctx, accessToken); err != nil {
		return fmt.Errorf("cognito logout: %w", err)
	}

	slog.Info("user logout succeeded")
	return nil
}

// ChangePasswordRequest is the body for POST /auth/change-password.
// The user is identified by the AccessToken extracted from the request
// (cookie or Authorization header) — there's no user_id on the wire.
type ChangePasswordRequest struct {
	OldPassword     string `json:"old_password"`
	NewPassword     string `json:"new_password"`
	ConfirmPassword string `json:"confirm_password"`
}

// ChangePasswordWithToken validates the request body and delegates to
// Cognito ChangePassword. Cognito itself enforces the password policy on
// the new value (length, character classes, history), so we only do
// minimum-sanity checks here:
//   - all three fields present
//   - new == confirm
//   - new ≠ old (cheap guard so we don't waste a Cognito round-trip
//     on the obvious mistake)
//
// The handler maps typed errors (ErrPasswordTooShort, ErrPasswordMismatch)
// to HTTP 400 VALIDATION_ERROR. Cognito's own errors (wrong old password,
// policy violation, etc.) fall through to the existing classifier.
func (s *AuthService) ChangePasswordWithToken(
	ctx context.Context, accessToken string, req ChangePasswordRequest,
) error {
	if accessToken == "" {
		return fmt.Errorf("access token is required")
	}
	if req.OldPassword == "" {
		return fmt.Errorf("old password is required")
	}
	// Re-use the same min-length sentinel the forgot-password flow uses
	// (`minForgotPasswordLen`) so the handler maps the response
	// identically across both surfaces.
	if len(req.NewPassword) < minForgotPasswordLen {
		return ErrPasswordTooShort
	}
	if req.NewPassword != req.ConfirmPassword {
		return ErrPasswordMismatch
	}
	if req.NewPassword == req.OldPassword {
		return fmt.Errorf("new password must be different from the current one")
	}

	slog.Info("user change password attempt")

	if err := s.cognito.ChangePassword(
		ctx, accessToken, req.OldPassword, req.NewPassword,
	); err != nil {
		return fmt.Errorf("cognito change password: %w", err)
	}

	slog.Info("user change password succeeded")
	return nil
}

// UpdateOnboardingStatus sets onboarding completion flag for the account.
func (s *AuthService) UpdateOnboardingStatus(
	ctx context.Context,
	accountID string,
	req UpdateOnboardingRequest,
) error {
	if accountID == "" {
		return fmt.Errorf("account id is required")
	}
	if err := s.accounts.SetHasCompletedOnboarding(
		ctx, accountID, req.HasCompletedOnboarding,
	); err != nil {
		return fmt.Errorf("updating onboarding status: %w", err)
	}
	return nil
}

// idTokenClaims holds the subset of Cognito ID token claims used for
// just-in-time local account creation.
type idTokenClaims struct {
	Sub        string `json:"sub"`
	Email      string `json:"email"`
	GivenName  string `json:"given_name"`
	FamilyName string `json:"family_name"`
}

// EnsureAccountFromIDToken parses the Cognito ID token (without
// cryptographic verification — the token came directly from the
// Cognito API) and creates a local account if one does not already
// exist for the user's cognito sub.
func (s *AuthService) EnsureAccountFromIDToken(
	ctx context.Context, rawIDToken string,
) {
	claims, err := parseIDTokenClaims(rawIDToken)
	if err != nil {
		slog.Warn("could not parse id token for JIT provisioning",
			"error", err,
		)
		return
	}

	existing, err := s.accounts.GetByCognitoSub(ctx, claims.Sub)
	if err != nil {
		slog.Error("JIT account lookup failed",
			"cognito_sub", claims.Sub, "error", err,
		)
		return
	}
	if existing != nil {
		return
	}

	acct, err := s.accounts.Create(ctx, repository.CreateAccountParams{
		FirstName:  claims.GivenName,
		LastName:   claims.FamilyName,
		Email:      claims.Email,
		CognitoSub: claims.Sub,
	})
	if err != nil {
		slog.Error("JIT account creation failed",
			"cognito_sub", claims.Sub,
			"email", claims.Email,
			"error", err,
		)
		return
	}

	slog.Info("JIT account created on login",
		"account_id", acct.ID,
		"email", claims.Email,
		"cognito_sub", claims.Sub,
	)

	s.backfillInvites(claims.Email, acct.ID)

	if s.provisioner != nil {
		slog.Info("Triggering post-signup provisioning",
			"account_id", acct.ID,
		)
		go s.provisioner.ProvisionNewAccount(context.Background(), acct.ID)
	}
}

// parseIDTokenClaims extracts claims from the JWT payload without
// signature verification. This is safe because the token was received
// directly from the Cognito API in a server-to-server call.
func parseIDTokenClaims(rawToken string) (*idTokenClaims, error) {
	parts := strings.SplitN(rawToken, ".", 3)
	if len(parts) != 3 {
		return nil, fmt.Errorf("malformed jwt: expected 3 segments")
	}

	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return nil, fmt.Errorf("decoding jwt payload: %w", err)
	}

	var claims idTokenClaims
	if err := json.Unmarshal(payload, &claims); err != nil {
		return nil, fmt.Errorf("unmarshalling jwt claims: %w", err)
	}

	if claims.Sub == "" {
		return nil, fmt.Errorf("id token missing sub claim")
	}
	return &claims, nil
}

// backfillInvites updates pending datapack_invites that match the
// user's email with their newly created user ID. Best-effort: errors
// are logged but do not block the signup/login flow.
func (s *AuthService) backfillInvites(email, userID string) {
	if s.inviteBackfiller == nil {
		return
	}
	n, err := s.inviteBackfiller.BackfillRecipientUserID(email, userID)
	if err != nil {
		slog.Warn("invite backfill failed (non-fatal)",
			"email", email, "error", err,
		)
		return
	}
	if n > 0 {
		slog.Info("backfilled pending invites for new user",
			"email", email, "user_id", userID, "count", n,
		)
	}
}

func validateSignupRequest(req SignupRequest) error {
	if req.Email == "" {
		return fmt.Errorf("email is required")
	}
	if req.Password == "" {
		return fmt.Errorf("password is required")
	}
	if req.FirstName == "" {
		return fmt.Errorf("first name is required")
	}
	if req.LastName == "" {
		return fmt.Errorf("last name is required")
	}
	return nil
}
