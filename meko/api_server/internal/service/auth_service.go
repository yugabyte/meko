package service

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math/big"
	"net/mail"
	"strings"
	"time"
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
	// ResetTempPassword sets a new temporary password on an admin-created user
	// without sending any Cognito email. AuthService emails the password itself
	// with the password-setup template (see sendTempPassword).
	ResetTempPassword(ctx context.Context, email, temporaryPassword string) error
	// SetPermanentPassword sets a permanent password, which moves a
	// FORCE_CHANGE_PASSWORD user to CONFIRMED. Called only after the user
	// presents a valid Meko-issued reset code.
	SetPermanentPassword(ctx context.Context, email, password string) error
	// ResendInvite regenerates the temporary password and re-sends the Cognito
	// invitation email (the pool's "Welcome to Meko" template). The
	// expired-invite rescue falls back to it only when no SES sender is
	// configured, so deployments without SES keep a working rescue.
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

// ErrInvalidProfileName is returned by UpdateProfile when first/last name
// is blank after trimming. Cognito mandates both, so we reject empties.
var ErrInvalidProfileName = errors.New("first name and last name are required")

// Email-project (post-signup, federated-no-email) flow errors.
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
	cognito              CognitoProvider
	accounts             AccountStore
	provisioner          Provisioner
	inviteBackfiller     InviteBackfiller
	passwordSetupEmailer PasswordSetupEmailSender
	resetStore           PasswordResetStore
	issuerOwnsEmail      bool
	// now is time.Now; tests replace it.
	now func() time.Time
}

// PasswordSetupReason says why Meko sent a password-setup email: a reset code
// for Forgot password, or a new temporary password for an expired invite. The
// email wording depends on it.
type PasswordSetupReason string

const (
	// PasswordSetupForgotPassword: the user asked to reset their password
	// before finishing first sign-in. The email carries a Meko reset code.
	PasswordSetupForgotPassword PasswordSetupReason = "forgot_password"
	// PasswordSetupInviteExpired: the user signed in with an invite temporary
	// password that had expired. The email carries a new temporary password.
	PasswordSetupInviteExpired PasswordSetupReason = "invite_expired"
)

// PasswordSetupEmailParams carries what the password-setup email needs. It is
// a separate email from the invite: it never says "welcome" or names a
// datapack. Exactly one of Code (with CodeTTL) and TempPassword is set,
// matching Reason.
type PasswordSetupEmailParams struct {
	RecipientEmail string
	Code           string
	TempPassword   string
	AppURL         string
	Reason         PasswordSetupReason
	CodeTTL        time.Duration
}

// PasswordResetStore persists forgot-password requests for the per-email
// limit and the reset codes Meko issues itself. The email is passed
// lowercased. Implemented by repository.PasswordResetRepo.
type PasswordResetStore interface {
	CountResetRequestsSince(ctx context.Context, email string, since time.Time) (int, error)
	RecordResetRequest(ctx context.Context, email string) (string, error)
	AttachResetCode(ctx context.Context, email, id, codeHash string, expiresAt time.Time) error
	LatestResetRequest(ctx context.Context, email string) (*models.PasswordResetRequest, error)
	// ReserveResetAttempt atomically uses one of the code's maxAttempts
	// guesses. It reports false when the code is used, expired or out of
	// guesses, so concurrent guesses can't exceed the limit.
	ReserveResetAttempt(ctx context.Context, email, id string, maxAttempts int) (bool, error)
	ClaimResetCode(ctx context.Context, email, id string) (bool, error)
	UnclaimResetCode(ctx context.Context, email, id string) error
	// DeleteResetRequest drops a recorded request that was rejected or
	// abandoned so it neither counts toward the limit nor hides a live code.
	DeleteResetRequest(ctx context.Context, email, id string) error
}

// Forgot-password limits, following the OWASP Forgot Password Cheat Sheet:
// a per-account request limit, and a short-lived, single-use,
// attempt-limited code that changes nothing until it is presented.
const (
	// maxResetRequestsPerWindow requests per email in resetRequestWindow,
	// counted across both the Cognito and the Meko-code paths.
	maxResetRequestsPerWindow = 5
	resetRequestWindow        = time.Hour
	// resetCodeTTL matches Cognito's own reset-code lifetime.
	resetCodeTTL = time.Hour
	// maxResetCodeAttempts guesses per code, right or wrong, reserved
	// atomically in the store.
	maxResetCodeAttempts = 5
	// resetCodeDigits gives 10^8 codes; with maxResetCodeAttempts guesses per
	// code a guess succeeds with probability 5e-8.
	resetCodeDigits = 8
)

