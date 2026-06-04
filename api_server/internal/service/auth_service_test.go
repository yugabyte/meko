package service_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"meko/api_server/internal/auth"
	"meko/api_server/internal/models"
	"meko/api_server/internal/repository"
	"meko/api_server/internal/service"
)

// fakeIDToken builds a JWT-shaped string (header.payload.signature)
// with the given claims in the payload. No real signature is needed
// because the service parses without verification.
func fakeIDToken(sub, email, given, family string) string {
	header := base64.RawURLEncoding.EncodeToString(
		[]byte(`{"alg":"RS256","typ":"JWT"}`),
	)
	claims := map[string]string{
		"sub":         sub,
		"email":       email,
		"given_name":  given,
		"family_name": family,
	}
	payload, _ := json.Marshal(claims)
	payloadB64 := base64.RawURLEncoding.EncodeToString(payload)
	return header + "." + payloadB64 + ".fake-sig"
}

// --- mock CognitoProvider ---

type mockCognito struct {
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
	resendInviteFn          func(ctx context.Context, email string) error
}

func (m *mockCognito) SignUp(
	ctx context.Context,
	email, password, firstName, lastName string,
) (string, error) {
	return m.signUpFn(ctx, email, password, firstName, lastName)
}

func (m *mockCognito) ConfirmSignUp(
	ctx context.Context, email, code string,
) error {
	return m.confirmFn(ctx, email, code)
}

func (m *mockCognito) ResendConfirmationCode(
	ctx context.Context, email string,
) error {
	if m.resendFn != nil {
		return m.resendFn(ctx, email)
	}
	return nil
}

func (m *mockCognito) ForgotPassword(ctx context.Context, email string) error {
	if m.forgotPasswordFn != nil {
		return m.forgotPasswordFn(ctx, email)
	}
	return nil
}

func (m *mockCognito) ConfirmForgotPassword(
	ctx context.Context, email, code, newPassword string,
) error {
	if m.confirmForgotPasswordFn != nil {
		return m.confirmForgotPasswordFn(ctx, email, code, newPassword)
	}
	return nil
}

func (m *mockCognito) GetUserStatus(ctx context.Context, email string) (auth.UserStatus, error) {
	if m.getUserStatusFn != nil {
		return m.getUserStatusFn(ctx, email)
	}
	return "", nil
}

func (m *mockCognito) ResendInvite(ctx context.Context, email string) error {
	if m.resendInviteFn != nil {
		return m.resendInviteFn(ctx, email)
	}
	return nil
}

func (m *mockCognito) Login(
	ctx context.Context, email, password string,
) (*auth.Result, error) {
	return m.loginFn(ctx, email, password)
}

