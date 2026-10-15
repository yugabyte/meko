package handlers_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
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
	completeNewPasswordFn   func(ctx context.Context, email, newPassword, session, firstName, lastName string) (*auth.Result, error)
	logoutFn                func(ctx context.Context, accessToken string) error
	changePasswordFn        func(ctx context.Context, accessToken, previousPassword, proposedPassword string) error
	getUserStatusFn         func(ctx context.Context, email string) (auth.UserStatus, error)
	resetTempPasswordFn     func(ctx context.Context, email, temporaryPassword string) error
	resendInviteFn          func(ctx context.Context, email string) error
	setPermanentPasswordFn  func(ctx context.Context, email, password string) error
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

func (m *handlerMockCognito) GetUserStatus(ctx context.Context, email string) (auth.UserStatus, error) {
	if m.getUserStatusFn != nil {
		return m.getUserStatusFn(ctx, email)
	}
	return "", nil
}

func (m *handlerMockCognito) ResetTempPassword(ctx context.Context, email, temporaryPassword string) error {
	if m.resetTempPasswordFn != nil {
		return m.resetTempPasswordFn(ctx, email, temporaryPassword)
	}
	return nil
}

func (m *handlerMockCognito) ResendInvite(ctx context.Context, email string) error {
	if m.resendInviteFn != nil {
		return m.resendInviteFn(ctx, email)
	}
	return nil
}

func (m *handlerMockCognito) SetPermanentPassword(ctx context.Context, email, password string) error {
	if m.setPermanentPasswordFn != nil {
		return m.setPermanentPasswordFn(ctx, email, password)
	}
	return nil
}

// handlerResetStore is a minimal in-memory service.PasswordResetStore.
type handlerResetStore struct {
	rows []*models.PasswordResetRequest
}

func (f *handlerResetStore) CountResetRequestsSince(_ context.Context, email string, _ time.Time) (int, error) {
	n := 0
	for _, r := range f.rows {
		if r.Email == email {
			n++
		}
	}
	return n, nil
}

func (f *handlerResetStore) RecordResetRequest(_ context.Context, email string) (string, error) {
	id := fmt.Sprintf("req-%d", len(f.rows)+1)
	f.rows = append(f.rows, &models.PasswordResetRequest{ID: id, Email: email, RequestedAt: time.Now()})
	return id, nil
}

func (f *handlerResetStore) AttachResetCode(_ context.Context, _, id, codeHash string, expiresAt time.Time) error {
	for _, r := range f.rows {
		if r.ID == id {
			r.CodeHash, r.ExpiresAt = codeHash, &expiresAt
		}
	}
	return nil
}

func (f *handlerResetStore) LatestResetRequest(_ context.Context, email string) (*models.PasswordResetRequest, error) {
	for i := len(f.rows) - 1; i >= 0; i-- {
		if f.rows[i].Email == email {
			return f.rows[i], nil
		}
	}
	return nil, nil
}

func (f *handlerResetStore) ReserveResetAttempt(context.Context, string, string, int) (bool, error) {
	return true, nil
}

func (f *handlerResetStore) ClaimResetCode(context.Context, string, string) (bool, error) {
	return true, nil
}

func (f *handlerResetStore) UnclaimResetCode(context.Context, string, string) error {
	return nil
}

func (f *handlerResetStore) DeleteResetRequest(_ context.Context, _, id string) error {
	for i, r := range f.rows {
		if r.ID == id {
			f.rows = append(f.rows[:i], f.rows[i+1:]...)
			return nil
		}
	}
	return nil
}

// handlerMockPasswordSetupEmailer counts password-setup emails and returns err.
type handlerMockPasswordSetupEmailer struct {
	err  error
	sent int
}

func (m *handlerMockPasswordSetupEmailer) SendPasswordSetupEmail(
	_ context.Context, _ service.PasswordSetupEmailParams,
) error {
	m.sent++
	return m.err
}

func (m *handlerMockCognito) Login(
	ctx context.Context, email, password string,
) (*auth.Result, error) {
	return m.loginFn(ctx, email, password)
}

