package handlers_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/labstack/echo/v4"

	"meko/api_server/internal/auth"
	"meko/api_server/internal/handlers"
	"meko/api_server/internal/models"
	"meko/api_server/internal/repository"
	"meko/api_server/internal/service"
)

func handlerFakeIDToken(sub, email string) string {
	header := base64.RawURLEncoding.EncodeToString(
		[]byte(`{"alg":"RS256","typ":"JWT"}`),
	)
	claims := map[string]string{"sub": sub, "email": email}
	payload, _ := json.Marshal(claims)
	payloadB64 := base64.RawURLEncoding.EncodeToString(payload)
	return header + "." + payloadB64 + ".fake-sig"
}

func noopAccountStore() *handlerMockAccountStore {
	return &handlerMockAccountStore{
		getByCognitoSubFn: func(
			_ context.Context, _ string,
		) (*models.Account, error) {
			return &models.Account{ID: "acct-1"}, nil
		},
		createFn: func(
			_ context.Context, _ repository.CreateAccountParams,
		) (*models.Account, error) {
			return &models.Account{ID: "acct-1"}, nil
		},
		verifyEmailFn: func(_ context.Context, _ string) error {
			return nil
		},
	}
}

// --- mock CognitoProvider for handler tests ---

type handlerMockCognito struct {
	signUpFn                func(ctx context.Context, email, password, firstName, lastName string) (string, error)
	confirmFn               func(ctx context.Context, email, code string) error
	resendFn                func(ctx context.Context, email string) error
	forgotPasswordFn        func(ctx context.Context, email string) error
	confirmForgotPasswordFn func(ctx context.Context, email, code, newPassword string) error
	loginFn                 func(ctx context.Context, email, password string) (*auth.Result, error)
	logoutFn                func(ctx context.Context, accessToken string) error
}

func (m *handlerMockCognito) SignUp(
	ctx context.Context,
	email, password, firstName, lastName string,
) (string, error) {
	return m.signUpFn(ctx, email, password, firstName, lastName)
}

func (m *handlerMockCognito) ConfirmSignUp(
	ctx context.Context, email, code string,
) error {
	return m.confirmFn(ctx, email, code)
}

func (m *handlerMockCognito) ResendConfirmationCode(
	ctx context.Context, email string,
) error {
	if m.resendFn != nil {
		return m.resendFn(ctx, email)
	}
	return nil
}

func (m *handlerMockCognito) ForgotPassword(ctx context.Context, email string) error {
	if m.forgotPasswordFn != nil {
		return m.forgotPasswordFn(ctx, email)
	}
	return nil
}

func (m *handlerMockCognito) ConfirmForgotPassword(
	ctx context.Context, email, code, newPassword string,
) error {
	if m.confirmForgotPasswordFn != nil {
		return m.confirmForgotPasswordFn(ctx, email, code, newPassword)
	}
	return nil
}

func (m *handlerMockCognito) Login(
	ctx context.Context, email, password string,
) (*auth.Result, error) {
	return m.loginFn(ctx, email, password)
}

func (m *handlerMockCognito) Logout(
	ctx context.Context, accessToken string,
) error {
	return m.logoutFn(ctx, accessToken)
}

// --- mock AccountStore for handler tests ---

type handlerMockAccountStore struct {
	createFn          func(ctx context.Context, params repository.CreateAccountParams) (*models.Account, error)
	getByCognitoSubFn func(ctx context.Context, sub string) (*models.Account, error)
	verifyEmailFn     func(ctx context.Context, email string) error
	setOnboardingFn   func(ctx context.Context, accountID string, completed bool) error
}

func (m *handlerMockAccountStore) Create(
	ctx context.Context, params repository.CreateAccountParams,
) (*models.Account, error) {
	return m.createFn(ctx, params)
}

func (m *handlerMockAccountStore) GetByCognitoSub(
	ctx context.Context, sub string,
) (*models.Account, error) {
	return m.getByCognitoSubFn(ctx, sub)
}