func (m *mockCognito) CompleteNewPassword(
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

func (m *mockCognito) Logout(
	ctx context.Context, accessToken string,
) error {
	return m.logoutFn(ctx, accessToken)
}

func (m *mockCognito) ChangePassword(
	ctx context.Context, accessToken, previousPassword, proposedPassword string,
) error {
	if m.changePasswordFn != nil {
		return m.changePasswordFn(ctx, accessToken, previousPassword, proposedPassword)
	}
	return nil
}

// --- mock AccountStore ---

type mockAccountStore struct {
	createFn          func(ctx context.Context, params repository.CreateAccountParams) (*models.Account, error)
	getByCognitoSubFn func(ctx context.Context, sub string) (*models.Account, error)
	verifyEmailFn     func(ctx context.Context, email string) error
	setOnboardingFn   func(ctx context.Context, accountID string, completed bool) error
}

func (m *mockAccountStore) Create(
	ctx context.Context, params repository.CreateAccountParams,
) (*models.Account, error) {
	return m.createFn(ctx, params)
}

func (m *mockAccountStore) GetByCognitoSub(
	ctx context.Context, sub string,
) (*models.Account, error) {
	return m.getByCognitoSubFn(ctx, sub)
}

func (m *mockAccountStore) VerifyEmail(
	ctx context.Context, email string,
) error {
	return m.verifyEmailFn(ctx, email)
}

func (m *mockAccountStore) SetHasCompletedOnboarding(
	ctx context.Context, accountID string, completed bool,
) error {
	if m.setOnboardingFn == nil {
		return nil
	}
	return m.setOnboardingFn(ctx, accountID, completed)
}

// --- Signup tests ---

func TestAuthService_Signup_Success(t *testing.T) {
	cog := &mockCognito{
		signUpFn: func(
			_ context.Context, _, _, _, _ string,
		) (string, error) {
			return "sub-abc-123", nil
		},
	}
	store := &mockAccountStore{
		createFn: func(
			_ context.Context,
			params repository.CreateAccountParams,
		) (*models.Account, error) {
			return &models.Account{
				ID:         "acct-uuid-1",
				Email:      params.Email,
				CognitoSub: params.CognitoSub,
				CreatedAt:  time.Now(),
				UpdatedAt:  time.Now(),
			}, nil
		},
	}

	svc := service.NewAuthService(cog, store)

	resp, err := svc.Signup(context.Background(), service.SignupRequest{
		Email:     "jane@example.com",
		Password:  "P@ssw0rd!",
		FirstName: "Jane",
		LastName:  "Doe",
	})
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
	if resp.AccountID != "acct-uuid-1" {
		t.Errorf("expected acct-uuid-1, got %s", resp.AccountID)
	}
	if resp.Email != "jane@example.com" {
		t.Errorf("expected jane@example.com, got %s", resp.Email)
	}
}

func TestAuthService_Signup_ValidationError(t *testing.T) {
	svc := service.NewAuthService(nil, nil)

	tests := []struct {
		name string
		req  service.SignupRequest
	}{
		{"empty email", service.SignupRequest{
			Password: "p", FirstName: "A", LastName: "B",
		}},
		{"empty password", service.SignupRequest{
			Email: "a@b.com", FirstName: "A", LastName: "B",
		}},
		{"empty first name", service.SignupRequest{
			Email: "a@b.com", Password: "p", LastName: "B",
		}},
		{"empty last name", service.SignupRequest{
			Email: "a@b.com", Password: "p", FirstName: "A",
		}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := svc.Signup(context.Background(), tc.req)
			if err == nil {
				t.Error("expected validation error, got nil")
			}
		})
	}
}

func TestAuthService_Signup_RejectsWeakPasswordBeforeCognito(t *testing.T) {
	called := false
	cog := &mockCognito{
		signUpFn: func(
			_ context.Context, _, _, _, _ string,
		) (string, error) {
			called = true
			return "sub-should-not-happen", nil
		},
	}
	svc := service.NewAuthService(cog, nil)

	_, err := svc.Signup(context.Background(), service.SignupRequest{
		Email:     "curl@example.com",
		Password:  "lowercase1",
		FirstName: "Curl",
		LastName:  "User",
	})
	if !errors.Is(err, service.ErrPasswordMissingUppercase) {
		t.Fatalf("expected missing uppercase error, got %v", err)
	}
	if called {
		t.Fatal("expected weak password to be rejected before Cognito SignUp")
	}
}

func TestAuthService_Signup_CognitoError(t *testing.T) {
	cog := &mockCognito{
		signUpFn: func(
			_ context.Context, _, _, _, _ string,
		) (string, error) {
			return "", auth.ErrUserExists
		},
	}
	svc := service.NewAuthService(cog, nil)

	_, err := svc.Signup(context.Background(), service.SignupRequest{
		Email:     "existing@example.com",
		Password:  "P@ssw0rd!",
		FirstName: "Jane",
		LastName:  "Doe",
	})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !errors.Is(err, auth.ErrUserExists) {
		t.Errorf("expected ErrUserExists, got %v", err)
	}
}

// --- ConfirmSignup tests ---

func TestAuthService_ConfirmSignup_Success(t *testing.T) {
	cog := &mockCognito{
		confirmFn: func(_ context.Context, _, _ string) error {
			return nil
		},
	}
	store := &mockAccountStore{
		verifyEmailFn: func(_ context.Context, _ string) error {
			return nil
		},
	}
	svc := service.NewAuthService(cog, store)

	err := svc.ConfirmSignup(
		context.Background(),
		service.ConfirmSignupRequest{
			Email: "jane@example.com",
			Code:  "123456",
		},
	)
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
}

