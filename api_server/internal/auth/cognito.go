// Package auth provides AWS Cognito authentication, JWT validation,
// and Echo middleware for the meko API server.
package auth

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	awscfg "github.com/aws/aws-sdk-go-v2/config"
	cip "github.com/aws/aws-sdk-go-v2/service/cognitoidentityprovider"
	"github.com/aws/aws-sdk-go-v2/service/cognitoidentityprovider/types"
)

// CognitoAPI abstracts the subset of the AWS Cognito Identity Provider
// client used by CognitoClient, enabling test doubles.
type CognitoAPI interface {
	SignUp(
		ctx context.Context,
		params *cip.SignUpInput,
		optFns ...func(*cip.Options),
	) (*cip.SignUpOutput, error)

	ConfirmSignUp(
		ctx context.Context,
		params *cip.ConfirmSignUpInput,
		optFns ...func(*cip.Options),
	) (*cip.ConfirmSignUpOutput, error)

	InitiateAuth(
		ctx context.Context,
		params *cip.InitiateAuthInput,
		optFns ...func(*cip.Options),
	) (*cip.InitiateAuthOutput, error)

	GlobalSignOut(
		ctx context.Context,
		params *cip.GlobalSignOutInput,
		optFns ...func(*cip.Options),
	) (*cip.GlobalSignOutOutput, error)

	AdminCreateUser(
		ctx context.Context,
		params *cip.AdminCreateUserInput,
		optFns ...func(*cip.Options),
	) (*cip.AdminCreateUserOutput, error)

	// RespondToAuthChallenge completes a Cognito challenge such as
	// NEW_PASSWORD_REQUIRED that was returned by InitiateAuth.
	RespondToAuthChallenge(
		ctx context.Context,
		params *cip.RespondToAuthChallengeInput,
		optFns ...func(*cip.Options),
	) (*cip.RespondToAuthChallengeOutput, error)

	// ResendConfirmationCode emails a new verification code to an existing
	// unconfirmed user (Cognito user pool sign-up flow).
	ResendConfirmationCode(
		ctx context.Context,
		params *cip.ResendConfirmationCodeInput,
		optFns ...func(*cip.Options),
	) (*cip.ResendConfirmationCodeOutput, error)

	// ForgotPassword emails a confirmation code for password reset (self-service).
	ForgotPassword(
		ctx context.Context,
		params *cip.ForgotPasswordInput,
		optFns ...func(*cip.Options),
	) (*cip.ForgotPasswordOutput, error)

	// ConfirmForgotPassword sets a new password using the emailed code.
	ConfirmForgotPassword(
		ctx context.Context,
		params *cip.ConfirmForgotPasswordInput,
		optFns ...func(*cip.Options),
	) (*cip.ConfirmForgotPasswordOutput, error)

	// ChangePassword changes the password for a signed-in user. The user
	// is identified by the AccessToken (Cognito resolves the sub from the
	// JWT signature) — no email/user_id needed.
	ChangePassword(
		ctx context.Context,
		params *cip.ChangePasswordInput,
		optFns ...func(*cip.Options),
	) (*cip.ChangePasswordOutput, error)
}

// Result holds the tokens returned by a successful login. When
// Cognito responds to InitiateAuth with a challenge instead of tokens
// (e.g. NEW_PASSWORD_REQUIRED for users in FORCE_CHANGE_PASSWORD
// state), AccessToken/IDToken/RefreshToken are empty and
// ChallengeName + Session carry the data needed to complete the
// challenge via a follow-up RespondToAuthChallenge call.
type Result struct {
	AccessToken  string `json:"access_token"`
	IDToken      string `json:"id_token"`
	RefreshToken string `json:"refresh_token"`
	ExpiresIn    int32  `json:"expires_in"`

	ChallengeName string `json:"challenge_name,omitempty"`
	Session       string `json:"session,omitempty"`
}

// CognitoClient wraps the AWS Cognito Identity Provider SDK to provide
// signup, confirmation, login, and logout operations.
type CognitoClient struct {
	api          CognitoAPI
	poolID       string
	clientID     string
	clientSecret string
}