func (m *handlerMockCognito) CompleteNewPassword(
	ctx context.Context,
	email, newPassword, session, firstName, lastName string,
) (*auth.Result, error) {
	if m.completeNewPasswordFn != nil {
		return m.completeNewPasswordFn(
			ctx, email, newPassword, session, firstName, lastName,
		)
	}
	return nil, nil
}

func (m *handlerMockCognito) Logout(
	ctx context.Context, accessToken string,
) error {
	return m.logoutFn(ctx, accessToken)
}

func (m *handlerMockCognito) ChangePassword(
	ctx context.Context, accessToken, previousPassword, proposedPassword string,
) error {
	if m.changePasswordFn != nil {
		return m.changePasswordFn(ctx, accessToken, previousPassword, proposedPassword)
	}
	return nil
}

func (m *handlerMockCognito) UpdateUserName(
	_ context.Context, _, _, _ string,
) error {
	return nil
}

func (m *handlerMockCognito) UpdateEmailAttribute(
	_ context.Context, _, _ string,
) error {
	return nil
}

func (m *handlerMockCognito) SendEmailAttributeCode(
	_ context.Context, _ string,
) error {
	return nil
}

func (m *handlerMockCognito) VerifyEmailAttribute(
	_ context.Context, _, _ string,
) error {
	return nil
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

func (m *handlerMockAccountStore) GetByID(
	_ context.Context, _ string,
) (*models.Account, error) {
	return nil, nil
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

func (m *handlerMockAccountStore) UpdateProfile(
	_ context.Context, _, _, _ string,
) error {
	return nil
}

func (m *handlerMockAccountStore) EmailInUse(
	_ context.Context, _, _ string,
) (bool, error) {
	return false, nil
}

func (m *handlerMockAccountStore) UpdateEmail(
	_ context.Context, _, _ string,
) error {
	return nil
}

func (m *handlerMockAccountStore) MarkEmailVerified(
	_ context.Context, _ string,
) error {
	return nil
}

// TestAuth_Me_HasEmailPasswordLogin verifies GET /auth/me derives
// has_email_password_login from the access-token username claim only
// ("<provider>_" => federated / no native password). Missing username
// (e.g. PAT path) fails open to true.
func TestAuth_Me_HasEmailPasswordLogin(t *testing.T) {
	cases := []struct {
		name            string
		cognitoUsername string
		want            bool
	}{
		{"google username hides form", "Google_103162326666714296584", false},
		{"linkedin username hides form", "LinkedIn_abc", false},
		{"github username hides form", "GitHub_42", false},
		{"native username shows form", "c2205626-8c83-4777-80cd-90f8dbd66ad6", true},
		{"email username shows form", "user@example.com", true},
		{"missing username fails open", "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			authH := handlers.NewAuthHandler(
				service.NewAuthService(&handlerMockCognito{}, &handlerMockAccountStore{}),
				nil,
			)

			e := echo.New()
			req := httptest.NewRequest(http.MethodGet, "/api/v1/auth/me", nil)
			rec := httptest.NewRecorder()
			c := e.NewContext(req, rec)
			c.Set(auth.ContextKeyAccount, &models.Account{
				ID:    "acct-1",
				Email: "user@example.com",
			})
			c.Set(auth.ContextKeyCognitoUsername, tc.cognitoUsername)

			if err := authH.Me(c); err != nil {
				t.Fatalf("Me returned error: %v", err)
			}
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200", rec.Code)
			}

			var apiResp handlers.APIResponse
			if err := json.Unmarshal(rec.Body.Bytes(), &apiResp); err != nil {
				t.Fatalf("failed to decode response: %v", err)
			}
			data, ok := apiResp.Data.(map[string]any)
			if !ok {
				t.Fatalf("data is not an object: %T", apiResp.Data)
			}
			got, ok := data["has_email_password_login"].(bool)
			if !ok {
				t.Fatalf("has_email_password_login missing or not a bool: %v",
					data["has_email_password_login"])
			}
			if got != tc.want {
				t.Errorf("has_email_password_login = %v, want %v", got, tc.want)
			}
		})
	}
}