func (m *handlerMockAccountStore) VerifyEmail(
	ctx context.Context, email string,
) error {
	return m.verifyEmailFn(ctx, email)
}

func (m *handlerMockAccountStore) SetHasCompletedOnboarding(
	ctx context.Context, accountID string, completed bool,
) error {
	if m.setOnboardingFn == nil {
		return nil
	}
	return m.setOnboardingFn(ctx, accountID, completed)
}

// setupAuthTestServer creates an Echo instance with auth routes wired
// to the provided mock dependencies.
func setupAuthTestServer(
	t *testing.T,
	cog service.CognitoProvider,
	store service.AccountStore,
) *httptest.Server {
	t.Helper()

	authSvc := service.NewAuthService(cog, store)
	authH := handlers.NewAuthHandler(authSvc)

	e := echo.New()
	e.HideBanner = true
	e.HTTPErrorHandler = handlers.CustomHTTPErrorHandler

	g := e.Group("/api/v1/auth")
	g.POST("/signup", authH.Signup)
	g.POST("/confirm-signup", authH.ConfirmSignup)
	g.POST("/resend-confirmation", authH.ResendConfirmation)
	g.POST("/forgot-password", authH.ForgotPassword)
	g.POST("/forgot-password/confirm", authH.ForgotPasswordConfirm)
	g.POST("/login", authH.Login)
	g.POST("/logout", authH.Logout)

	return httptest.NewServer(e)
}

func postJSON(
	t *testing.T, url string, body any,
) *http.Response {
	t.Helper()
	b, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("failed to marshal body: %v", err)
	}
	resp, err := http.Post( //nolint:gosec // test URL
		url, "application/json", bytes.NewReader(b),
	)
	if err != nil {
		t.Fatalf("POST %s failed: %v", url, err)
	}
	return resp
}

func decodeResponse(
	t *testing.T, resp *http.Response,
) handlers.APIResponse {
	t.Helper()
	var apiResp handlers.APIResponse
	if err := json.NewDecoder(resp.Body).Decode(&apiResp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	return apiResp
}

// --- Signup tests ---

func TestAuth_Signup_Success(t *testing.T) {
	cog := &handlerMockCognito{
		signUpFn: func(
			_ context.Context, _, _, _, _ string,
		) (string, error) {
			return "sub-new-user", nil
		},
	}
	store := &handlerMockAccountStore{
		createFn: func(
			_ context.Context, p repository.CreateAccountParams,
		) (*models.Account, error) {
			return &models.Account{
				ID:         "acct-1",
				Email:      p.Email,
				CognitoSub: p.CognitoSub,
				CreatedAt:  time.Now(),
				UpdatedAt:  time.Now(),
			}, nil
		},
	}

	srv := setupAuthTestServer(t, cog, store)
	defer srv.Close()

	resp := postJSON(t, srv.URL+"/api/v1/auth/signup", map[string]string{
		"email":      "new@example.com",
		"password":   "P@ssw0rd!",
		"first_name": "Jane",
		"last_name":  "Doe",
	})
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("expected 201, got %d", resp.StatusCode)
	}

	apiResp := decodeResponse(t, resp)
	if apiResp.Error != nil {
		t.Fatalf("expected nil error, got %+v", apiResp.Error)
	}

	data, ok := apiResp.Data.(map[string]any)
	if !ok {
		t.Fatal("expected data to be a map")
	}
	if data["account_id"] != "acct-1" {
		t.Errorf("expected acct-1, got %v", data["account_id"])
	}
}

func TestAuth_Signup_DuplicateEmail(t *testing.T) {
	cog := &handlerMockCognito{
		signUpFn: func(
			_ context.Context, _, _, _, _ string,
		) (string, error) {
			return "", auth.ErrUserExists
		},
	}

	srv := setupAuthTestServer(t, cog, noopAccountStore())
	defer srv.Close()

	resp := postJSON(t, srv.URL+"/api/v1/auth/signup", map[string]string{
		"email":      "existing@example.com",
		"password":   "P@ssw0rd!",
		"first_name": "Jane",
		"last_name":  "Doe",
	})
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("expected 409, got %d", resp.StatusCode)
	}
}