// NewCognitoClient creates a CognitoClient configured for the given
// AWS region, Cognito User Pool ID, App Client ID, and optional client
// secret.
func NewCognitoClient(
	ctx context.Context,
	region, poolID, clientID, clientSecret string,
) (*CognitoClient, error) {
	cfg, err := awscfg.LoadDefaultConfig(ctx,
		awscfg.WithRegion(region),
	)
	if err != nil {
		return nil, fmt.Errorf("loading aws config: %w", err)
	}

	client := cip.NewFromConfig(cfg)

	slog.Info("cognito client initialized",
		"region", region,
		"pool_id", poolID,
		"has_secret", clientSecret != "",
	)

	return &CognitoClient{
		api:          client,
		poolID:       poolID,
		clientID:     clientID,
		clientSecret: clientSecret,
	}, nil
}

// NewCognitoClientWithAPI creates a CognitoClient with a caller-supplied
// CognitoAPI implementation (useful for testing).
func NewCognitoClientWithAPI(
	api CognitoAPI,
	poolID, clientID, clientSecret string,
) *CognitoClient {
	return &CognitoClient{
		api:          api,
		poolID:       poolID,
		clientID:     clientID,
		clientSecret: clientSecret,
	}
}

// SignUp registers a new user in the Cognito User Pool and returns the
// Cognito user sub (unique identifier).
func (c *CognitoClient) SignUp(
	ctx context.Context,
	email, password, firstName, lastName string,
) (string, error) {
	input := &cip.SignUpInput{
		ClientId:   &c.clientID,
		Username:   &email,
		Password:   &password,
		SecretHash: c.secretHash(email),
		UserAttributes: []types.AttributeType{
			{Name: strPtr("email"), Value: &email},
			{Name: strPtr("given_name"), Value: &firstName},
			{Name: strPtr("family_name"), Value: &lastName},
		},
	}

	output, err := c.api.SignUp(ctx, input)
	if err != nil {
		slog.Error("cognito signup failed",
			"email", email, "error", err,
		)
		return "", classifyCognitoErr(err)
	}

	userSub := ""
	if output.UserSub != nil {
		userSub = *output.UserSub
	}

	slog.Info("cognito signup succeeded",
		"email", email, "cognito_sub", userSub,
	)
	return userSub, nil
}

// ConfirmSignUp verifies a user's email using the confirmation code
// sent by Cognito.
func (c *CognitoClient) ConfirmSignUp(
	ctx context.Context,
	email, code string,
) error {
	input := &cip.ConfirmSignUpInput{
		ClientId:         &c.clientID,
		Username:         &email,
		ConfirmationCode: &code,
		SecretHash:       c.secretHash(email),
	}

	_, err := c.api.ConfirmSignUp(ctx, input)
	if err != nil {
		slog.Warn("cognito confirm signup failed",
			"email", email, "error", err,
		)
		return classifyCognitoErr(err)
	}

	slog.Info("cognito email confirmed", "email", email)
	return nil
}

// ResendConfirmationCode asks Cognito to send another email verification
// code to the user. Only applies to users in UNCONFIRMED state (same app
// client + SECRET_HASH rules as SignUp / ConfirmSignUp).
func (c *CognitoClient) ResendConfirmationCode(ctx context.Context, email string) error {
	email = strings.TrimSpace(email)
	if email == "" {
		return fmt.Errorf("email is required")
	}
	input := &cip.ResendConfirmationCodeInput{
		ClientId:   &c.clientID,
		Username:   &email,
		SecretHash: c.secretHash(email),
	}
	_, err := c.api.ResendConfirmationCode(ctx, input)
	if err != nil {
		slog.Warn("cognito resend confirmation failed",
			"email", email, "error", err,
		)
		return classifyCognitoErr(err)
	}
	slog.Info("cognito confirmation code resent", "email", email)
	return nil
}