// setupAuthTestServer creates an Echo instance with auth routes wired
// to the provided mock dependencies.
func setupAuthTestServer(
	t *testing.T,
	cog service.CognitoProvider,
	store service.AccountStore,
	opts ...service.AuthServiceOption,
) *httptest.Server {
	t.Helper()

	authSvc := service.NewAuthService(cog, store, opts...)
	// Tests don't exercise /auth/me/usage; pass nil for the usage service.
	authH := handlers.NewAuthHandler(authSvc, nil)

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
	g.POST("/complete-new-password", authH.CompleteNewPassword)
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

func TestAuth_Login_NewPasswordRequiredChallenge(t *testing.T) {
	cog := &handlerMockCognito{
		loginFn: func(
			_ context.Context, _, _ string,
		) (*auth.Result, error) {
			return &auth.Result{
				ChallengeName: service.ChallengeNewPasswordRequired,
				Session:       "session-token-xyz",
			}, nil
		},
	}

	srv := setupAuthTestServer(t, cog, noopAccountStore())
	defer srv.Close()

	resp := postJSON(t, srv.URL+"/api/v1/auth/login", map[string]string{
		"email":    "fcp@example.com",
		"password": "Temp-PW-1!",
	})
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 with challenge payload, got %d", resp.StatusCode)
	}
	for _, cookie := range resp.Cookies() {
		if cookie.Name == "access_token" || cookie.Name == "id_token" || cookie.Name == "refresh_token" {
			if cookie.Value != "" {
				t.Errorf("did not expect %s cookie on challenge response, got %q",
					cookie.Name, cookie.Value)
			}
		}
	}

	apiResp := decodeResponse(t, resp)
	data, ok := apiResp.Data.(map[string]any)
	if !ok {
		t.Fatal("expected data to be a map")
	}
	if data["challenge"] != service.ChallengeNewPasswordRequired {
		t.Errorf("expected challenge=%s, got %v",
			service.ChallengeNewPasswordRequired, data["challenge"])
	}
	if data["session"] != "session-token-xyz" {
		t.Errorf("expected session echoed back, got %v", data["session"])
	}
	if _, hasAccess := data["access_token"]; hasAccess {
		t.Error("expected no access_token field on challenge response")
	}
}