func TestAuth_Signup_ValidationErrors(t *testing.T) {
	srv := setupAuthTestServer(t, nil, noopAccountStore())
	defer srv.Close()

	resp := postJSON(t, srv.URL+"/api/v1/auth/signup", map[string]string{
		"email": "",
	})
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", resp.StatusCode)
	}

	apiResp := decodeResponse(t, resp)
	if apiResp.Error == nil {
		t.Fatal("expected error in response")
	}
	if apiResp.Error.Code != "VALIDATION_ERROR" {
		t.Errorf(
			"expected VALIDATION_ERROR, got %s", apiResp.Error.Code,
		)
	}
}

// --- ConfirmSignup tests ---

func TestAuth_ConfirmSignup_Success(t *testing.T) {
	cog := &handlerMockCognito{
		confirmFn: func(_ context.Context, _, _ string) error {
			return nil
		},
	}
	store := &handlerMockAccountStore{
		verifyEmailFn: func(_ context.Context, _ string) error {
			return nil
		},
	}

	srv := setupAuthTestServer(t, cog, store)
	defer srv.Close()

	resp := postJSON(t,
		srv.URL+"/api/v1/auth/confirm-signup",
		map[string]string{
			"email": "jane@example.com",
			"code":  "123456",
		},
	)
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
}

func TestAuth_ConfirmSignup_InvalidCode(t *testing.T) {
	cog := &handlerMockCognito{
		confirmFn: func(_ context.Context, _, _ string) error {
			return auth.ErrCodeMismatch
		},
	}

	srv := setupAuthTestServer(t, cog, noopAccountStore())
	defer srv.Close()

	resp := postJSON(t,
		srv.URL+"/api/v1/auth/confirm-signup",
		map[string]string{
			"email": "jane@example.com",
			"code":  "000000",
		},
	)
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", resp.StatusCode)
	}

	apiResp := decodeResponse(t, resp)
	if apiResp.Error == nil || apiResp.Error.Code != "INVALID_CODE" {
		t.Errorf("expected INVALID_CODE, got %+v", apiResp.Error)
	}
}

// --- Login tests ---

func TestAuth_Login_Success(t *testing.T) {
	idToken := handlerFakeIDToken("sub-1", "jane@example.com")
	cog := &handlerMockCognito{
		loginFn: func(
			_ context.Context, _, _ string,
		) (*auth.Result, error) {
			return &auth.Result{
				AccessToken:  "access-tok",
				IDToken:      idToken,
				RefreshToken: "refresh-tok",
				ExpiresIn:    3600,
			}, nil
		},
	}

	srv := setupAuthTestServer(t, cog, noopAccountStore())
	defer srv.Close()

	resp := postJSON(t, srv.URL+"/api/v1/auth/login", map[string]string{
		"email":    "jane@example.com",
		"password": "P@ssw0rd!",
	})
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}

	apiResp := decodeResponse(t, resp)
	data, ok := apiResp.Data.(map[string]any)
	if !ok {
		t.Fatal("expected data to be a map")
	}
	if data["access_token"] != "access-tok" {
		t.Errorf("expected access-tok, got %v", data["access_token"])
	}
	if data["refresh_token"] != "refresh-tok" {
		t.Errorf(
			"expected refresh-tok, got %v", data["refresh_token"],
		)
	}
}

func TestAuth_Login_WrongPassword(t *testing.T) {
	cog := &handlerMockCognito{
		loginFn: func(
			_ context.Context, _, _ string,
		) (*auth.Result, error) {
			return nil, auth.ErrNotAuthorized
		},
	}

	srv := setupAuthTestServer(t, cog, noopAccountStore())
	defer srv.Close()

	resp := postJSON(t, srv.URL+"/api/v1/auth/login", map[string]string{
		"email":    "jane@example.com",
		"password": "wrong",
	})
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", resp.StatusCode)
	}
}

