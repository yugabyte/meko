package service

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/mail"
	"strings"
	"unicode/utf8"

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
	// GetUserStatus returns the Cognito account status (e.g.
	// FORCE_CHANGE_PASSWORD) for the given email.
	GetUserStatus(ctx context.Context, email string) (auth.UserStatus, error)
	// ResendInvite regenerates the temporary password and re-sends the Cognito
	// invitation email. Used to rescue an admin-created user whose temp password
	// expired so they get a fresh one and can complete first login.
	ResendInvite(ctx context.Context, email string) error
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
	// UpdateUserName syncs the given_name/family_name a user confirms during
	// onboarding back to Cognito (addressed by pool sub) so the user pool stays
	// consistent with the local account.
	UpdateUserName(
		ctx context.Context, sub, firstName, lastName string,
	) error
	// UpdateEmailAttribute sets the signed-in user's email (by access token)
	// and triggers Cognito to email a verification code to the new address.
	UpdateEmailAttribute(ctx context.Context, accessToken, newEmail string) error
	// SendEmailAttributeCode (re)sends the email verification code to the
	// signed-in user's current email attribute.
	SendEmailAttributeCode(ctx context.Context, accessToken string) error
	// VerifyEmailAttribute confirms the signed-in user's email with the emailed
	// code, flipping Cognito's email_verified to true.
	VerifyEmailAttribute(ctx context.Context, accessToken, code string) error
}

// ErrForgotPasswordNoAccount is returned when Cognito has no user for this email
// on the forgot-password flow (mapped to HTTP 404 in the auth handler).
var ErrForgotPasswordNoAccount = errors.New("no account exists for this email address")

// ErrInvalidProfileName is returned by UpdateProfile when first/last name
// is blank after trimming. Cognito mandates both, so we reject empties.
var ErrInvalidProfileName = errors.New("first name and last name are required")

// Email-collection (post-signup, federated-no-email) flow errors.
var (
	// ErrInvalidEmail is returned when the submitted email is empty/malformed
	// or is itself a synthesized placeholder address.
	ErrInvalidEmail = errors.New("a valid email address is required")
	// ErrEmailTaken is returned when another account already owns the email.
	ErrEmailTaken = errors.New("that email address is already in use")
)

// placeholderEmailHostPrefix / placeholderEmailHostSuffix bound the reserved,
// non-deliverable addresses the inbound-federation Lambda synthesizes when a
// federated IdP (LinkedIn / GitHub / ...) doesn't supply an email. They take
// the shape "<username>@no-reply.<provider>.meko.invalid", so the provider
// segment is variable — we match on the fixed host prefix + suffix rather than
// a single domain. MUST stay in sync with the address built in
// cognito-lambdas/cognito-inbound-federation/index.mjs. Accounts carrying such an
// address are unverified and gated until they supply + verify a real one.
const (
	placeholderEmailHostPrefix = "no-reply."
	placeholderEmailHostSuffix = ".meko.invalid"
)

// isPlaceholderEmail reports whether e is a synthesized placeholder address
// (case-insensitive match on the reserved host prefix + suffix).
func isPlaceholderEmail(e string) bool {
	e = strings.ToLower(strings.TrimSpace(e))
	at := strings.LastIndex(e, "@")
	if at < 0 {
		return false
	}
	host := e[at+1:]
	return strings.HasPrefix(host, placeholderEmailHostPrefix) &&
		strings.HasSuffix(host, placeholderEmailHostSuffix)
}

// isValidEmailFormat reports whether e is a single, bare RFC 5322 address with
// a dotted domain. It's a guard (the BE/Cognito are authoritative), not a
// deliverability check. net/mail.ParseAddress also accepts display-name forms
// ("Name <a@b>") and dotless domains ("a@b"), so we additionally require the
// parsed address to equal the trimmed input and the domain to contain a dot.
func isValidEmailFormat(e string) bool {
	e = strings.TrimSpace(e)
	addr, err := mail.ParseAddress(e)
	if err != nil || addr.Address != e {
		return false
	}
	at := strings.LastIndex(addr.Address, "@")
	return at >= 0 && strings.Contains(addr.Address[at+1:], ".")
}

// ErrInvitePasswordExpired is returned by Login when an admin-created user's
// temporary password has expired. Login rescues the account by re-sending a
// fresh temporary password and returns this so the handler can answer 401 with
// a code the UI renders as "your temp password expired — we emailed a new one".
var ErrInvitePasswordExpired = errors.New("invite temporary password expired; a new one was sent")