func TestAuth_CompleteNewPassword_Success(t *testing.T) {
	idToken := handlerFakeIDToken("sub-fcp-1", "fcp@example.com")
	var gotFirst, gotLast string
	cog := &handlerMockCognito{
		completeNewPasswordFn: func(
			_ context.Context, _, _, _, firstName, lastName string,
		) (*auth.Result, error) {
			gotFirst, gotLast = firstName, lastName
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

	resp := postJSON(t,
		srv.URL+"/api/v1/auth/complete-new-password",
		map[string]string{
			"email":            "fcp@example.com",
			"new_password":     "NewP@ssw0rd!",
			"confirm_password": "NewP@ssw0rd!",
			"session":          "session-token-xyz",
			"first_name":       "Force",
			"last_name":        "Change",
		},
	)
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
	if gotFirst != "Force" || gotLast != "Change" {
		t.Errorf("expected names forwarded to cognito, got first=%q last=%q",
			gotFirst, gotLast)
	}
}

func TestAuth_CompleteNewPassword_PasswordMismatch(t *testing.T) {
	srv := setupAuthTestServer(t, &handlerMockCognito{}, noopAccountStore())
	defer srv.Close()

	resp := postJSON(t,
		srv.URL+"/api/v1/auth/complete-new-password",
		map[string]string{
			"email":            "fcp@example.com",
			"new_password":     "abcdefg",
			"confirm_password": "different",
			"session":          "s",
		},
	)
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", resp.StatusCode)
	}
	apiResp := decodeResponse(t, resp)
	if apiResp.Error == nil || apiResp.Error.Code != "VALIDATION_ERROR" {
		t.Errorf("expected VALIDATION_ERROR, got %+v", apiResp.Error)
	}
}

func TestAuth_CompleteNewPassword_InvalidPassword(t *testing.T) {
	cog := &handlerMockCognito{
		completeNewPasswordFn: func(
			_ context.Context, _, _, _, _, _ string,
		) (*auth.Result, error) {
			return nil, auth.ErrInvalidPassword
		},
	}
	srv := setupAuthTestServer(t, cog, noopAccountStore())
	defer srv.Close()

	resp := postJSON(t,
		srv.URL+"/api/v1/auth/complete-new-password",
		map[string]string{
			"email":            "fcp@example.com",
			"new_password":     "abcdefg",
			"confirm_password": "abcdefg",
			"session":          "s",
		},
	)
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", resp.StatusCode)
	}
	apiResp := decodeResponse(t, resp)
	if apiResp.Error == nil || apiResp.Error.Code != "VALIDATION_ERROR" {
		t.Errorf("expected VALIDATION_ERROR for ErrInvalidPassword, got %+v",
			apiResp.Error)
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

// forceChangeHandlerCognito fails ForgotPassword the way Cognito does for an
// invitee who never finished first sign-in.
func forceChangeHandlerCognito() *handlerMockCognito {
	return &handlerMockCognito{
		forgotPasswordFn: func(_ context.Context, _ string) error {
			return auth.ErrPasswordResetNotAllowed
		},
		getUserStatusFn: func(_ context.Context, _ string) (auth.UserStatus, error) {
			return auth.UserStatusForceChangePassword, nil
		},
	}
}

//nolint:gosec // a response message, not a credential
const forgotPasswordMessage = "If an account exists, a verification code has been sent."

func forgotPasswordMessageOf(t *testing.T, resp *http.Response) string {
	t.Helper()
	data, ok := decodeResponse(t, resp).Data.(map[string]any)
	if !ok {
		t.Fatal("expected a JSON object in data")
	}
	if _, has := data["next_step"]; has {
		t.Error("the response must not reveal account state")
	}
	msg, ok := data["message"].(string)
	if !ok {
		t.Fatal("expected a message string")
	}
	return msg
}

func TestAuth_ForgotPassword_ForceChange_SameResponseAndMekoCode(t *testing.T) {
	mailer := &handlerMockPasswordSetupEmailer{}
	srv := setupAuthTestServer(t, forceChangeHandlerCognito(), noopAccountStore(),
		service.WithPasswordSetupEmailer(mailer), service.WithPasswordResetStore(&handlerResetStore{}))
	defer srv.Close()

	resp := postJSON(t, srv.URL+"/api/v1/auth/forgot-password", map[string]string{
		"email": "invited@example.com",
	})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	if msg := forgotPasswordMessageOf(t, resp); msg != forgotPasswordMessage {
		t.Errorf("expected the standard message, got %q", msg)
	}
	if mailer.sent != 1 {
		t.Errorf("expected one reset code email, got %d", mailer.sent)
	}
}

func TestAuth_ForgotPassword_ForceChange_EmailFails_SameResponse(t *testing.T) {
	mailer := &handlerMockPasswordSetupEmailer{err: errors.New("MessageRejected: RequestId: abc-123")}
	srv := setupAuthTestServer(t, forceChangeHandlerCognito(), noopAccountStore(),
		service.WithPasswordSetupEmailer(mailer), service.WithPasswordResetStore(&handlerResetStore{}))
	defer srv.Close()

	resp := postJSON(t, srv.URL+"/api/v1/auth/forgot-password", map[string]string{
		"email": "invited@example.com",
	})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected the uniform 200, got %d", resp.StatusCode)
	}
	if msg := forgotPasswordMessageOf(t, resp); msg != forgotPasswordMessage {
		t.Errorf("expected the standard message, got %q", msg)
	}
}

func TestAuth_ForgotPassword_ForceChange_NoSES_SameResponse(t *testing.T) {
	srv := setupAuthTestServer(t, forceChangeHandlerCognito(), noopAccountStore(),
		service.WithPasswordResetStore(&handlerResetStore{}))
	defer srv.Close()

	resp := postJSON(t, srv.URL+"/api/v1/auth/forgot-password", map[string]string{
		"email": "invited@example.com",
	})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected the uniform 200 without SES, got %d", resp.StatusCode)
	}
}