// ForgotPassword triggers Cognito to send a password-reset verification code
// to the user (email/SMS per pool configuration).
func (c *CognitoClient) ForgotPassword(ctx context.Context, email string) error {
	email = strings.TrimSpace(email)
	if email == "" {
		return fmt.Errorf("email is required")
	}
	input := &cip.ForgotPasswordInput{
		ClientId:   &c.clientID,
		Username:   &email,
		SecretHash: c.secretHash(email),
	}
	_, err := c.api.ForgotPassword(ctx, input)
	if err != nil {
		slog.Warn("cognito forgot password failed", "email", email, "error", err)
		return classifyCognitoErr(err)
	}
	slog.Info("cognito forgot password code sent", "email", email)
	return nil
}

// ConfirmForgotPassword sets a new password using the code from ForgotPassword.
func (c *CognitoClient) ConfirmForgotPassword(
	ctx context.Context, email, code, newPassword string,
) error {
	email = strings.TrimSpace(email)
	code = strings.TrimSpace(code)
	if email == "" {
		return fmt.Errorf("email is required")
	}
	if code == "" {
		return fmt.Errorf("confirmation code is required")
	}
	if newPassword == "" {
		return fmt.Errorf("password is required")
	}
	input := &cip.ConfirmForgotPasswordInput{
		ClientId:         &c.clientID,
		Username:         &email,
		ConfirmationCode: &code,
		Password:         &newPassword,
		SecretHash:       c.secretHash(email),
	}
	_, err := c.api.ConfirmForgotPassword(ctx, input)
	if err != nil {
		slog.Warn("cognito confirm forgot password failed", "email", email, "error", err)
		return classifyCognitoErr(err)
	}
	slog.Info("cognito password reset complete", "email", email)
	return nil
}

// Login authenticates a user and returns a Result containing access,
// ID, and refresh tokens.
func (c *CognitoClient) Login(
	ctx context.Context,
	email, password string,
) (*Result, error) {
	authParams := map[string]string{
		"USERNAME": email,
		"PASSWORD": password,
	}
	if hash := c.secretHash(email); hash != nil {
		authParams["SECRET_HASH"] = *hash
	}

	input := &cip.InitiateAuthInput{
		AuthFlow:       types.AuthFlowTypeUserPasswordAuth,
		ClientId:       &c.clientID,
		AuthParameters: authParams,
	}

	output, err := c.api.InitiateAuth(ctx, input)
	if err != nil {
		slog.Warn("cognito login failed",
			"email", email, "error", err,
		)
		return nil, classifyCognitoErr(err)
	}

	// Users created via AdminCreateUser sit in FORCE_CHANGE_PASSWORD
	// state until they set a permanent password. Cognito surfaces
	// that as a NEW_PASSWORD_REQUIRED challenge with no auth result;
	// hand the session back to the caller so it can be completed via
	// CompleteNewPassword.
	if output.ChallengeName == types.ChallengeNameTypeNewPasswordRequired {
		session := ""
		if output.Session != nil {
			session = *output.Session
		}
		slog.Info("cognito login requires new password",
			"email", email,
		)
		return &Result{
			ChallengeName: string(types.ChallengeNameTypeNewPasswordRequired),
			Session:       session,
		}, nil
	}

	if output.AuthenticationResult == nil {
		slog.Error("cognito login returned nil auth result",
			"email", email,
			"challenge_name", string(output.ChallengeName),
		)
		return nil, ErrAuthResultNil
	}

	result := &Result{
		ExpiresIn: output.AuthenticationResult.ExpiresIn,
	}
	if output.AuthenticationResult.AccessToken != nil {
		result.AccessToken = *output.AuthenticationResult.AccessToken
	}
	if output.AuthenticationResult.IdToken != nil {
		result.IDToken = *output.AuthenticationResult.IdToken
	}
	if output.AuthenticationResult.RefreshToken != nil {
		result.RefreshToken = *output.AuthenticationResult.RefreshToken
	}

	slog.Info("cognito login succeeded", "email", email)
	return result, nil
}