// Password validation errors for the forgot-password / set-password flows.
// They are returned as typed sentinels so the handler can map them to
// HTTP 400 VALIDATION_ERROR explicitly instead of falling through to a
// generic 500.
var (
	ErrPasswordTooShort         = errors.New("password must be at least 8 characters long")
	ErrPasswordTooLong          = errors.New("password must be 256 characters or fewer")
	ErrPasswordMissingLowercase = errors.New("password must include at least one lowercase letter")
	ErrPasswordMissingUppercase = errors.New("password must include at least one uppercase letter")
	ErrPasswordMissingNumber    = errors.New("password must include at least one number")
	ErrPasswordMissingSymbol    = errors.New("password must include at least one special character")
	ErrPasswordMismatch         = errors.New("password and confirmation do not match")
)

const (
	minPasswordLength = 8
	maxPasswordLength = 256
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
	GetByID(ctx context.Context, id string) (*models.Account, error)
	VerifyEmail(ctx context.Context, email string) error
	SetHasCompletedOnboarding(ctx context.Context, accountID string, completed bool) error
	UpdateProfile(ctx context.Context, userID, firstName, lastName string) error
	// EmailInUse reports whether an account other than excludeUserID already
	// owns the email. Read-only pre-check used to avoid sending an OTP for an
	// address that's already taken.
	EmailInUse(ctx context.Context, email, excludeUserID string) (bool, error)
	// UpdateEmail replaces a user's email and resets it to unverified (the new
	// address still needs OTP confirmation). Returns ErrEmailTaken if another
	// account already owns the address.
	UpdateEmail(ctx context.Context, userID, email string) error
	// MarkEmailVerified stamps email_verified_at=NOW() for the given user.
	MarkEmailVerified(ctx context.Context, userID string) error
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

// ForgotPasswordSendCode triggers Cognito ForgotPassword (sends a reset code to
// the user's email). Note: Cognito rejects this for accounts still in
// FORCE_CHANGE_PASSWORD; those users are rescued at login time (see Login),
// which flips them to CONFIRMED so this standard flow works.
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
	if err := validateCognitoPasswordPolicy(newPw); err != nil {
		return err
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
		// On expired temp password, send a new temp password and let the caller know.
		if errors.Is(err, auth.ErrTempPasswordExpired) {
			if s.rescueExpiredInvite(ctx, req.Email) {
				return nil, ErrInvitePasswordExpired
			}
			// Fallback to not authorized if rescue fails.
			return nil, auth.ErrNotAuthorized
		}
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

	if _, err := s.EnsureAccountFromIDToken(ctx, result.IDToken); err != nil {
		return nil, fmt.Errorf("provisioning local account: %w", err)
	}

	slog.Info("user login succeeded", "email", req.Email)

	return &LoginResponse{
		AccessToken:  result.AccessToken,
		IDToken:      result.IDToken,
		RefreshToken: result.RefreshToken,
		ExpiresIn:    result.ExpiresIn,
	}, nil
}

// rescueExpiredInvite flips an admin-created user whose temporary password
// expired from FORCE_CHANGE_PASSWORD to CONFIRMED by setting a random permanent
// password the user never learns. This unblocks the standard ForgotPassword
// flow (Cognito rejects reset while the account is FORCE_CHANGE_PASSWORD). It is
// intentionally best-effort and self-contained: it confirms the account really
// is in FORCE_CHANGE_PASSWORD before changing anything, and returns false (with
// a log) on any problem so the caller falls back to a normal 401 rather than
// leaking state or surfacing an error. Returns true only if the account was
// rescued.
func (s *AuthService) rescueExpiredInvite(ctx context.Context, email string) bool {
	status, err := s.cognito.GetUserStatus(ctx, email)
	if err != nil {
		slog.Warn("expired-invite rescue: status lookup failed", "email", email, "error", err)
		return false
	}
	if status != auth.UserStatusForceChangePassword {
		slog.Warn("expired-invite rescue: unexpected status, skipping",
			"email", email, "status", string(status))
		return false
	}
	if err := s.cognito.ResendInvite(ctx, email); err != nil {
		slog.Error("expired-invite rescue: resend invite failed", "email", email, "error", err)
		return false
	}
	slog.Info("expired-invite rescue: fresh temporary password sent, user can complete first login",
		"email", email)
	return true
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
	if err := validateCognitoPasswordPolicy(req.NewPassword); err != nil {
		return nil, err
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

	if _, err := s.EnsureAccountFromIDToken(ctx, result.IDToken); err != nil {
		return nil, fmt.Errorf("provisioning local account: %w", err)
	}

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
// Cognito ChangePassword. We mirror the user-pool password policy here
// so browser-less clients (curl/scripts) receive immediate, specific
// validation errors before the Cognito round-trip:
//   - all three fields present
//   - new password matches the Cognito policy shown in the UI
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
	if err := validateCognitoPasswordPolicy(req.NewPassword); err != nil {
		return err
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
	// EmailVerified is Cognito's standard email_verified claim (set for
	// native users after they confirm their email, and for federated users
	// via the IdP attribute mapping). Decoded as `any` because the value can
	// arrive as a JSON boolean (true) or a quoted string ("true") depending on
	// how the attribute is mapped; emailIsVerified compares against both shapes.
	EmailVerified any `json:"email_verified"`
	// Identities is present (non-empty) only for users federated from an
	// external IdP (Google, LinkedIn, ...). Its presence is a reliable signal
	// that the email was vouched for by that provider, even when the IdP
	// attribute mapping doesn't forward email_verified.
	Identities []cognitoIdentity `json:"identities"`
}

// cognitoIdentity is one entry of the Cognito ID token "identities" claim,
// describing the external IdP a federated user signed in with.
type cognitoIdentity struct {
	ProviderName string `json:"providerName"`
}

// emailIsVerified reports whether the account's email should be treated as
// verified. We trust Cognito's standard email_verified claim, which for
// federated users (Google / LinkedIn) is populated from the IdP attribute
// mapping (e.g. Google's own email_verified). This relies on that mapping
// being configured in the user pool; the debug logging in
// EnsureAccountFromIDToken surfaces the raw claim so drift is visible.
//
// The claim normally decodes to a JSON boolean, but an IdP-mapped string
// attribute can surface it as "true"; we accept either shape. Anything else
// (false, "false", null, absent, a number) is treated as not verified — this
// also guards against the nOAuth "string-false-is-truthy" footgun.
//
// A synthesized placeholder address (LinkedIn signups with no IdP email) is
// never treated as verified, regardless of the claim: doing so would let a
// federated user past the post-signup email-collection gate without ever
// proving a real address.
func (c idTokenClaims) emailIsVerified() bool {
	if isPlaceholderEmail(c.Email) {
		return false
	}
	return c.EmailVerified == true || c.EmailVerified == "true" || c.EmailVerified == "True"
}

// providerNames returns the external IdP names from the Cognito "identities"
// claim (empty for native users). Used only for debug logging.
func (c idTokenClaims) providerNames() []string {
	names := make([]string, 0, len(c.Identities))
	for _, id := range c.Identities {
		names = append(names, id.ProviderName)
	}
	return names
}

// federatedNamePlaceholder is the value the Cognito inbound-federation Lambda
// injects for given_name/family_name that the IdP didn't supply, so Cognito's
// required-attribute check passes (see cognito-lambdas/cognito-inbound-federation/).
const federatedNamePlaceholder = "-"

// normalizeFederatedName maps that placeholder back to an empty string for the
// local account. The app reads names from the local DB and the onboarding
// name-confirmation modal triggers on an empty first/last name, so storing ""
// (not "-") is what prompts the user to enter their real name. A genuine
// single-character name is implausible here and not worth special-casing.
func normalizeFederatedName(v string) string {
	if strings.TrimSpace(v) == federatedNamePlaceholder {
		return ""
	}
	return v
}

// EnsureAccountFromIDToken parses the Cognito ID token (without
// cryptographic verification — the token came directly from the
// Cognito API) and creates a local account if one does not already
// exist for the user's cognito sub.
//
// The bool is true only when a brand-new local account was created in this
// call; the social callbacks use it to show the one-time name-confirmation
// step on first signup only — an existing user signing in again returns
// (false, nil). The error is non-nil only on a genuine failure (malformed
// token, DB lookup/insert error); callers MUST surface it (fail the
// login / redirect to sign-in) rather than proceeding, otherwise the user
// gets auth cookies but no local account and every subsequent API call 401s.
func (s *AuthService) EnsureAccountFromIDToken(
	ctx context.Context, rawIDToken string,
) (bool, error) {
	claims, err := parseIDTokenClaims(rawIDToken)
	if err != nil {
		slog.Warn("could not parse id token for JIT provisioning",
			"error", err,
		)
		return false, fmt.Errorf("parse id token: %w", err)
	}

	// Debug: surface the raw verification-related claims so we can confirm
	// whether Cognito forwards the IdP's email_verified (esp. for Google,
	// where it depends on the user-pool attribute mapping). If
	// email_verified is false here for a federated user, the mapping is the
	// culprit, not the app.
	slog.Info("JIT id token claims",
		"cognito_sub", claims.Sub,
		"email", claims.Email,
		"email_verified", claims.EmailVerified,
		"identities", claims.providerNames(),
		"email_is_verified", claims.emailIsVerified(),
	)

	existing, err := s.accounts.GetByCognitoSub(ctx, claims.Sub)
	if err != nil {
		slog.Error("JIT account lookup failed",
			"cognito_sub", claims.Sub, "error", err,
		)
		return false, fmt.Errorf("account lookup: %w", err)
	}
	if existing != nil {
		return false, nil
	}

	acct, err := s.accounts.Create(ctx, repository.CreateAccountParams{
		FirstName:     normalizeFederatedName(claims.GivenName),
		LastName:      normalizeFederatedName(claims.FamilyName),
		Email:         claims.Email,
		CognitoSub:    claims.Sub,
		EmailVerified: claims.emailIsVerified(),
	})
	if err != nil {
		slog.Error("JIT account creation failed",
			"cognito_sub", claims.Sub,
			"email", claims.Email,
			"error", err,
		)
		return false, fmt.Errorf("account creation: %w", err)
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

	return true, nil
}

// UpdateProfileRequest holds the input for updating a user's display name.
type UpdateProfileRequest struct {
	FirstName string `json:"first_name"`
	LastName  string `json:"last_name"`
}

// UpdateProfile updates the local account's first/last name and syncs it back
// to Cognito so the user pool stays consistent with the local account.
//
// The local DB is the source of truth the app reads from (see Me), so the local
// write is what matters for the UI. The Cognito write exists for federated
// (Google) users: Google never sends family_name, so the inbound-federation
// Lambda seeds a "-" placeholder in the pool; pushing the confirmed name here
// overwrites that placeholder, and the Lambda's idempotent no-op on later
// sign-ins preserves it. The Cognito sync is best-effort — a failure there must
// not block onboarding, since the app reads names from the local DB.
func (s *AuthService) UpdateProfile(
	ctx context.Context,
	accountID string,
	req UpdateProfileRequest,
) error {
	if accountID == "" {
		return fmt.Errorf("account id is required")
	}
	first := strings.TrimSpace(req.FirstName)
	last := strings.TrimSpace(req.LastName)
	if first == "" || last == "" {
		return ErrInvalidProfileName
	}
	if err := s.accounts.UpdateProfile(ctx, accountID, first, last); err != nil {
		return fmt.Errorf("updating profile: %w", err)
	}

	acct, err := s.accounts.GetByID(ctx, accountID)
	if err != nil {
		slog.Warn("update profile: could not load account to sync name to cognito",
			"account_id", accountID, "error", err)
		return nil
	}
	if acct == nil || strings.TrimSpace(acct.CognitoSub) == "" {
		return nil
	}
	if err := s.cognito.UpdateUserName(ctx, acct.CognitoSub, first, last); err != nil {
		slog.Warn("update profile: failed to sync name to cognito",
			"account_id", accountID, "error", err)
	}
	return nil
}

// StartEmailChangeRequest is the body for POST /auth/me/email/start.
type StartEmailChangeRequest struct {
	Email string `json:"email"`
}

// StartEmailChange begins the post-signup email-collection flow for a federated
// user (e.g. LinkedIn) who arrived without an email and carries a placeholder.
// It validates the address, writes it to Cognito (which emails an OTP to the
// new address), and records it locally as unverified. The account stays gated
// until VerifyEmailChange confirms the code.
//
// Cognito is updated first (it's the authoritative store + the OTP source);
// only on success do we persist locally. Both the access token (identity proof
// for the Cognito write) and the account ID (for the local write) are required.
func (s *AuthService) StartEmailChange(
	ctx context.Context, accountID, accessToken string, req StartEmailChangeRequest,
) error {
	if accountID == "" {
		return fmt.Errorf("account id is required")
	}
	if accessToken == "" {
		return fmt.Errorf("access token is required")
	}
	email := strings.TrimSpace(req.Email)
	if email == "" || !isValidEmailFormat(email) || isPlaceholderEmail(email) {
		return ErrInvalidEmail
	}

	// Pre-check BEFORE the Cognito write: if another local account already owns
	// this address, bail out now so we never fire an OTP for an email that's
	// already taken. Cognito's AliasExistsException only catches addresses
	// registered as sign-in aliases, so a collision with e.g. a federated
	// account whose email isn't an alias would otherwise slip past the Cognito
	// write (sending a code) and only fail later at the local UpdateEmail.
	if inUse, err := s.accounts.EmailInUse(ctx, email, accountID); err != nil {
		return fmt.Errorf("checking email availability: %w", err)
	} else if inUse {
		return ErrEmailTaken
	}

	// Cognito write: sets the email attribute (unverified) and sends the OTP.
	// AliasExistsException (the address is already a sign-in alias on another
	// account) is a second guard for the alias case.
	if err := s.cognito.UpdateEmailAttribute(ctx, accessToken, email); err != nil {
		if errors.Is(err, auth.ErrEmailExists) {
			return ErrEmailTaken
		}
		return fmt.Errorf("updating cognito email: %w", err)
	}

	// Persist locally as unverified. A unique-violation here means another
	// local account already owns the address.
	if err := s.accounts.UpdateEmail(ctx, accountID, email); err != nil {
		if errors.Is(err, repository.ErrEmailTaken) {
			return ErrEmailTaken
		}
		return fmt.Errorf("updating local email: %w", err)
	}

	slog.Info("email change started; verification code sent",
		"account_id", accountID, "email", email)
	return nil
}

// ResendEmailChangeCode re-sends the OTP to the user's pending (current) email
// attribute in Cognito.
func (s *AuthService) ResendEmailChangeCode(ctx context.Context, accessToken string) error {
	if accessToken == "" {
		return fmt.Errorf("access token is required")
	}
	return s.cognito.SendEmailAttributeCode(ctx, accessToken)
}

// VerifyEmailChangeRequest is the body for POST /auth/me/email/verify.
type VerifyEmailChangeRequest struct {
	Code string `json:"code"`
}

// VerifyEmailChange confirms the pending email with the emailed OTP: it verifies
// the code against Cognito (flipping Cognito's email_verified) and, on success,
// stamps email_verified_at locally — which clears the onboarding email gate.
func (s *AuthService) VerifyEmailChange(
	ctx context.Context, accountID, accessToken string, req VerifyEmailChangeRequest,
) error {
	if accountID == "" {
		return fmt.Errorf("account id is required")
	}
	if accessToken == "" {
		return fmt.Errorf("access token is required")
	}
	code := strings.TrimSpace(req.Code)
	if code == "" {
		return fmt.Errorf("verification code is required")
	}
	if err := s.cognito.VerifyEmailAttribute(ctx, accessToken, code); err != nil {
		// Bad/expired codes are surfaced as typed auth sentinels the handler
		// maps to 400; wrap everything else.
		return err
	}
	if err := s.accounts.MarkEmailVerified(ctx, accountID); err != nil {
		return fmt.Errorf("marking email verified: %w", err)
	}
	slog.Info("email change verified", "account_id", accountID)
	return nil
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
	if err := validateCognitoPasswordPolicy(req.Password); err != nil {
		return err
	}
	if req.FirstName == "" {
		return fmt.Errorf("first name is required")
	}
	if req.LastName == "" {
		return fmt.Errorf("last name is required")
	}
	return nil
}

func validateCognitoPasswordPolicy(password string) error {
	length := utf8.RuneCountInString(password)
	if length < minPasswordLength {
		return ErrPasswordTooShort
	}
	if length > maxPasswordLength {
		return ErrPasswordTooLong
	}

	var hasLower, hasUpper, hasNumber, hasSymbol bool
	for _, r := range password {
		switch {
		case r >= 'a' && r <= 'z':
			hasLower = true
		case r >= 'A' && r <= 'Z':
			hasUpper = true
		case r >= '0' && r <= '9':
			hasNumber = true
		case strings.ContainsRune("!@#$%^&*()_+-=[]{}|;':\",./<>?`~\\", r):
			hasSymbol = true
		}
	}

	switch {
	case !hasLower:
		return ErrPasswordMissingLowercase
	case !hasUpper:
		return ErrPasswordMissingUppercase
	case !hasNumber:
		return ErrPasswordMissingNumber
	case !hasSymbol:
		return ErrPasswordMissingSymbol
	default:
		return nil
	}
}