// --- ForgotPasswordSendCode tests ---

func TestAuthService_ForgotPasswordSendCode_Success(t *testing.T) {
	var forgotEmail string
	cog := &mockCognito{
		forgotPasswordFn: func(_ context.Context, email string) error {
			forgotEmail = email
			return nil
		},
	}
	svc := service.NewAuthService(cog, &mockAccountStore{})

	if err := svc.ForgotPasswordSendCode(context.Background(), service.ForgotPasswordStartRequest{
		Email: "  user@example.com  ", // trimmed before use
	}); err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
	if forgotEmail != "user@example.com" {
		t.Errorf("expected ForgotPassword for trimmed email, got %q", forgotEmail)
	}
}

func TestAuthService_ForgotPasswordSendCode_NoAccount(t *testing.T) {
	cog := &mockCognito{
		forgotPasswordFn: func(_ context.Context, _ string) error {
			return auth.ErrUserNotFound
		},
	}
	svc := service.NewAuthService(cog, &mockAccountStore{})

	err := svc.ForgotPasswordSendCode(context.Background(), service.ForgotPasswordStartRequest{
		Email: "missing@example.com",
	})
	if !errors.Is(err, service.ErrForgotPasswordNoAccount) {
		t.Errorf("expected ErrForgotPasswordNoAccount, got %v", err)
	}
}

// --- Login expired-temp-password rescue tests ---

func TestAuthService_Login_ExpiredTempPassword_RescuesAndSignals(t *testing.T) {
	var resentEmail string
	cog := &mockCognito{
		loginFn: func(_ context.Context, _, _ string) (*auth.Result, error) {
			return nil, auth.ErrTempPasswordExpired
		},
		getUserStatusFn: func(_ context.Context, _ string) (auth.UserStatus, error) {
			return auth.UserStatusForceChangePassword, nil
		},
		resendInviteFn: func(_ context.Context, email string) error {
			resentEmail = email
			return nil
		},
	}
	svc := service.NewAuthService(cog, &mockAccountStore{})

	_, err := svc.Login(context.Background(), service.LoginRequest{
		Email:    "pending@example.com",
		Password: "Expired-1!",
	})
	if !errors.Is(err, service.ErrInvitePasswordExpired) {
		t.Errorf("expected ErrInvitePasswordExpired, got %v", err)
	}
	if resentEmail != "pending@example.com" {
		t.Errorf("expected ResendInvite for pending@example.com, got %q", resentEmail)
	}
}

func TestAuthService_Login_ExpiredTempPassword_NotForceChange_FallsBack(t *testing.T) {
	// Defensive: if the account isn't actually FORCE_CHANGE_PASSWORD, do not
	// resend anything and fall back to a normal failed-login.
	resendCalled := false
	cog := &mockCognito{
		loginFn: func(_ context.Context, _, _ string) (*auth.Result, error) {
			return nil, auth.ErrTempPasswordExpired
		},
		getUserStatusFn: func(_ context.Context, _ string) (auth.UserStatus, error) {
			return auth.UserStatusConfirmed, nil
		},
		resendInviteFn: func(_ context.Context, _ string) error {
			resendCalled = true
			return nil
		},
	}
	svc := service.NewAuthService(cog, &mockAccountStore{})

	_, err := svc.Login(context.Background(), service.LoginRequest{
		Email:    "confirmed@example.com",
		Password: "wrong",
	})
	if errors.Is(err, service.ErrInvitePasswordExpired) {
		t.Error("must not signal invite-expired when account is not FORCE_CHANGE_PASSWORD")
	}
	if err == nil {
		t.Error("expected a login error")
	}
	if resendCalled {
		t.Error("ResendInvite must not be called for a non-force-change account")
	}
}