func TestAuth_ForgotPassword_PerEmailLimit_Returns429(t *testing.T) {
	store := &handlerResetStore{}
	srv := setupAuthTestServer(t, &handlerMockCognito{}, noopAccountStore(), service.WithPasswordResetStore(store))
	defer srv.Close()

	for i := 0; i < 6; i++ {
		resp := postJSON(t, srv.URL+"/api/v1/auth/forgot-password", map[string]string{
			"email": "user@example.com",
		})
		resp.Body.Close()
		want := http.StatusOK
		if i == 5 {
			want = http.StatusTooManyRequests
		}
		if resp.StatusCode != want {
			t.Fatalf("request %d: expected %d, got %d", i+1, want, resp.StatusCode)
		}
	}
}

func TestAuth_ForgotPassword_NoAccount_SameResponse(t *testing.T) {
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
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 (same as an existing account), got %d", resp.StatusCode)
	}
	if msg := forgotPasswordMessageOf(t, resp); msg != forgotPasswordMessage {
		t.Errorf("expected the standard message, got %q", msg)
	}
}

func TestAuth_Login_ExpiredTempPassword_Returns401WithCode(t *testing.T) {
	resetCalled := false
	cog := &handlerMockCognito{
		loginFn: func(_ context.Context, _, _ string) (*auth.Result, error) {
			return nil, auth.ErrTempPasswordExpired
		},
		getUserStatusFn: func(_ context.Context, _ string) (auth.UserStatus, error) {
			return auth.UserStatusForceChangePassword, nil
		},
		resetTempPasswordFn: func(_ context.Context, _, _ string) error {
			resetCalled = true
			return nil
		},
	}
	srv := setupAuthTestServer(t, cog, noopAccountStore(),
		service.WithPasswordSetupEmailer(&handlerMockPasswordSetupEmailer{}))
	defer srv.Close()

	resp := postJSON(t, srv.URL+"/api/v1/auth/login", map[string]string{
		"email":    "pending@example.com",
		"password": "Expired-1!",
	})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", resp.StatusCode)
	}
	apiResp := decodeResponse(t, resp)
	if apiResp.Error == nil || apiResp.Error.Code != "INVITE_PASSWORD_EXPIRED" {
		t.Errorf("expected error code INVITE_PASSWORD_EXPIRED, got %+v", apiResp.Error)
	}
	if !resetCalled {
		t.Error("expected ResetTempPassword to be called for the expired-invite rescue")
	}
}

func TestAuth_Login_ExpiredTempPassword_CognitoRefusesEmailedPassword_Returns503(t *testing.T) {
	// The email went out but Cognito refused the new password. The user must
	// be told to retry (a static 503), not "invalid credentials", and never
	// the invite-expired code.
	cog := &handlerMockCognito{
		loginFn: func(_ context.Context, _, _ string) (*auth.Result, error) {
			return nil, auth.ErrTempPasswordExpired
		},
		getUserStatusFn: func(_ context.Context, _ string) (auth.UserStatus, error) {
			return auth.UserStatusForceChangePassword, nil
		},
		resetTempPasswordFn: func(_ context.Context, _, _ string) error {
			return auth.ErrTooManyRequests
		},
	}
	srv := setupAuthTestServer(t, cog, noopAccountStore(),
		service.WithPasswordSetupEmailer(&handlerMockPasswordSetupEmailer{}))
	defer srv.Close()

	resp := postJSON(t, srv.URL+"/api/v1/auth/login", map[string]string{
		"email":    "pending@example.com",
		"password": "Expired-1!",
	})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("expected 503, got %d", resp.StatusCode)
	}
	apiResp := decodeResponse(t, resp)
	if apiResp.Error == nil || apiResp.Error.Code == "INVITE_PASSWORD_EXPIRED" {
		t.Errorf("expected a static 503 (not the invite-expired code), got %+v", apiResp.Error)
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
		"new_password":     "Abcdef1!",
		"confirm_password": "Abcdef1!",
	})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	if gotEmail != "u@example.com" || gotCode != "123456" || gotPw != "Abcdef1!" {
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