func TestAuth_Login_UserNotConfirmed(t *testing.T) {
	cog := &handlerMockCognito{
		loginFn: func(
			_ context.Context, _, _ string,
		) (*auth.Result, error) {
			return nil, auth.ErrUserNotConfirmed
		},
	}

	srv := setupAuthTestServer(t, cog, noopAccountStore())
	defer srv.Close()

	resp := postJSON(t, srv.URL+"/api/v1/auth/login", map[string]string{
		"email":    "unconfirmed@example.com",
		"password": "P@ssw0rd!",
	})
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("expected 403, got %d", resp.StatusCode)
	}
	apiResp := decodeResponse(t, resp)
	if apiResp.Error == nil || apiResp.Error.Code != "USER_NOT_CONFIRMED" {
		t.Fatalf("expected USER_NOT_CONFIRMED, got %+v", apiResp.Error)
	}
}

func TestAuth_ResendConfirmation_Success(t *testing.T) {
	var resendEmail string
	cog := &handlerMockCognito{
		resendFn: func(_ context.Context, email string) error {
			resendEmail = email
			return nil
		},
	}

	srv := setupAuthTestServer(t, cog, noopAccountStore())
	defer srv.Close()

	resp := postJSON(t, srv.URL+"/api/v1/auth/resend-confirmation", map[string]string{
		"email": "resume@example.com",
	})
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	if resendEmail != "resume@example.com" {
		t.Errorf("expected resend for resume@example.com, got %q", resendEmail)
	}
}

func TestAuth_ForgotPassword_NoAccount(t *testing.T) {
	cog := &handlerMockCognito{
		forgotPasswordFn: func(_ context.Context, _ string) error {
			return auth.ErrUserNotFound
		},
	}
	srv := setupAuthTestServer(t, cog, noopAccountStore())
	defer srv.Close()

	resp := postJSON(t, srv.URL+"/api/v1/auth/forgot-password", map[string]string{
		"email": "missing@example.com",
	})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", resp.StatusCode)
	}
}

func TestAuth_ForgotPasswordConfirm_Success(t *testing.T) {
	var gotEmail, gotCode, gotPw string
	cog := &handlerMockCognito{
		confirmForgotPasswordFn: func(_ context.Context, email, code, newPw string) error {
			gotEmail, gotCode, gotPw = email, code, newPw
			return nil
		},
	}
	srv := setupAuthTestServer(t, cog, noopAccountStore())
	defer srv.Close()

	resp := postJSON(t, srv.URL+"/api/v1/auth/forgot-password/confirm", map[string]string{
		"email":            "u@example.com",
		"code":             "123456",
		"new_password":     "abcdefg",
		"confirm_password": "abcdefg",
	})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	if gotEmail != "u@example.com" || gotCode != "123456" || gotPw != "abcdefg" {
		t.Fatalf("unexpected args: %q %q %q", gotEmail, gotCode, gotPw)
	}
}

// --- Logout tests ---

func TestAuth_Logout_Success(t *testing.T) {
	cog := &handlerMockCognito{
		logoutFn: func(_ context.Context, _ string) error {
			return nil
		},
	}

	srv := setupAuthTestServer(t, cog, noopAccountStore())
	defer srv.Close()

	req, err := http.NewRequest(
		http.MethodPost,
		srv.URL+"/api/v1/auth/logout", nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer some-access-token")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST /logout failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}

	apiResp := decodeResponse(t, resp)
	data, ok := apiResp.Data.(map[string]any)
	if !ok {
		t.Fatal("expected data to be a map")
	}
	if data["message"] != "logged out" {
		t.Errorf(
			"expected 'logged out', got %v", data["message"],
		)
	}
}

func TestAuth_Logout_NoToken(t *testing.T) {
	srv := setupAuthTestServer(t, nil, noopAccountStore())
	defer srv.Close()

	req, err := http.NewRequest(
		http.MethodPost,
		srv.URL+"/api/v1/auth/logout", nil,
	)
	if err != nil {
		t.Fatal(err)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST /logout failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", resp.StatusCode)
	}
}