func TestAuthService_Login_ExpiredTempPassword_ResendError_FallsBack(t *testing.T) {
	cog := &mockCognito{
		loginFn: func(_ context.Context, _, _ string) (*auth.Result, error) {
			return nil, auth.ErrTempPasswordExpired
		},
		getUserStatusFn: func(_ context.Context, _ string) (auth.UserStatus, error) {
			return auth.UserStatusForceChangePassword, nil
		},
		resendInviteFn: func(_ context.Context, _ string) error {
			return auth.ErrTooManyRequests
		},
	}
	svc := service.NewAuthService(cog, &mockAccountStore{})

	_, err := svc.Login(context.Background(), service.LoginRequest{
		Email:    "pending@example.com",
		Password: "Expired-1!",
	})
	if errors.Is(err, service.ErrInvitePasswordExpired) {
		t.Error("must not signal invite-expired when the rescue resend failed")
	}
	if err == nil {
		t.Error("expected a login error")
	}
}

func TestAuthService_Login_WrongPassword_NoRescue(t *testing.T) {
	// A plain wrong password (not expired) must not trigger the rescue.
	statusChecked := false
	cog := &mockCognito{
		loginFn: func(_ context.Context, _, _ string) (*auth.Result, error) {
			return nil, auth.ErrNotAuthorized
		},
		getUserStatusFn: func(_ context.Context, _ string) (auth.UserStatus, error) {
			statusChecked = true
			return auth.UserStatusForceChangePassword, nil
		},
	}
	svc := service.NewAuthService(cog, &mockAccountStore{})

	_, err := svc.Login(context.Background(), service.LoginRequest{
		Email:    "user@example.com",
		Password: "wrong",
	})
	if !errors.Is(err, auth.ErrNotAuthorized) {
		t.Errorf("expected ErrNotAuthorized, got %v", err)
	}
	if statusChecked {
		t.Error("GetUserStatus must not be called for a plain wrong-password failure")
	}
}

// --- Login tests ---