// PasswordSetupEmailSender sends the password-setup email. Only router.go
// imports both service and email to wire it.
type PasswordSetupEmailSender interface {
	SendPasswordSetupEmail(ctx context.Context, p PasswordSetupEmailParams) error
}

// errPasswordSetupEmailDisabled is returned by sendTempPassword when no
// PasswordSetupEmailSender is configured, before any password is changed.
var errPasswordSetupEmailDisabled = errors.New("password-setup email sender not configured")

// ErrPasswordResetUnavailable is returned when the reset store fails, and when
// the expired-invite rescue emailed a temporary password that Cognito then
// refused. The underlying error is logged, not wrapped, so SES and AWS text
// never reaches the client. The handler maps it to a static 503.
var ErrPasswordResetUnavailable = errors.New("password reset is temporarily unavailable")

// ErrPasswordResetRateLimited is returned when an email has made
// maxResetRequestsPerWindow forgot-password requests in resetRequestWindow.
// The handler maps it to a static 429.
var ErrPasswordResetRateLimited = errors.New("too many password reset requests")

// AuthServiceOption configures optional AuthService dependencies.
type AuthServiceOption func(*AuthService)

// WithPasswordResetStore sets the store for the per-email forgot-password
// limit and Meko-issued reset codes. Without it there is no per-email limit,
// and an invitee who never finished first sign-in can't reset by email.
func WithPasswordResetStore(r PasswordResetStore) AuthServiceOption {
	return func(s *AuthService) { s.resetStore = r }
}

// WithPasswordSetupEmailer sets the sender for the password-setup email: a
// Meko reset code for Forgot password, or a new temporary password for the
// expired-invite rescue. Without it, an invitee who never finished first
// sign-in gets no email from Forgot password, and the rescue falls back to
// Cognito's own invitation resend.
func WithPasswordSetupEmailer(e PasswordSetupEmailSender) AuthServiceOption {
	return func(s *AuthService) { s.passwordSetupEmailer = e }
}

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