// CompleteNewPassword completes the NEW_PASSWORD_REQUIRED challenge
// returned from Login for users in FORCE_CHANGE_PASSWORD state. The
// session must be the one returned by the prior Login call; Cognito
// rejects expired or reused sessions as NotAuthorizedException.
//
// firstName and lastName are forwarded as
// `userAttributes.given_name` / `userAttributes.family_name` in the
// challenge response when non-empty. Cognito requires these whenever
// the User Pool marks given_name / family_name as required attributes
// and the admin-created user does not already have them set; without
// them Cognito rejects the call with InvalidParameterException
// ("Invalid attributes given, given_name is missing").
func (c *CognitoClient) CompleteNewPassword(
	ctx context.Context,
	email, newPassword, session, firstName, lastName string,
) (*Result, error) {
	email = strings.TrimSpace(email)
	if email == "" {
		return nil, fmt.Errorf("email is required")
	}
	if newPassword == "" {
		return nil, fmt.Errorf("password is required")
	}
	if session == "" {
		return nil, fmt.Errorf("session is required")
	}

	challengeResponses := map[string]string{
		"USERNAME":     email,
		"NEW_PASSWORD": newPassword,
	}
	if firstName = strings.TrimSpace(firstName); firstName != "" {
		challengeResponses["userAttributes.given_name"] = firstName
	}
	if lastName = strings.TrimSpace(lastName); lastName != "" {
		challengeResponses["userAttributes.family_name"] = lastName
	}
	if hash := c.secretHash(email); hash != nil {
		challengeResponses["SECRET_HASH"] = *hash
	}

	input := &cip.RespondToAuthChallengeInput{
		ChallengeName:      types.ChallengeNameTypeNewPasswordRequired,
		ClientId:           &c.clientID,
		Session:            &session,
		ChallengeResponses: challengeResponses,
	}

	output, err := c.api.RespondToAuthChallenge(ctx, input)
	if err != nil {
		slog.Warn("cognito complete new password failed",
			"email", email, "error", err,
		)
		return nil, classifyCognitoErr(err)
	}

	if output.AuthenticationResult == nil {
		slog.Error("cognito new password returned nil auth result",
			"email", email,
			"challenge_name", string(output.ChallengeName),
		)
		return nil, ErrAuthResultNil
	}

	result := &Result{
		ExpiresIn: output.AuthenticationResult.ExpiresIn,
	}
	if output.AuthenticationResult.AccessToken != nil {
		result.AccessToken = *output.AuthenticationResult.AccessToken
	}
	if output.AuthenticationResult.IdToken != nil {
		result.IDToken = *output.AuthenticationResult.IdToken
	}
	if output.AuthenticationResult.RefreshToken != nil {
		result.RefreshToken = *output.AuthenticationResult.RefreshToken
	}

	slog.Info("cognito new password set", "email", email)
	return result, nil
}

// Logout globally signs out the user, invalidating all issued tokens.
func (c *CognitoClient) Logout(
	ctx context.Context,
	accessToken string,
) error {
	input := &cip.GlobalSignOutInput{
		AccessToken: &accessToken,
	}

	_, err := c.api.GlobalSignOut(ctx, input)
	if err != nil {
		slog.Warn("cognito logout failed", "error", err)
		return classifyCognitoErr(err)
	}

	slog.Info("cognito logout succeeded")
	return nil
}

// ChangePassword changes the password for the user identified by
// `accessToken`. Cognito verifies `previousPassword` against the current
// stored value and applies `proposedPassword` — both arguments are
// validated by Cognito (password policy compliance, etc.) and any
// failure (wrong old password, weak new one) surfaces as a typed
// `classifyCognitoErr` value the handler maps to HTTP 400 / 401.
//
// Unlike forgot-password, this flow does NOT require an emailed code —
// the access token IS the identity proof. So the user must be signed in.
func (c *CognitoClient) ChangePassword(
	ctx context.Context,
	accessToken, previousPassword, proposedPassword string,
) error {
	input := &cip.ChangePasswordInput{
		AccessToken:      &accessToken,
		PreviousPassword: &previousPassword,
		ProposedPassword: &proposedPassword,
	}

	_, err := c.api.ChangePassword(ctx, input)
	if err != nil {
		slog.Warn("cognito change password failed", "error", err)
		return classifyCognitoErr(err)
	}

	slog.Info("cognito change password succeeded")
	return nil
}

