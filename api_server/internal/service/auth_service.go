package service

import (
	"context"
	"encoding/base64"
	"encoding/json"
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
	Login(
		ctx context.Context, email, password string,
	) (*auth.Result, error)
	Logout(ctx context.Context, accessToken string) error
}

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
}

// AuthService orchestrates user authentication operations: signup,
// email confirmation, login, and logout.
type AuthService struct {
	cognito  CognitoProvider
	accounts AccountStore
}

// NewAuthService creates a new AuthService.
func NewAuthService(
	cognito CognitoProvider,
	accounts AccountStore,
) *AuthService {
	return &AuthService{
		cognito:  cognito,
		accounts: accounts,
	}
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

// LoginRequest holds the input for user login.
type LoginRequest struct {
	Email    string `json:"email"`
	Password string `json:"password"`
}

// LoginResponse holds the tokens returned by a successful login.
type LoginResponse struct {
	AccessToken  string `json:"access_token"`
	IDToken      string `json:"id_token"`
	RefreshToken string `json:"refresh_token"`
	ExpiresIn    int32  `json:"expires_in"`
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

	s.ensureAccountFromIDToken(ctx, result.IDToken)

	slog.Info("user login succeeded", "email", req.Email)

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

// idTokenClaims holds the subset of Cognito ID token claims used for
// just-in-time local account creation.
type idTokenClaims struct {
	Sub        string `json:"sub"`
	Email      string `json:"email"`
	GivenName  string `json:"given_name"`
	FamilyName string `json:"family_name"`
}

// ensureAccountFromIDToken parses the Cognito ID token (without
// cryptographic verification — the token came directly from the
// Cognito API) and creates a local account if one does not already
// exist for the user's cognito sub.
func (s *AuthService) ensureAccountFromIDToken(
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