// WithIssuerOwnedEmail accepts the email in a verified ID token as verified.
//
// For the OIDC path only, and it is the difference between the two providers
// rather than a relaxation: an OIDC issuer IS the user store, the ID token was
// verified against its JWKS before this runs, and there is no endpoint here to
// finish verification with — /auth/me/email/* is Cognito's, registered only on
// that path. Without this, an issuer that omits email_verified (Entra never
// sends it) or reports it false (Keycloak, for any user it did not mail) leaves
// every account at email_verified=false, which puts the SPA's blocking
// onboarding modal in front of a flow that cannot complete.
func WithIssuerOwnedEmail() AuthServiceOption {
	return func(s *AuthService) { s.issuerOwnsEmail = true }
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
		now:      time.Now,
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

// ForgotPasswordSendCode starts a password reset. It answers the same way
// whether or not an account exists, and whether or not a code could be sent
// to it, so the response can't be used to find accounts (OWASP). Each email
// may make maxResetRequestsPerWindow requests per resetRequestWindow.
// Cognito rate limits still return 429; store failures still return 503.
//
// Cognito emails the reset code for a normal account. Cognito rejects
// ForgotPassword for an admin-created user who never finished first sign-in
// (FORCE_CHANGE_PASSWORD) with ErrPasswordResetNotAllowed, so that user would
// otherwise get no email (MEKO-887). For that user Meko issues and emails its
// own reset code; ConfirmForgotPasswordReset accepts it. Nothing about the
// account changes until the code is presented. Other Cognito errors
// (unconfirmed, disabled, invalid parameter, wrong client secret, outage)
// are logged and answered the same 200.
func (s *AuthService) ForgotPasswordSendCode(
	ctx context.Context, req ForgotPasswordStartRequest,
) error {
	email := strings.TrimSpace(req.Email)
	if email == "" {
		return fmt.Errorf("email is required")
	}
	key := strings.ToLower(email)

	requestID := ""
	if s.resetStore != nil {
		// Record first, then count including this request, so concurrent
		// requests for one email see each other's rows. Rejected or abandoned
		// requests delete their row so they don't count or hide a live code.
		var err error
		if requestID, err = s.resetStore.RecordResetRequest(ctx, key); err != nil {
			slog.Error("forgot password: recording reset request failed", "email", email, "error", err)
			return ErrPasswordResetUnavailable
		}
		n, err := s.resetStore.CountResetRequestsSince(ctx, key, s.now().Add(-resetRequestWindow))
		if err != nil {
			s.discardResetRequest(ctx, key, requestID)
			slog.Error("forgot password: reset request count failed", "email", email, "error", err)
			return ErrPasswordResetUnavailable
		}
		if n > maxResetRequestsPerWindow {
			s.discardResetRequest(ctx, key, requestID)
			slog.Warn("forgot password: per-email limit reached", "email", email, "requests", n)
			return ErrPasswordResetRateLimited
		}
	}

	err := s.cognito.ForgotPassword(ctx, email)
	if err == nil {
		return nil
	}
	if errors.Is(err, auth.ErrUserNotFound) {
		slog.Info("forgot password: no account for email", "email", email)
		return nil
	}
	if errors.Is(err, auth.ErrTooManyRequests) || errors.Is(err, auth.ErrLimitExceeded) {
		s.discardResetRequest(ctx, key, requestID)
		return fmt.Errorf("cognito forgot password: %w", err)
	}
	if !errors.Is(err, auth.ErrPasswordResetNotAllowed) {
		s.discardResetRequest(ctx, key, requestID)
		slog.Warn("forgot password: cognito error answered as uniform success",
			"email", email, "error", err)
		return nil
	}
	// From here on every outcome returns nil, so the response doesn't tell
	// an unactivated invitee apart from an unknown email.
	if s.passwordSetupEmailer == nil || s.resetStore == nil {
		s.discardResetRequest(ctx, key, requestID)
		slog.Warn("forgot password: account can't be reset and Meko reset codes are disabled (needs SES)",
			"email", email)
		return nil
	}
	status, statusErr := s.cognito.GetUserStatus(ctx, email)
	if statusErr != nil || status != auth.UserStatusForceChangePassword {
		s.discardResetRequest(ctx, key, requestID)
		slog.Warn("forgot password: account can't be reset and isn't an unactivated invite",
			"email", email, "status", string(status), "error", statusErr)
		return nil
	}
	if err := s.sendResetCode(ctx, email, key, requestID); err != nil {
		s.discardResetRequest(ctx, key, requestID)
		slog.Error("forgot password: Meko reset code not sent", "email", email, "error", err)
	}
	return nil
}

// discardResetRequest removes a recorded request that was rejected or never
// finished. Failures are logged; the caller's return value is unchanged.
func (s *AuthService) discardResetRequest(ctx context.Context, key, requestID string) {
	if s.resetStore == nil || requestID == "" {
		return
	}
	if err := s.resetStore.DeleteResetRequest(ctx, key, requestID); err != nil {
		slog.Warn("forgot password: discarding reset request failed",
			"email", key, "request_id", requestID, "error", err)
	}
}

// sendResetCode issues a Meko reset code on request requestID and emails it.
// The code is stored only as a hash. It is stored before the email goes out
// so it already works when the user enters it. The caller logs a failure and
// still answers 200, to keep the response uniform.
func (s *AuthService) sendResetCode(ctx context.Context, email, key, requestID string) error {
	code, err := genResetCode()
	if err != nil {
		return fmt.Errorf("generate reset code: %w", err)
	}
	expiresAt := s.now().Add(resetCodeTTL)
	if err := s.resetStore.AttachResetCode(ctx, key, requestID, hashResetCode(key, code), expiresAt); err != nil {
		return fmt.Errorf("store reset code: %w", err)
	}
	if err := s.passwordSetupEmailer.SendPasswordSetupEmail(ctx, PasswordSetupEmailParams{
		RecipientEmail: email,
		Code:           code,
		CodeTTL:        resetCodeTTL,
		Reason:         PasswordSetupForgotPassword,
	}); err != nil {
		return fmt.Errorf("send reset code email: %w", err)
	}
	slog.Info("forgot password: Meko reset code sent to unactivated account", "email", email)
	return nil
}

// genResetCode returns a uniformly random resetCodeDigits-digit code.
func genResetCode() (string, error) {
	limit := new(big.Int).Exp(big.NewInt(10), big.NewInt(resetCodeDigits), nil)
	n, err := rand.Int(rand.Reader, limit)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%0*d", resetCodeDigits, n), nil
}

// hashResetCode is what the store keeps instead of the code. The email is
// part of the input, so a code hash is only valid for its own account.
func hashResetCode(key, code string) string {
	sum := sha256.Sum256([]byte(key + ":" + code))
	return hex.EncodeToString(sum[:])
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
	if s.resetStore != nil {
		key := strings.ToLower(email)
		latest, err := s.resetStore.LatestResetRequest(ctx, key)
		if err != nil {
			slog.Error("forgot password confirm: reading reset request failed", "email", email, "error", err)
			return ErrPasswordResetUnavailable
		}
		// The newest request decides the path: a Meko-issued code, or a code
		// Cognito sent. A request made after a Meko code supersedes it.
		if latest != nil && latest.CodeHash != "" {
			return s.confirmMekoResetCode(ctx, email, key, code, newPw, latest)
		}
	}
	if err := s.cognito.ConfirmForgotPassword(ctx, email, code, newPw); err != nil {
		return fmt.Errorf("cognito confirm forgot password: %w", err)
	}
	return nil
}

// confirmMekoResetCode checks a Meko-issued code and, if it matches, sets the
// new password as permanent, which also moves the account out of
// FORCE_CHANGE_PASSWORD. Each check first reserves one of the code's
// maxResetCodeAttempts guesses in the store, atomically and in SQL, so
// concurrent requests can't get extra guesses; the reservation also rejects a
// used or expired code. The code is claimed before Cognito accepts so only one
// caller can set the password; a Cognito failure unclaims it for a retry.
// Errors use the same sentinels as Cognito's own codes, so the client can't
// tell the two paths apart.
func (s *AuthService) confirmMekoResetCode(
	ctx context.Context, email, key, code, newPw string, req *models.PasswordResetRequest,
) error {
	reserved, err := s.resetStore.ReserveResetAttempt(ctx, key, req.ID, maxResetCodeAttempts)
	if err != nil {
		slog.Error("forgot password confirm: reserving an attempt failed", "email", email, "error", err)
		return ErrPasswordResetUnavailable
	}
	if !reserved {
		return auth.ErrCodeExpired
	}
	if subtle.ConstantTimeCompare([]byte(hashResetCode(key, code)), []byte(req.CodeHash)) != 1 {
		return auth.ErrCodeMismatch
	}
	claimed, err := s.resetStore.ClaimResetCode(ctx, key, req.ID)
	if err != nil {
		slog.Error("forgot password confirm: claiming code failed", "email", email, "error", err)
		return ErrPasswordResetUnavailable
	}
	if !claimed {
		return auth.ErrCodeExpired
	}
	if err := s.cognito.SetPermanentPassword(ctx, email, newPw); err != nil {
		if unclaimErr := s.resetStore.UnclaimResetCode(ctx, key, req.ID); unclaimErr != nil {
			slog.Error("forgot password confirm: unclaim after Cognito failure failed",
				"email", email, "error", unclaimErr)
		}
		return fmt.Errorf("cognito set permanent password: %w", err)
	}
	slog.Info("forgot password confirm: Meko reset code accepted, password set", "email", email)
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
			rescued, rescueErr := s.rescueExpiredInvite(ctx, req.Email)
			if rescueErr != nil {
				return nil, rescueErr
			}
			if rescued {
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

// rescueExpiredInvite emails a fresh temporary password to an admin-created
// user whose invite temporary password expired. It uses the password-setup
// email when SES is configured, and Cognito's own invitation resend otherwise.
// The account stays in FORCE_CHANGE_PASSWORD, so signing in with the new
// temporary password leads to the normal NEW_PASSWORD_REQUIRED flow.
//
// It confirms the account really is in FORCE_CHANGE_PASSWORD before changing
// anything. It returns (true, nil) once the new password is sent, and
// (false, nil) on a problem that left the account unchanged, so the caller
// falls back to a normal 401. It returns ErrPasswordResetUnavailable when the
// email went out but Cognito then refused the new password; see
// sendTempPassword.
func (s *AuthService) rescueExpiredInvite(ctx context.Context, email string) (bool, error) {
	status, err := s.cognito.GetUserStatus(ctx, email)
	if err != nil {
		slog.Warn("expired-invite rescue: status lookup failed", "email", email, "error", err)
		return false, nil
	}
	if status != auth.UserStatusForceChangePassword {
		slog.Warn("expired-invite rescue: unexpected status, skipping",
			"email", email, "status", string(status))
		return false, nil
	}
	if s.passwordSetupEmailer == nil {
		if err := s.cognito.ResendInvite(ctx, email); err != nil {
			slog.Error("expired-invite rescue: resend invite failed", "email", email, "error", err)
			return false, nil
		}
	} else if err := s.sendTempPassword(ctx, email, PasswordSetupInviteExpired); err != nil {
		slog.Error("expired-invite rescue: sending temporary password failed", "email", email, "error", err)
		if errors.Is(err, ErrPasswordResetUnavailable) {
			return false, ErrPasswordResetUnavailable
		}
		return false, nil
	}
	slog.Info("expired-invite rescue: fresh temporary password sent, user can complete first login",
		"email", email)
	return true, nil
}

// errPasswordSetupEmailFailed is returned by sendTempPassword when the email
// failed and nothing in Cognito changed.
var errPasswordSetupEmailFailed = errors.New("password-setup email failed")

// sendTempPassword emails a fresh temporary password to a FORCE_CHANGE_PASSWORD
// user with the password-setup template, then sets it in Cognito. Its only
// caller is the expired-invite rescue, so the user's current temporary password
// has already expired.
//
// The email goes first. If SES fails, nothing changed: it returns
// errPasswordSetupEmailFailed, and signing in again with the expired password
// runs the rescue again. If Cognito then refuses the new password, the email
// holds a password that won't work and the expired one is still in place: it
// returns ErrPasswordResetUnavailable, which the handler answers with a 503 so
// the user tries again rather than seeing "invalid credentials". Setting the
// password first would be worse: a failed email would leave a password the
// user never received, and the expired password would no longer trigger the
// rescue.
func (s *AuthService) sendTempPassword(ctx context.Context, email string, reason PasswordSetupReason) error {
	if s.passwordSetupEmailer == nil {
		return errPasswordSetupEmailDisabled
	}
	tempPwd, err := genTempPassword()
	if err != nil {
		return fmt.Errorf("generate temporary password: %w", err)
	}
	if err := s.passwordSetupEmailer.SendPasswordSetupEmail(ctx, PasswordSetupEmailParams{
		RecipientEmail: email,
		TempPassword:   tempPwd,
		Reason:         reason,
	}); err != nil {
		slog.Error("password-setup email failed", "email", email, "reason", string(reason), "error", err)
		return errPasswordSetupEmailFailed
	}
	if err := s.cognito.ResetTempPassword(ctx, email, tempPwd); err != nil {
		slog.Error("password-setup: email sent but Cognito refused the temporary password",
			"email", email, "reason", string(reason), "error", err)
		return ErrPasswordResetUnavailable
	}
	slog.Info("password-setup email sent", "email", email, "reason", string(reason))
	return nil
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
// federated user past the post-signup email-project gate without ever
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
		FirstName:  normalizeFederatedName(claims.GivenName),
		LastName:   normalizeFederatedName(claims.FamilyName),
		Email:      claims.Email,
		CognitoSub: claims.Sub,
		// Under an issuer that owns its users, a real address in a verified ID
		// token is as verified as it gets here — see WithIssuerOwnedEmail. The
		// placeholder check still applies: a synthesized address is nobody's.
		EmailVerified: claims.emailIsVerified() ||
			(s.issuerOwnsEmail && claims.Email != "" && !isPlaceholderEmail(claims.Email)),
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

// StartEmailChange begins the post-signup email-project flow for a federated
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