// ExchangeCode exchanges an OAuth2 authorization code (from the
// Cognito Hosted UI redirect) for tokens by calling the Cognito
// /oauth2/token endpoint.
func (c *CognitoClient) ExchangeCode(
	ctx context.Context,
	code, cognitoDomain, redirectURI string,
) (*Result, error) {
	tokenURL := fmt.Sprintf("https://%s/oauth2/token", cognitoDomain)

	form := url.Values{
		"grant_type":   {"authorization_code"},
		"client_id":    {c.clientID},
		"code":         {code},
		"redirect_uri": {redirectURI},
	}
	if c.clientSecret != "" {
		form.Set("client_secret", c.clientSecret)
	}

	req, err := http.NewRequestWithContext(
		ctx, http.MethodPost, tokenURL,
		strings.NewReader(form.Encode()),
	)
	if err != nil {
		return nil, fmt.Errorf("building token request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		slog.Error("cognito token exchange failed",
			"url", tokenURL, "error", err,
		)
		return nil, fmt.Errorf("token exchange request: %w", err)
	}
	defer func() {
		if closeErr := resp.Body.Close(); closeErr != nil {
			slog.Warn("failed to close token response body",
				"error", closeErr,
			)
		}
	}()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("reading token response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		slog.Error("cognito token exchange returned error",
			"status", resp.StatusCode, "body", string(body),
		)
		return nil, fmt.Errorf(
			"token exchange returned status %d", resp.StatusCode,
		)
	}

	var tokenResp struct {
		AccessToken  string `json:"access_token"`
		IDToken      string `json:"id_token"`
		RefreshToken string `json:"refresh_token"`
		ExpiresIn    int32  `json:"expires_in"`
		TokenType    string `json:"token_type"`
	}
	if err := json.Unmarshal(body, &tokenResp); err != nil {
		return nil, fmt.Errorf("parsing token response: %w", err)
	}

	slog.Info("cognito code exchange succeeded")

	return &Result{
		AccessToken:  tokenResp.AccessToken,
		IDToken:      tokenResp.IDToken,
		RefreshToken: tokenResp.RefreshToken,
		ExpiresIn:    tokenResp.ExpiresIn,
	}, nil
}

// InviteUser creates (or re-invites) a user in the Cognito User Pool
// via AdminCreateUser. Cognito sends them a signup invitation email
// with a temporary password. Set resend=true to re-deliver the invite
// for an already-created user.
func (c *CognitoClient) InviteUser(
	ctx context.Context,
	email string,
	resend bool,
) error {
	input := &cip.AdminCreateUserInput{
		UserPoolId:             &c.poolID,
		Username:               &email,
		DesiredDeliveryMediums: []types.DeliveryMediumType{types.DeliveryMediumTypeEmail},
		UserAttributes: []types.AttributeType{
			{Name: strPtr("email"), Value: &email},
			{Name: strPtr("email_verified"), Value: strPtr("true")},
		},
	}
	if resend {
		input.MessageAction = types.MessageActionTypeResend
	}

	_, err := c.api.AdminCreateUser(ctx, input)
	if err != nil {
		var usernameExistsErr *types.UsernameExistsException
		if errors.As(err, &usernameExistsErr) {
			slog.Info("cognito invite: user already exists, skipping",
				"email", email,
			)
			return nil
		}
		slog.Error("cognito invite user failed",
			"email", email, "resend", resend, "error", err,
		)
		return fmt.Errorf("cognito admin create user: %w", err)
	}

	slog.Info("cognito invite user succeeded",
		"email", email, "resend", resend,
	)
	return nil
}

// secretHash computes the HMAC-SHA256 SECRET_HASH required when the
// Cognito App Client is configured with a client secret. Returns nil
// when no secret is configured.
func (c *CognitoClient) secretHash(username string) *string {
	if c.clientSecret == "" {
		return nil
	}
	mac := hmac.New(sha256.New, []byte(c.clientSecret))
	mac.Write([]byte(username + c.clientID))
	hash := base64.StdEncoding.EncodeToString(mac.Sum(nil))
	return &hash
}