func TestAuthService_Login_Success(t *testing.T) {
	idToken := fakeIDToken(
		"sub-abc-123", "jane@example.com", "Jane", "Doe",
	)

	cog := &mockCognito{
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
	store := &mockAccountStore{
		getByCognitoSubFn: func(
			_ context.Context, _ string,
		) (*models.Account, error) {
			return &models.Account{
				ID:         "acct-uuid-1",
				Email:      "jane@example.com",
				CognitoSub: "sub-abc-123",
			}, nil
		},
	}
	svc := service.NewAuthService(cog, store)

	resp, err := svc.Login(context.Background(), service.LoginRequest{
		Email:    "jane@example.com",
		Password: "P@ssw0rd!",
	})
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
	if resp.AccessToken != "access-tok" {
		t.Errorf("expected access-tok, got %s", resp.AccessToken)
	}
	if resp.ExpiresIn != 3600 {
		t.Errorf("expected 3600, got %d", resp.ExpiresIn)
	}
}

func TestAuthService_Login_JITAccountCreation(t *testing.T) {
	idToken := fakeIDToken(
		"new-sub-456", "newuser@example.com", "New", "User",
	)
	var createdParams repository.CreateAccountParams

	cog := &mockCognito{
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
	store := &mockAccountStore{
		getByCognitoSubFn: func(
			_ context.Context, _ string,
		) (*models.Account, error) {
			return nil, nil
		},
		createFn: func(
			_ context.Context,
			params repository.CreateAccountParams,
		) (*models.Account, error) {
			createdParams = params
			return &models.Account{
				ID:         "new-acct-uuid",
				Email:      params.Email,
				CognitoSub: params.CognitoSub,
				FirstName:  params.FirstName,
				LastName:   params.LastName,
				CreatedAt:  time.Now(),
				UpdatedAt:  time.Now(),
			}, nil
		},
	}
	svc := service.NewAuthService(cog, store)

	resp, err := svc.Login(context.Background(), service.LoginRequest{
		Email:    "newuser@example.com",
		Password: "P@ssw0rd!",
	})
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
	if resp.AccessToken != "access-tok" {
		t.Errorf("expected access-tok, got %s", resp.AccessToken)
	}
	if createdParams.CognitoSub != "new-sub-456" {
		t.Errorf("expected JIT create with sub new-sub-456, got %s",
			createdParams.CognitoSub,
		)
	}
	if createdParams.Email != "newuser@example.com" {
		t.Errorf("expected JIT create with email newuser@example.com, got %s",
			createdParams.Email,
		)
	}
	if createdParams.FirstName != "New" {
		t.Errorf("expected JIT create with first_name New, got %s",
			createdParams.FirstName,
		)
	}
}

func TestAuthService_Login_InvalidCredentials(t *testing.T) {
	cog := &mockCognito{
		loginFn: func(
			_ context.Context, _, _ string,
		) (*auth.Result, error) {
			return nil, auth.ErrNotAuthorized
		},
	}
	store := &mockAccountStore{}
	svc := service.NewAuthService(cog, store)

	_, err := svc.Login(context.Background(), service.LoginRequest{
		Email:    "jane@example.com",
		Password: "wrong",
	})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !errors.Is(err, auth.ErrNotAuthorized) {
		t.Errorf("expected ErrNotAuthorized, got %v", err)
	}
}

func TestAuthService_Login_NewPasswordRequiredChallenge(t *testing.T) {
	cog := &mockCognito{
		loginFn: func(
			_ context.Context, _, _ string,
		) (*auth.Result, error) {
			return &auth.Result{
				ChallengeName: service.ChallengeNewPasswordRequired,
				Session:       "session-token-xyz",
			}, nil
		},
	}
	jitCalls := 0
	store := &mockAccountStore{
		getByCognitoSubFn: func(
			_ context.Context, _ string,
		) (*models.Account, error) {
			jitCalls++
			return nil, nil
		},
		createFn: func(
			_ context.Context,
			_ repository.CreateAccountParams,
		) (*models.Account, error) {
			t.Fatal("Create should not be called when challenge is returned")
			return nil, nil
		},
	}
	svc := service.NewAuthService(cog, store)

	resp, err := svc.Login(context.Background(), service.LoginRequest{
		Email:    "user@example.com",
		Password: "Temp-PW-1!",
	})
	if err != nil {
		t.Fatalf("expected no error on challenge, got %v", err)
	}
	if resp.Challenge != service.ChallengeNewPasswordRequired {
		t.Errorf("expected challenge %s, got %q",
			service.ChallengeNewPasswordRequired, resp.Challenge,
		)
	}
	if resp.Session != "session-token-xyz" {
		t.Errorf("expected Session=session-token-xyz, got %q", resp.Session)
	}
	if resp.Email != "user@example.com" {
		t.Errorf("expected Email echoed back, got %q", resp.Email)
	}
	if resp.AccessToken != "" || resp.IDToken != "" {
		t.Errorf("expected no tokens on challenge, got %+v", resp)
	}
	// EnsureAccountFromIDToken would call GetByCognitoSub; on the
	// challenge branch we must skip JIT entirely.
	if jitCalls != 0 {
		t.Errorf("expected no JIT lookups on challenge branch, got %d", jitCalls)
	}
}

// --- CompleteNewPassword tests ---

func TestAuthService_CompleteNewPassword_Success(t *testing.T) {
	idToken := fakeIDToken(
		"sub-fcp-1", "fcp@example.com", "Force", "Change",
	)
	var gotEmail, gotNewPw, gotSession, gotFirst, gotLast string
	cog := &mockCognito{
		completeNewPasswordFn: func(
			_ context.Context,
			email, newPassword, session, firstName, lastName string,
		) (*auth.Result, error) {
			gotEmail, gotNewPw, gotSession = email, newPassword, session
			gotFirst, gotLast = firstName, lastName
			return &auth.Result{
				AccessToken:  "access-tok",
				IDToken:      idToken,
				RefreshToken: "refresh-tok",
				ExpiresIn:    3600,
			}, nil
		},
	}
	var createdParams repository.CreateAccountParams
	store := &mockAccountStore{
		getByCognitoSubFn: func(
			_ context.Context, _ string,
		) (*models.Account, error) {
			return nil, nil
		},
		createFn: func(
			_ context.Context, params repository.CreateAccountParams,
		) (*models.Account, error) {
			createdParams = params
			return &models.Account{
				ID:         "acct-fcp-1",
				Email:      params.Email,
				CognitoSub: params.CognitoSub,
			}, nil
		},
	}
	svc := service.NewAuthService(cog, store)

	resp, err := svc.CompleteNewPassword(
		context.Background(),
		service.CompleteNewPasswordRequest{
			Email:           "fcp@example.com",
			NewPassword:     "NewP@ssw0rd!",
			ConfirmPassword: "NewP@ssw0rd!",
			Session:         "session-token-xyz",
			FirstName:       "Force",
			LastName:        "Change",
		},
	)
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
	if resp.AccessToken != "access-tok" {
		t.Errorf("expected access-tok, got %s", resp.AccessToken)
	}
	if gotEmail != "fcp@example.com" {
		t.Errorf("expected email forwarded, got %q", gotEmail)
	}
	if gotNewPw != "NewP@ssw0rd!" {
		t.Errorf("expected new password forwarded, got %q", gotNewPw)
	}
	if gotSession != "session-token-xyz" {
		t.Errorf("expected session forwarded, got %q", gotSession)
	}
	if gotFirst != "Force" {
		t.Errorf("expected first name forwarded, got %q", gotFirst)
	}
	if gotLast != "Change" {
		t.Errorf("expected last name forwarded, got %q", gotLast)
	}
	if createdParams.CognitoSub != "sub-fcp-1" {
		t.Errorf("expected JIT account created with sub from id token, got %q",
			createdParams.CognitoSub,
		)
	}
}

func TestAuthService_CompleteNewPassword_ValidationErrors(t *testing.T) {
	svc := service.NewAuthService(nil, nil)

	tests := []struct {
		name    string
		req     service.CompleteNewPasswordRequest
		wantErr error
	}{
		{
			"empty email",
			service.CompleteNewPasswordRequest{
				NewPassword:     "NewP@ssw0rd!",
				ConfirmPassword: "NewP@ssw0rd!",
				Session:         "s",
			},
			nil,
		},
		{
			"empty session",
			service.CompleteNewPasswordRequest{
				Email:           "u@example.com",
				NewPassword:     "NewP@ssw0rd!",
				ConfirmPassword: "NewP@ssw0rd!",
			},
			nil,
		},
		{
			"password too short",
			service.CompleteNewPasswordRequest{
				Email:           "u@example.com",
				NewPassword:     "short",
				ConfirmPassword: "short",
				Session:         "s",
			},
			service.ErrPasswordTooShort,
		},
		{
			"password mismatch",
			service.CompleteNewPasswordRequest{
				Email:           "u@example.com",
				NewPassword:     "NewP@ssw0rd!",
				ConfirmPassword: "different",
				Session:         "s",
			},
			service.ErrPasswordMismatch,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := svc.CompleteNewPassword(context.Background(), tc.req)
			if err == nil {
				t.Fatal("expected validation error, got nil")
			}
			if tc.wantErr != nil && !errors.Is(err, tc.wantErr) {
				t.Errorf("expected %v, got %v", tc.wantErr, err)
			}
		})
	}
}

func TestAuthService_CompleteNewPassword_CognitoError(t *testing.T) {
	cog := &mockCognito{
		completeNewPasswordFn: func(
			_ context.Context, _, _, _, _, _ string,
		) (*auth.Result, error) {
			return nil, auth.ErrInvalidPassword
		},
	}
	svc := service.NewAuthService(cog, &mockAccountStore{})

	_, err := svc.CompleteNewPassword(
		context.Background(),
		service.CompleteNewPasswordRequest{
			Email:           "u@example.com",
			NewPassword:     "NewP@ssw0rd!",
			ConfirmPassword: "NewP@ssw0rd!",
			Session:         "s",
		},
	)
	if !errors.Is(err, auth.ErrInvalidPassword) {
		t.Errorf("expected ErrInvalidPassword, got %v", err)
	}
}

func TestAuthService_ForgotPasswordConfirm_RejectsTooLongPasswordBeforeCognito(t *testing.T) {
	called := false
	cog := &mockCognito{
		confirmForgotPasswordFn: func(
			_ context.Context, _, _, _ string,
		) error {
			called = true
			return nil
		},
	}
	svc := service.NewAuthService(cog, nil)
	tooLongPassword := "Aa1!" + strings.Repeat("x", 253)

	err := svc.ConfirmForgotPasswordReset(
		context.Background(),
		service.ForgotPasswordConfirmRequest{
			Email:           "curl@example.com",
			Code:            "123456",
			NewPassword:     tooLongPassword,
			ConfirmPassword: tooLongPassword,
		},
	)
	if !errors.Is(err, service.ErrPasswordTooLong) {
		t.Fatalf("expected too-long password error, got %v", err)
	}
	if called {
		t.Fatal("expected too-long password to be rejected before Cognito reset")
	}
}

// --- Logout tests ---

func TestAuthService_Logout_Success(t *testing.T) {
	cog := &mockCognito{
		logoutFn: func(_ context.Context, _ string) error {
			return nil
		},
	}
	store := &mockAccountStore{}
	svc := service.NewAuthService(cog, store)

	err := svc.Logout(context.Background(), "some-access-token")
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
}

// --- Provisioner spy ---

type spyProvisioner struct {
	mu        sync.Mutex
	wg        sync.WaitGroup
	called    bool
	accountID string
}

func newSpyProvisioner() *spyProvisioner {
	s := &spyProvisioner{}
	s.wg.Add(1)
	return s
}

func (s *spyProvisioner) ProvisionNewAccount(_ context.Context, accountID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.called = true
	s.accountID = accountID
	s.wg.Done()
}

// --- EnsureAccountFromIDToken provisioning tests ---

func TestEnsureAccountFromIDToken_NewAccount_TriggersProvisioning(t *testing.T) {
	idToken := fakeIDToken(
		"new-sub-prov", "prov@example.com", "Prov", "User",
	)
	store := &mockAccountStore{
		getByCognitoSubFn: func(_ context.Context, _ string) (*models.Account, error) {
			return nil, nil
		},
		createFn: func(_ context.Context, params repository.CreateAccountParams) (*models.Account, error) {
			return &models.Account{
				ID:         "acct-prov-1",
				Email:      params.Email,
				CognitoSub: params.CognitoSub,
			}, nil
		},
	}
	prov := newSpyProvisioner()

	svc := service.NewAuthService(nil, store, service.WithProvisioner(prov))
	svc.EnsureAccountFromIDToken(context.Background(), idToken)

	prov.wg.Wait()

	prov.mu.Lock()
	defer prov.mu.Unlock()
	if !prov.called {
		t.Fatal("expected provisioner to be called for new account")
	}
	if prov.accountID != "acct-prov-1" {
		t.Errorf("provisioner accountID = %q, want acct-prov-1", prov.accountID)
	}
}

func TestEnsureAccountFromIDToken_ExistingAccount_NoProvisioning(t *testing.T) {
	idToken := fakeIDToken(
		"existing-sub", "existing@example.com", "Ex", "User",
	)
	store := &mockAccountStore{
		getByCognitoSubFn: func(_ context.Context, _ string) (*models.Account, error) {
			return &models.Account{
				ID:         "acct-existing",
				CognitoSub: "existing-sub",
			}, nil
		},
	}
	prov := &spyProvisioner{}

	svc := service.NewAuthService(nil, store, service.WithProvisioner(prov))
	svc.EnsureAccountFromIDToken(context.Background(), idToken)

	time.Sleep(50 * time.Millisecond)

	prov.mu.Lock()
	defer prov.mu.Unlock()
	if prov.called {
		t.Fatal("provisioner should NOT be called for existing account")
	}
}

func TestEnsureAccountFromIDToken_NoProvisioner_NoPanic(t *testing.T) {
	idToken := fakeIDToken(
		"new-sub-nop", "nop@example.com", "No", "Prov",
	)
	store := &mockAccountStore{
		getByCognitoSubFn: func(_ context.Context, _ string) (*models.Account, error) {
			return nil, nil
		},
		createFn: func(_ context.Context, params repository.CreateAccountParams) (*models.Account, error) {
			return &models.Account{
				ID:         "acct-nop-1",
				Email:      params.Email,
				CognitoSub: params.CognitoSub,
			}, nil
		},
	}

	svc := service.NewAuthService(nil, store)
	// Should not panic even without a provisioner.
	svc.EnsureAccountFromIDToken(context.Background(), idToken)
}
