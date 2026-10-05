package mekoapi

import (
	"context"
	"net/http"
)

// LoginResult is the POST /auth/login body. A user who must set a password
// first gets Challenge and Session and no tokens.
type LoginResult struct {
	AccessToken  string `json:"access_token"`
	IDToken      string `json:"id_token"`
	RefreshToken string `json:"refresh_token"`
	ExpiresIn    int    `json:"expires_in"`
	Challenge    string `json:"challenge"`
	Session      string `json:"session"`
	Email        string `json:"email"`
}

type SignupRequest struct {
	Email     string `json:"email"`
	Password  string `json:"password"`
	FirstName string `json:"first_name"`
	LastName  string `json:"last_name"`
}

type SignupResult struct {
	AccountID string `json:"account_id"`
	Email     string `json:"email"`
	Message   string `json:"message"`
}

type NewPasswordRequest struct {
	Email           string `json:"email"`
	NewPassword     string `json:"new_password"`
	ConfirmPassword string `json:"confirm_password"`
	Session         string `json:"session"`
	FirstName       string `json:"first_name,omitempty"`
	LastName        string `json:"last_name,omitempty"`
}

type Me struct {
	AccountID              string `json:"account_id"`
	Email                  string `json:"email"`
	FirstName              string `json:"first_name"`
	LastName               string `json:"last_name"`
	EmailVerified          bool   `json:"email_verified"`
	HasCompletedOnboarding bool   `json:"has_completed_onboarding"`
	HasEmailPasswordLogin  bool   `json:"has_email_password_login"`
	UserType               string `json:"user_type"`
}

func (c *Client) Login(ctx context.Context, email, password string) (LoginResult, error) {
	var out LoginResult
	err := c.do(ctx, http.MethodPost, "/auth/login", map[string]string{"email": email, "password": password}, &out)
	return out, err
}

func (c *Client) Signup(ctx context.Context, req SignupRequest) (SignupResult, error) {
	var out SignupResult
	err := c.do(ctx, http.MethodPost, "/auth/signup", req, &out)
	return out, err
}

func (c *Client) ConfirmSignup(ctx context.Context, email, code string) error {
	return c.do(ctx, http.MethodPost, "/auth/confirm-signup", map[string]string{"email": email, "code": code}, nil)
}

func (c *Client) CompleteNewPassword(ctx context.Context, req NewPasswordRequest) (LoginResult, error) {
	var out LoginResult
	err := c.do(ctx, http.MethodPost, "/auth/complete-new-password", req, &out)
	return out, err
}

func (c *Client) Me(ctx context.Context) (Me, error) {
	var out Me
	err := c.do(ctx, http.MethodGet, "/auth/me", nil, &out)
	return out, err
}

func (c *Client) Logout(ctx context.Context) error {
	return c.do(ctx, http.MethodPost, "/auth/logout", nil, nil)
}

func (c *Client) Token() string { return c.token }