// Sentinel errors returned by CognitoClient methods.
var (
	ErrUserExists              = errors.New("user already exists")
	ErrInvalidPassword         = errors.New("password does not meet requirements")
	ErrCodeMismatch            = errors.New("invalid confirmation code")
	ErrCodeExpired             = errors.New("confirmation code has expired")
	ErrNotAuthorized           = errors.New("invalid credentials")
	ErrUserNotFound            = errors.New("user not found")
	ErrUserNotConfirmed        = errors.New("user email not confirmed")
	ErrUserAlreadyConfirmed    = errors.New("user email is already confirmed")
	ErrAuthResultNil           = errors.New("authentication result is nil")
	ErrTooManyRequests         = errors.New("too many requests")
	ErrLimitExceeded           = errors.New("request limit exceeded")
	ErrCognitoInternal         = errors.New("cognito internal error")
	ErrInvalidCognitoParameter = errors.New("invalid cognito request")
)

// classifyCognitoErr maps AWS Cognito SDK exceptions to sentinel errors.
func classifyCognitoErr(err error) error {
	var usernameExistsErr *types.UsernameExistsException
	if errors.As(err, &usernameExistsErr) {
		return ErrUserExists
	}

	var invalidPasswordErr *types.InvalidPasswordException
	if errors.As(err, &invalidPasswordErr) {
		return ErrInvalidPassword
	}

	var codeMismatchErr *types.CodeMismatchException
	if errors.As(err, &codeMismatchErr) {
		return ErrCodeMismatch
	}

	var expiredCodeErr *types.ExpiredCodeException
	if errors.As(err, &expiredCodeErr) {
		return ErrCodeExpired
	}

	// Login can surface UserNotConfirmedException, but some pools return
	// NotAuthorizedException with a message like "User is not confirmed" instead.
	var notAuthErr *types.NotAuthorizedException
	if errors.As(err, &notAuthErr) {
		if notAuthErr.Message != nil &&
			strings.Contains(strings.ToLower(*notAuthErr.Message), "not confirmed") {
			return ErrUserNotConfirmed
		}
		return ErrNotAuthorized
	}

	var userNotFoundErr *types.UserNotFoundException
	if errors.As(err, &userNotFoundErr) {
		return ErrUserNotFound
	}

	var notConfirmedErr *types.UserNotConfirmedException
	if errors.As(err, &notConfirmedErr) {
		return ErrUserNotConfirmed
	}

	var tooManyErr *types.TooManyRequestsException
	if errors.As(err, &tooManyErr) {
		return ErrTooManyRequests
	}

	var limitErr *types.LimitExceededException
	if errors.As(err, &limitErr) {
		return ErrLimitExceeded
	}

	var internalErr *types.InternalErrorException
	if errors.As(err, &internalErr) {
		return ErrCognitoInternal
	}

	// InvalidParameterException is ambiguous: it can mean the client sent a
	// bad value (e.g. resend on an already-confirmed user) OR that the
	// server is misconfigured (wrong SECRET_HASH, ClientId, missing auth
	// flow, etc.). Log the original message explicitly so operators can
	// triage even when the API response body stays generic, and wrap the
	// sentinel so the original detail survives in the error chain.
	var invalidParamErr *types.InvalidParameterException
	if errors.As(err, &invalidParamErr) {
		msg := ""
		if invalidParamErr.Message != nil {
			msg = *invalidParamErr.Message
		}
		// Special-case the resend-on-already-confirmed flow: Cognito uses
		// InvalidParameterException with this exact phrasing instead of a
		// dedicated exception type, so callers benefit from a clearer code.
		slog.Warn("cognito invalid parameter (verify client input or pool/app-client config)",
			"message", msg,
		)
		if strings.Contains(strings.ToLower(msg), "already confirmed") {
			return ErrUserAlreadyConfirmed
		}
		if msg == "" {
			return ErrInvalidCognitoParameter
		}
		return fmt.Errorf("%w: %s", ErrInvalidCognitoParameter, msg)
	}

	return fmt.Errorf("cognito error: %w", err)
}

func strPtr(s string) *string {
	return &s
}
