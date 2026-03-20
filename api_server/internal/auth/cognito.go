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
}

// Result holds the tokens returned by a successful login.
type Result struct {
	AccessToken  string `json:"access_token"`
	IDToken      string `json:"id_token"`
	RefreshToken string `json:"refresh_token"`
	ExpiresIn    int32  `json:"expires_in"`
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

	if output.AuthenticationResult == nil {
		slog.Error("cognito login returned nil auth result",
			"email", email,
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
	ErrUserExists       = errors.New("user already exists")
	ErrInvalidPassword  = errors.New("password does not meet requirements")
	ErrCodeMismatch     = errors.New("invalid confirmation code")
	ErrCodeExpired      = errors.New("confirmation code has expired")
	ErrNotAuthorized    = errors.New("invalid credentials")
	ErrUserNotFound     = errors.New("user not found")
	ErrUserNotConfirmed = errors.New("user email not confirmed")
	ErrAuthResultNil    = errors.New("authentication result is nil")
	ErrTooManyRequests  = errors.New("too many requests")
	ErrLimitExceeded    = errors.New("request limit exceeded")
	ErrCognitoInternal  = errors.New("cognito internal error")
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

	var notAuthErr *types.NotAuthorizedException
	if errors.As(err, &notAuthErr) {
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

	return fmt.Errorf("cognito error: %w", err)
}

func strPtr(s string) *string {
	return &s
}
