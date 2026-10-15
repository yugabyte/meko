package service_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
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
	resetTempPasswordFn     func(ctx context.Context, email, temporaryPassword string) error
	resendInviteFn          func(ctx context.Context, email string) error
	setPermanentPasswordFn  func(ctx context.Context, email, password string) error
	updateUserNameFn        func(ctx context.Context, username, firstName, lastName string) error
	updateEmailAttrFn       func(ctx context.Context, accessToken, newEmail string) error
	sendEmailAttrCodeFn     func(ctx context.Context, accessToken string) error
	verifyEmailAttrFn       func(ctx context.Context, accessToken, code string) error
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

func (m *mockCognito) ResetTempPassword(ctx context.Context, email, temporaryPassword string) error {
	if m.resetTempPasswordFn != nil {
		return m.resetTempPasswordFn(ctx, email, temporaryPassword)
	}
	return nil
}

func (m *mockCognito) ResendInvite(ctx context.Context, email string) error {
	if m.resendInviteFn != nil {
		return m.resendInviteFn(ctx, email)
	}
	return nil
}

func (m *mockCognito) SetPermanentPassword(ctx context.Context, email, password string) error {
	if m.setPermanentPasswordFn != nil {
		return m.setPermanentPasswordFn(ctx, email, password)
	}
	return nil
}

// fakeResetStore is an in-memory service.PasswordResetStore that mirrors
// repository.PasswordResetRepo's SQL.
type fakeResetStore struct {
	countErr error
	rows     map[string][]*models.PasswordResetRequest
	seq      int
}

func newFakeResetStore() *fakeResetStore {
	return &fakeResetStore{rows: map[string][]*models.PasswordResetRequest{}}
}

func (f *fakeResetStore) CountResetRequestsSince(_ context.Context, email string, since time.Time) (int, error) {
	if f.countErr != nil {
		return 0, f.countErr
	}
	n := 0
	for _, r := range f.rows[email] {
		if !r.RequestedAt.Before(since) {
			n++
		}
	}
	return n, nil
}

func (f *fakeResetStore) RecordResetRequest(_ context.Context, email string) (string, error) {
	f.seq++
	id := fmt.Sprintf("req-%d", f.seq)
	f.rows[email] = append(f.rows[email], &models.PasswordResetRequest{
		ID: id, Email: email, RequestedAt: time.Now().Add(time.Duration(f.seq) * time.Millisecond),
	})
	return id, nil
}

func (f *fakeResetStore) find(email, id string) *models.PasswordResetRequest {
	for _, r := range f.rows[email] {
		if r.ID == id {
			return r
		}
	}
	return nil
}

func (f *fakeResetStore) AttachResetCode(_ context.Context, email, id, codeHash string, expiresAt time.Time) error {
	r := f.find(email, id)
	if r == nil {
		return errors.New("no such request")
	}
	r.CodeHash, r.ExpiresAt = codeHash, &expiresAt
	return nil
}

func (f *fakeResetStore) LatestResetRequest(_ context.Context, email string) (*models.PasswordResetRequest, error) {
	rows := f.rows[email]
	if len(rows) == 0 {
		return nil, nil
	}
	latest := *rows[len(rows)-1]
	return &latest, nil
}

func (f *fakeResetStore) ReserveResetAttempt(_ context.Context, email, id string, maxAttempts int) (bool, error) {
	r := f.find(email, id)
	if r == nil || r.CodeHash == "" || r.UsedAt != nil || r.ExpiresAt == nil ||
		!time.Now().Before(*r.ExpiresAt) || r.Attempts >= maxAttempts {
		return false, nil
	}
	r.Attempts++
	return true, nil
}

func (f *fakeResetStore) ClaimResetCode(_ context.Context, email, id string) (bool, error) {
	r := f.find(email, id)
	if r == nil || r.UsedAt != nil {
		return false, nil
	}
	now := time.Now()
	r.UsedAt = &now
	return true, nil
}

func (f *fakeResetStore) UnclaimResetCode(_ context.Context, email, id string) error {
	r := f.find(email, id)
	if r == nil {
		return errors.New("no such request")
	}
	r.UsedAt = nil
	return nil
}

func (f *fakeResetStore) DeleteResetRequest(_ context.Context, email, id string) error {
	rows := f.rows[email]
	for i, r := range rows {
		if r.ID == id {
			f.rows[email] = append(rows[:i], rows[i+1:]...)
			return nil
		}
	}
	return nil
}

// mockPasswordSetupEmailer records the password-setup emails AuthService sends.
type mockPasswordSetupEmailer struct {
	err  error
	sent []service.PasswordSetupEmailParams
}

func (m *mockPasswordSetupEmailer) SendPasswordSetupEmail(
	_ context.Context, p service.PasswordSetupEmailParams,
) error {
	m.sent = append(m.sent, p)
	return m.err
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

func (m *mockCognito) UpdateUserName(
	ctx context.Context, username, firstName, lastName string,
) error {
	if m.updateUserNameFn != nil {
		return m.updateUserNameFn(ctx, username, firstName, lastName)
	}
	return nil
}

func (m *mockCognito) UpdateEmailAttribute(
	ctx context.Context, accessToken, newEmail string,
) error {
	if m.updateEmailAttrFn != nil {
		return m.updateEmailAttrFn(ctx, accessToken, newEmail)
	}
	return nil
}

func (m *mockCognito) SendEmailAttributeCode(
	ctx context.Context, accessToken string,
) error {
	if m.sendEmailAttrCodeFn != nil {
		return m.sendEmailAttrCodeFn(ctx, accessToken)
	}
	return nil
}

func (m *mockCognito) VerifyEmailAttribute(
	ctx context.Context, accessToken, code string,
) error {
	if m.verifyEmailAttrFn != nil {
		return m.verifyEmailAttrFn(ctx, accessToken, code)
	}
	return nil
}

// --- mock AccountStore ---

type mockAccountStore struct {
	createFn            func(ctx context.Context, params repository.CreateAccountParams) (*models.Account, error)
	getByCognitoSubFn   func(ctx context.Context, sub string) (*models.Account, error)
	getByIDFn           func(ctx context.Context, id string) (*models.Account, error)
	verifyEmailFn       func(ctx context.Context, email string) error
	setOnboardingFn     func(ctx context.Context, accountID string, completed bool) error
	emailInUseFn        func(ctx context.Context, email, excludeUserID string) (bool, error)
	updateEmailFn       func(ctx context.Context, userID, email string) error
	markEmailVerifiedFn func(ctx context.Context, userID string) error
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

func (m *mockAccountStore) GetByID(
	ctx context.Context, id string,
) (*models.Account, error) {
	if m.getByIDFn != nil {
		return m.getByIDFn(ctx, id)
	}
	return nil, nil
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

func (m *mockAccountStore) UpdateProfile(
	_ context.Context, _, _, _ string,
) error {
	return nil
}

func (m *mockAccountStore) EmailInUse(
	ctx context.Context, email, excludeUserID string,
) (bool, error) {
	if m.emailInUseFn != nil {
		return m.emailInUseFn(ctx, email, excludeUserID)
	}
	return false, nil
}

func (m *mockAccountStore) UpdateEmail(
	ctx context.Context, userID, email string,
) error {
	if m.updateEmailFn != nil {
		return m.updateEmailFn(ctx, userID, email)
	}
	return nil
}

func (m *mockAccountStore) MarkEmailVerified(
	ctx context.Context, userID string,
) error {
	if m.markEmailVerifiedFn != nil {
		return m.markEmailVerifiedFn(ctx, userID)
	}
	return nil
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
	store := newFakeResetStore()
	svc := service.NewAuthService(cog, &mockAccountStore{}, service.WithPasswordResetStore(store))

	if err := svc.ForgotPasswordSendCode(context.Background(), service.ForgotPasswordStartRequest{
		Email: "  User@Example.com  ", // trimmed before use
	}); err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
	if forgotEmail != "User@Example.com" {
		t.Errorf("expected ForgotPassword for trimmed email, got %q", forgotEmail)
	}
	if got := len(store.rows["user@example.com"]); got != 1 {
		t.Errorf("expected one request recorded under the lowercased email, got %d", got)
	}
}

func TestAuthService_ForgotPasswordSendCode_NoAccount_SameResponse(t *testing.T) {
	// OWASP: the response must not reveal whether an account exists.
	cog := &mockCognito{
		forgotPasswordFn: func(_ context.Context, _ string) error {
			return auth.ErrUserNotFound
		},
	}
	svc := service.NewAuthService(cog, &mockAccountStore{})

	if err := svc.ForgotPasswordSendCode(context.Background(), service.ForgotPasswordStartRequest{
		Email: "missing@example.com",
	}); err != nil {
		t.Errorf("expected the same success as an existing account, got %v", err)
	}
}

func TestAuthService_ForgotPasswordSendCode_PerEmailLimit(t *testing.T) {
	calls := 0
	cog := &mockCognito{
		forgotPasswordFn: func(_ context.Context, _ string) error {
			calls++
			return nil
		},
	}
	svc := service.NewAuthService(cog, &mockAccountStore{}, service.WithPasswordResetStore(newFakeResetStore()))

	for i := 0; i < 5; i++ {
		if err := svc.ForgotPasswordSendCode(context.Background(), service.ForgotPasswordStartRequest{
			Email: "user@example.com",
		}); err != nil {
			t.Fatalf("request %d: expected no error, got %v", i+1, err)
		}
	}
	err := svc.ForgotPasswordSendCode(context.Background(), service.ForgotPasswordStartRequest{
		Email: "USER@example.com",
	})
	if !errors.Is(err, service.ErrPasswordResetRateLimited) {
		t.Errorf("expected ErrPasswordResetRateLimited on the 6th request, got %v", err)
	}
	if calls != 5 {
		t.Errorf("expected Cognito to be called 5 times, got %d", calls)
	}
}

func TestAuthService_ForgotPasswordSendCode_StoreError_Unavailable(t *testing.T) {
	store := newFakeResetStore()
	store.countErr = errors.New("connection refused")
	svc := service.NewAuthService(&mockCognito{}, &mockAccountStore{}, service.WithPasswordResetStore(store))

	err := svc.ForgotPasswordSendCode(context.Background(), service.ForgotPasswordStartRequest{
		Email: "user@example.com",
	})
	if !errors.Is(err, service.ErrPasswordResetUnavailable) {
		t.Errorf("expected ErrPasswordResetUnavailable, got %v", err)
	}
}

// forceChangeCognito returns a mock whose ForgotPassword fails with forgotErr
// (ErrPasswordResetNotAllowed for a FORCE_CHANGE_PASSWORD user) and
// reports status. Any password change fails the test: Forgot password must
// not change the account before a code is presented.
func forceChangeCognito(t *testing.T, forgotErr error, status auth.UserStatus) *mockCognito {
	return &mockCognito{
		forgotPasswordFn: func(_ context.Context, _ string) error {
			return forgotErr
		},
		getUserStatusFn: func(_ context.Context, _ string) (auth.UserStatus, error) {
			return status, nil
		},
		resetTempPasswordFn: func(_ context.Context, _, _ string) error {
			t.Error("Forgot password must not reset the temporary password")
			return nil
		},
		setPermanentPasswordFn: func(_ context.Context, _, _ string) error {
			t.Error("Forgot password must not set a password before a code is presented")
			return nil
		},
	}
}

func TestAuthService_ForgotPasswordSendCode_ForceChange_SendsMekoCode(t *testing.T) {
	store := newFakeResetStore()
	mailer := &mockPasswordSetupEmailer{}
	svc := service.NewAuthService(
		forceChangeCognito(t, auth.ErrPasswordResetNotAllowed, auth.UserStatusForceChangePassword),
		&mockAccountStore{}, service.WithPasswordSetupEmailer(mailer), service.WithPasswordResetStore(store))

	if err := svc.ForgotPasswordSendCode(context.Background(), service.ForgotPasswordStartRequest{
		Email: "invited@example.com",
	}); err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
	if len(mailer.sent) != 1 {
		t.Fatalf("expected one email, got %d", len(mailer.sent))
	}
	sent := mailer.sent[0]
	if sent.Reason != service.PasswordSetupForgotPassword || len(sent.Code) != 8 || sent.TempPassword != "" ||
		sent.CodeTTL != time.Hour {
		t.Errorf("expected an 8-digit code valid for an hour and no temp password, got %+v", sent)
	}
	latest, err := store.LatestResetRequest(context.Background(), "invited@example.com")
	if err != nil {
		t.Fatal(err)
	}
	if latest == nil || latest.CodeHash == "" || strings.Contains(latest.CodeHash, sent.Code) {
		t.Errorf("expected only a hash of the code to be stored, got %+v", latest)
	}
}

func TestAuthService_ForgotPasswordSendCode_ForceChange_NoEmailer_SameResponse(t *testing.T) {
	// Without SES no code can be sent, but the answer must still match an
	// unknown email's, or it would reveal pending invitees.
	svc := service.NewAuthService(
		forceChangeCognito(t, auth.ErrPasswordResetNotAllowed, auth.UserStatusForceChangePassword),
		&mockAccountStore{}, service.WithPasswordResetStore(newFakeResetStore()))

	if err := svc.ForgotPasswordSendCode(context.Background(), service.ForgotPasswordStartRequest{
		Email: "invited@example.com",
	}); err != nil {
		t.Errorf("expected the same success as any other email, got %v", err)
	}
}
func TestAuthService_ForgotPasswordSendCode_NotAllowed_OtherStatus_SameResponse(t *testing.T) {
	mailer := &mockPasswordSetupEmailer{}
	svc := service.NewAuthService(forceChangeCognito(t, auth.ErrPasswordResetNotAllowed, auth.UserStatusConfirmed),
		&mockAccountStore{}, service.WithPasswordSetupEmailer(mailer), service.WithPasswordResetStore(newFakeResetStore()))

	err := svc.ForgotPasswordSendCode(context.Background(), service.ForgotPasswordStartRequest{
		Email: "confirmed@example.com",
	})
	if err != nil || len(mailer.sent) != 0 {
		t.Errorf("expected the uniform success and no email, got err=%v sent=%d", err, len(mailer.sent))
	}
}
func TestAuthService_ForgotPasswordSendCode_OtherCognitoError_UniformSuccess(t *testing.T) {
	// Account-specific Cognito errors (unconfirmed, disabled, bad secret,
	// outage) must not distinguish the email from an unknown one.
	otherErrs := []error{
		auth.ErrNotAuthorized, auth.ErrUserNotConfirmed, auth.ErrCognitoInternal,
		auth.ErrInvalidCognitoParameter, auth.ErrCognitoAccessDenied,
	}
	for _, forgotErr := range otherErrs {
		mailer := &mockPasswordSetupEmailer{}
		store := newFakeResetStore()
		svc := service.NewAuthService(forceChangeCognito(t, forgotErr, auth.UserStatusForceChangePassword),
			&mockAccountStore{}, service.WithPasswordSetupEmailer(mailer),
			service.WithPasswordResetStore(store))

		err := svc.ForgotPasswordSendCode(context.Background(), service.ForgotPasswordStartRequest{
			Email: "invited@example.com",
		})
		if err != nil || len(mailer.sent) != 0 {
			t.Errorf("%v: expected uniform success and no email, got err=%v sent=%d", forgotErr, err, len(mailer.sent))
		}
		if n, _ := store.CountResetRequestsSince(context.Background(), "invited@example.com", time.Now().Add(-time.Hour)); n != 0 {
			t.Errorf("%v: abandoned request must be discarded, got count=%d", forgotErr, n)
		}
	}
}

func TestAuthService_ForgotPasswordSendCode_RateLimit_DoesNotHideLiveCode(t *testing.T) {
	store := newFakeResetStore()
	mailer := &mockPasswordSetupEmailer{}
	svc := service.NewAuthService(
		forceChangeCognito(t, auth.ErrPasswordResetNotAllowed, auth.UserStatusForceChangePassword),
		&mockAccountStore{}, service.WithPasswordSetupEmailer(mailer), service.WithPasswordResetStore(store))

	for i := 0; i < 5; i++ {
		if err := svc.ForgotPasswordSendCode(context.Background(), service.ForgotPasswordStartRequest{
			Email: "invited@example.com",
		}); err != nil {
			t.Fatalf("request %d: %v", i+1, err)
		}
	}
	code := mailer.sent[len(mailer.sent)-1].Code
	if err := svc.ForgotPasswordSendCode(context.Background(), service.ForgotPasswordStartRequest{
		Email: "invited@example.com",
	}); !errors.Is(err, service.ErrPasswordResetRateLimited) {
		t.Fatalf("expected rate limit on the 6th request, got %v", err)
	}
	n, err := store.CountResetRequestsSince(context.Background(), "invited@example.com", time.Now().Add(-time.Hour))
	if err != nil || n != 5 {
		t.Fatalf("rejected request must not stay in the count, got n=%d err=%v", n, err)
	}
	cog := &mockCognito{
		setPermanentPasswordFn: func(_ context.Context, _, _ string) error { return nil },
		confirmForgotPasswordFn: func(_ context.Context, _, _, _ string) error {
			t.Error("live Meko code must not be sent to Cognito ConfirmForgotPassword")
			return nil
		},
	}
	svc = service.NewAuthService(cog, &mockAccountStore{},
		service.WithPasswordSetupEmailer(mailer), service.WithPasswordResetStore(store))
	if err := svc.ConfirmForgotPasswordReset(context.Background(), confirmReq(code)); err != nil {
		t.Fatalf("rate-limited request must not hide the live Meko code, got %v", err)
	}
}
func TestAuthService_ForgotPasswordSendCode_ForceChange_EmailFails_SameResponse(t *testing.T) {
	// An SES failure is logged; the answer stays the uniform success so it
	// can't single out pending invitees during an SES outage.
	mailer := &mockPasswordSetupEmailer{err: errors.New("MessageRejected: Email address is not verified. RequestId: abc")}
	svc := service.NewAuthService(
		forceChangeCognito(t, auth.ErrPasswordResetNotAllowed, auth.UserStatusForceChangePassword),
		&mockAccountStore{}, service.WithPasswordSetupEmailer(mailer), service.WithPasswordResetStore(newFakeResetStore()))

	if err := svc.ForgotPasswordSendCode(context.Background(), service.ForgotPasswordStartRequest{
		Email: "invited@example.com",
	}); err != nil {
		t.Errorf("expected the uniform success, got %v", err)
	}
}

func TestAuthService_ConfirmForgotPassword_MekoCode_CognitoFails_CodeStillWorks(t *testing.T) {
	// The code is claimed before Cognito accepts, then unclaimed on failure,
	// so a Cognito failure leaves it usable and concurrent callers can't both
	// set the password.
	calls := 0
	cog := &mockCognito{
		setPermanentPasswordFn: func(_ context.Context, _, _ string) error {
			calls++
			if calls == 1 {
				return auth.ErrTooManyRequests
			}
			return nil
		},
	}
	svc, code, store := issueMekoCode(t, cog)

	err := svc.ConfirmForgotPasswordReset(context.Background(), confirmReq(code))
	if !errors.Is(err, auth.ErrTooManyRequests) {
		t.Fatalf("expected the Cognito error, got %v", err)
	}
	latest, err := store.LatestResetRequest(context.Background(), "invited@example.com")
	if err != nil || latest == nil || latest.UsedAt != nil {
		t.Fatalf("code must be unclaimed after Cognito failure, got %+v err=%v", latest, err)
	}
	if err := svc.ConfirmForgotPasswordReset(context.Background(), confirmReq(code)); err != nil {
		t.Errorf("expected the retry with the same code to succeed, got %v", err)
	}
}

// --- ConfirmForgotPasswordReset with a Meko-issued code ---

// issueMekoCode runs Forgot password for an unactivated invitee and returns
// the service, the emailed code and the store.
func issueMekoCode(t *testing.T, cog *mockCognito) (*service.AuthService, string, *fakeResetStore) {
	t.Helper()
	cog.forgotPasswordFn = func(_ context.Context, _ string) error { return auth.ErrPasswordResetNotAllowed }
	cog.getUserStatusFn = func(_ context.Context, _ string) (auth.UserStatus, error) {
		return auth.UserStatusForceChangePassword, nil
	}
	store := newFakeResetStore()
	mailer := &mockPasswordSetupEmailer{}
	svc := service.NewAuthService(cog, &mockAccountStore{},
		service.WithPasswordSetupEmailer(mailer), service.WithPasswordResetStore(store))
	if err := svc.ForgotPasswordSendCode(context.Background(), service.ForgotPasswordStartRequest{
		Email: "invited@example.com",
	}); err != nil {
		t.Fatalf("ForgotPasswordSendCode: %v", err)
	}
	return svc, mailer.sent[0].Code, store
}

func confirmReq(code string) service.ForgotPasswordConfirmRequest {
	return service.ForgotPasswordConfirmRequest{
		Email: "invited@example.com", Code: code, NewPassword: "New-Pass-1!", ConfirmPassword: "New-Pass-1!",
	}
}

func TestAuthService_ConfirmForgotPassword_MekoCode_SetsPermanentPassword_Once(t *testing.T) {
	var setEmail, setPwd string
	cog := &mockCognito{
		setPermanentPasswordFn: func(_ context.Context, email, pwd string) error {
			setEmail, setPwd = email, pwd
			return nil
		},
		confirmForgotPasswordFn: func(_ context.Context, _, _, _ string) error {
			t.Error("a Meko code must not be sent to Cognito ConfirmForgotPassword")
			return nil
		},
	}
	svc, code, _ := issueMekoCode(t, cog)

	if err := svc.ConfirmForgotPasswordReset(context.Background(), confirmReq(code)); err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
	if setEmail != "invited@example.com" || setPwd != "New-Pass-1!" {
		t.Errorf("expected SetPermanentPassword for the user, got %q / %q", setEmail, setPwd)
	}
	err := svc.ConfirmForgotPasswordReset(context.Background(), confirmReq(code))
	if !errors.Is(err, auth.ErrCodeExpired) {
		t.Errorf("expected a used code to be rejected as expired, got %v", err)
	}
}

func TestAuthService_ConfirmForgotPassword_MekoCode_WrongCodeBurnsAfterFive(t *testing.T) {
	cog := &mockCognito{
		setPermanentPasswordFn: func(_ context.Context, _, _ string) error {
			t.Error("no password may be set after the code is burned")
			return nil
		},
	}
	svc, code, _ := issueMekoCode(t, cog)

	for i := 0; i < 5; i++ {
		err := svc.ConfirmForgotPasswordReset(context.Background(), confirmReq("00000000"))
		if code == "00000000" {
			t.Skip("random code collided with the wrong guess")
		}
		if !errors.Is(err, auth.ErrCodeMismatch) {
			t.Fatalf("attempt %d: expected ErrCodeMismatch, got %v", i+1, err)
		}
	}
	err := svc.ConfirmForgotPasswordReset(context.Background(), confirmReq(code))
	if !errors.Is(err, auth.ErrCodeExpired) {
		t.Errorf("expected the right code to fail after 5 wrong attempts, got %v", err)
	}
}

func TestAuthService_ConfirmForgotPassword_MekoCode_Expired(t *testing.T) {
	cog := &mockCognito{}
	svc, code, store := issueMekoCode(t, cog)
	past := time.Now().Add(-time.Minute)
	store.rows["invited@example.com"][0].ExpiresAt = &past

	err := svc.ConfirmForgotPasswordReset(context.Background(), confirmReq(code))
	if !errors.Is(err, auth.ErrCodeExpired) {
		t.Errorf("expected ErrCodeExpired, got %v", err)
	}
}

func TestAuthService_ConfirmForgotPassword_NoMekoCode_UsesCognito(t *testing.T) {
	cognitoCalled := false
	cog := &mockCognito{
		confirmForgotPasswordFn: func(_ context.Context, _, _, _ string) error {
			cognitoCalled = true
			return nil
		},
	}
	store := newFakeResetStore()
	svc := service.NewAuthService(cog, &mockAccountStore{}, service.WithPasswordResetStore(store))
	if err := svc.ForgotPasswordSendCode(context.Background(), service.ForgotPasswordStartRequest{
		Email: "invited@example.com",
	}); err != nil {
		t.Fatalf("ForgotPasswordSendCode: %v", err)
	}

	if err := svc.ConfirmForgotPasswordReset(context.Background(), confirmReq("123456")); err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
	if !cognitoCalled {
		t.Error("expected Cognito ConfirmForgotPassword when the newest request has no Meko code")
	}
}

func TestAuthService_Login_ExpiredTempPassword_NoSES_FallsBackToResendInvite(t *testing.T) {
	// Without SES the rescue keeps working through Cognito's own resend.
	resent := ""
	cog := &mockCognito{
		loginFn: func(_ context.Context, _, _ string) (*auth.Result, error) {
			return nil, auth.ErrTempPasswordExpired
		},
		getUserStatusFn: func(_ context.Context, _ string) (auth.UserStatus, error) {
			return auth.UserStatusForceChangePassword, nil
		},
		resendInviteFn: func(_ context.Context, email string) error {
			resent = email
			return nil
		},
		resetTempPasswordFn: func(_ context.Context, _, _ string) error {
			t.Error("ResetTempPassword must not run without an emailer")
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
	if resent != "pending@example.com" {
		t.Errorf("expected ResendInvite for pending@example.com, got %q", resent)
	}
}

func TestAuthService_Login_ExpiredTempPassword_EmailFails_KeepsPassword(t *testing.T) {
	// The rescue emails first: if SES fails, the temporary password is not
	// replaced.
	cog := &mockCognito{
		loginFn: func(_ context.Context, _, _ string) (*auth.Result, error) {
			return nil, auth.ErrTempPasswordExpired
		},
		getUserStatusFn: func(_ context.Context, _ string) (auth.UserStatus, error) {
			return auth.UserStatusForceChangePassword, nil
		},
		resetTempPasswordFn: func(_ context.Context, _, _ string) error {
			t.Error("ResetTempPassword must not run when the email fails")
			return nil
		},
	}
	mailer := &mockPasswordSetupEmailer{err: errors.New("ses down")}
	svc := service.NewAuthService(cog, &mockAccountStore{}, service.WithPasswordSetupEmailer(mailer))

	_, err := svc.Login(context.Background(), service.LoginRequest{
		Email:    "pending@example.com",
		Password: "Expired-1!",
	})
	if errors.Is(err, service.ErrInvitePasswordExpired) || err == nil {
		t.Errorf("expected a plain login failure, got %v", err)
	}
}

// --- Login expired-temp-password rescue tests ---

func TestAuthService_Login_ExpiredTempPassword_RescuesAndSignals(t *testing.T) {
	var resetEmail, resetPwd string
	cog := &mockCognito{
		loginFn: func(_ context.Context, _, _ string) (*auth.Result, error) {
			return nil, auth.ErrTempPasswordExpired
		},
		getUserStatusFn: func(_ context.Context, _ string) (auth.UserStatus, error) {
			return auth.UserStatusForceChangePassword, nil
		},
		resetTempPasswordFn: func(_ context.Context, email, pwd string) error {
			resetEmail, resetPwd = email, pwd
			return nil
		},
	}
	mailer := &mockPasswordSetupEmailer{}
	svc := service.NewAuthService(cog, &mockAccountStore{}, service.WithPasswordSetupEmailer(mailer))

	_, err := svc.Login(context.Background(), service.LoginRequest{
		Email:    "pending@example.com",
		Password: "Expired-1!",
	})
	if !errors.Is(err, service.ErrInvitePasswordExpired) {
		t.Errorf("expected ErrInvitePasswordExpired, got %v", err)
	}
	if resetEmail != "pending@example.com" || resetPwd == "" {
		t.Errorf("expected ResetTempPassword for pending@example.com with a password, got %q / %q",
			resetEmail, resetPwd)
	}
	if len(mailer.sent) != 1 {
		t.Fatalf("expected one password-setup email, got %d", len(mailer.sent))
	}
	got := mailer.sent[0]
	if got.Reason != service.PasswordSetupInviteExpired || got.TempPassword != resetPwd ||
		got.RecipientEmail != "pending@example.com" {
		t.Errorf("unexpected password-setup email params: %+v", got)
	}
}

func TestAuthService_Login_ExpiredTempPassword_NotForceChange_FallsBack(t *testing.T) {
	// Defensive: if the account isn't actually FORCE_CHANGE_PASSWORD, do not
	// resend anything and fall back to a normal failed-login.
	resetCalled := false
	cog := &mockCognito{
		loginFn: func(_ context.Context, _, _ string) (*auth.Result, error) {
			return nil, auth.ErrTempPasswordExpired
		},
		getUserStatusFn: func(_ context.Context, _ string) (auth.UserStatus, error) {
			return auth.UserStatusConfirmed, nil
		},
		resetTempPasswordFn: func(_ context.Context, _, _ string) error {
			resetCalled = true
			return nil
		},
	}
	svc := service.NewAuthService(cog, &mockAccountStore{},
		service.WithPasswordSetupEmailer(&mockPasswordSetupEmailer{}))

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
	if resetCalled {
		t.Error("ResetTempPassword must not be called for a non-force-change account")
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
		resetTempPasswordFn: func(_ context.Context, _, _ string) error {
			return auth.ErrTooManyRequests
		},
	}
	svc := service.NewAuthService(cog, &mockAccountStore{},
		service.WithPasswordSetupEmailer(&mockPasswordSetupEmailer{}))

	_, err := svc.Login(context.Background(), service.LoginRequest{
		Email:    "pending@example.com",
		Password: "Expired-1!",
	})
	if !errors.Is(err, service.ErrPasswordResetUnavailable) {
		t.Errorf("expected ErrPasswordResetUnavailable when Cognito refuses the emailed password, got %v", err)
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

// --- Email project / verification (federated no-email) tests ---

func TestStartEmailChange_RejectsBadEmail(t *testing.T) {
	cases := []string{
		"",                                 // empty
		"not-an-email",                     // no @/domain
		"a@b",                              // no dotted domain
		"x@no-reply.linkedin.meko.invalid", // the synthesized placeholder itself
	}
	for _, in := range cases {
		var cognitoCalled bool
		cog := &mockCognito{
			updateEmailAttrFn: func(_ context.Context, _, _ string) error {
				cognitoCalled = true
				return nil
			},
		}
		svc := service.NewAuthService(cog, &mockAccountStore{})
		err := svc.StartEmailChange(
			context.Background(), "acct-1", "tok",
			service.StartEmailChangeRequest{Email: in},
		)
		if !errors.Is(err, service.ErrInvalidEmail) {
			t.Errorf("email %q: expected ErrInvalidEmail, got %v", in, err)
		}
		if cognitoCalled {
			t.Errorf("email %q: Cognito should not be called for invalid input", in)
		}
	}
}

func TestStartEmailChange_Success_UpdatesCognitoThenDB(t *testing.T) {
	var order []string
	cog := &mockCognito{
		updateEmailAttrFn: func(_ context.Context, token, email string) error {
			if token != "tok" || email != "real@example.com" {
				t.Errorf("unexpected cognito args: %q %q", token, email)
			}
			order = append(order, "cognito")
			return nil
		},
	}
	store := &mockAccountStore{
		updateEmailFn: func(_ context.Context, userID, email string) error {
			if userID != "acct-1" || email != "real@example.com" {
				t.Errorf("unexpected db args: %q %q", userID, email)
			}
			order = append(order, "db")
			return nil
		},
	}
	svc := service.NewAuthService(cog, store)
	if err := svc.StartEmailChange(
		context.Background(), "acct-1", "tok",
		service.StartEmailChangeRequest{Email: "  real@example.com  "},
	); err != nil {
		t.Fatalf("expected success, got %v", err)
	}
	if len(order) != 2 || order[0] != "cognito" || order[1] != "db" {
		t.Fatalf("expected cognito-then-db ordering, got %v", order)
	}
}

func TestStartEmailChange_CognitoAliasExists_MapsToEmailTaken(t *testing.T) {
	var dbCalled bool
	cog := &mockCognito{
		updateEmailAttrFn: func(_ context.Context, _, _ string) error {
			return auth.ErrEmailExists
		},
	}
	store := &mockAccountStore{
		updateEmailFn: func(_ context.Context, _, _ string) error {
			dbCalled = true
			return nil
		},
	}
	svc := service.NewAuthService(cog, store)
	err := svc.StartEmailChange(
		context.Background(), "acct-1", "tok",
		service.StartEmailChangeRequest{Email: "taken@example.com"},
	)
	if !errors.Is(err, service.ErrEmailTaken) {
		t.Fatalf("expected ErrEmailTaken, got %v", err)
	}
	if dbCalled {
		t.Error("DB should not be written when Cognito rejects the email")
	}
}

func TestStartEmailChange_EmailInUse_NoOTPSent(t *testing.T) {
	var cognitoCalled, dbCalled bool
	cog := &mockCognito{
		updateEmailAttrFn: func(_ context.Context, _, _ string) error {
			cognitoCalled = true
			return nil
		},
	}
	store := &mockAccountStore{
		emailInUseFn: func(_ context.Context, email, excludeUserID string) (bool, error) {
			if email != "taken@example.com" || excludeUserID != "acct-1" {
				t.Errorf("unexpected EmailInUse args: %q %q", email, excludeUserID)
			}
			return true, nil
		},
		updateEmailFn: func(_ context.Context, _, _ string) error {
			dbCalled = true
			return nil
		},
	}
	svc := service.NewAuthService(cog, store)
	err := svc.StartEmailChange(
		context.Background(), "acct-1", "tok",
		service.StartEmailChangeRequest{Email: "taken@example.com"},
	)
	if !errors.Is(err, service.ErrEmailTaken) {
		t.Fatalf("expected ErrEmailTaken, got %v", err)
	}
	if cognitoCalled {
		t.Error("Cognito (OTP send) must not be called when the email is already in use")
	}
	if dbCalled {
		t.Error("DB write must not happen when the email is already in use")
	}
}

func TestStartEmailChange_DBConflict_MapsToEmailTaken(t *testing.T) {
	cog := &mockCognito{}
	store := &mockAccountStore{
		updateEmailFn: func(_ context.Context, _, _ string) error {
			return repository.ErrEmailTaken
		},
	}
	svc := service.NewAuthService(cog, store)
	err := svc.StartEmailChange(
		context.Background(), "acct-1", "tok",
		service.StartEmailChangeRequest{Email: "taken@example.com"},
	)
	if !errors.Is(err, service.ErrEmailTaken) {
		t.Fatalf("expected ErrEmailTaken, got %v", err)
	}
}

func TestVerifyEmailChange_Success_MarksVerified(t *testing.T) {
	var marked bool
	cog := &mockCognito{
		verifyEmailAttrFn: func(_ context.Context, token, code string) error {
			if token != "tok" || code != "123456" {
				t.Errorf("unexpected verify args: %q %q", token, code)
			}
			return nil
		},
	}
	store := &mockAccountStore{
		markEmailVerifiedFn: func(_ context.Context, userID string) error {
			if userID != "acct-1" {
				t.Errorf("unexpected userID: %q", userID)
			}
			marked = true
			return nil
		},
	}
	svc := service.NewAuthService(cog, store)
	if err := svc.VerifyEmailChange(
		context.Background(), "acct-1", "tok",
		service.VerifyEmailChangeRequest{Code: " 123456 "},
	); err != nil {
		t.Fatalf("expected success, got %v", err)
	}
	if !marked {
		t.Error("expected MarkEmailVerified to be called")
	}
}

func TestVerifyEmailChange_BadCode_DoesNotMarkVerified(t *testing.T) {
	var marked bool
	cog := &mockCognito{
		verifyEmailAttrFn: func(_ context.Context, _, _ string) error {
			return auth.ErrCodeMismatch
		},
	}
	store := &mockAccountStore{
		markEmailVerifiedFn: func(_ context.Context, _ string) error {
			marked = true
			return nil
		},
	}
	svc := service.NewAuthService(cog, store)
	err := svc.VerifyEmailChange(
		context.Background(), "acct-1", "tok",
		service.VerifyEmailChangeRequest{Code: "000000"},
	)
	if !errors.Is(err, auth.ErrCodeMismatch) {
		t.Fatalf("expected ErrCodeMismatch, got %v", err)
	}
	if marked {
		t.Error("email must not be marked verified when the code is rejected")
	}
}

// A deployment with no password store — dev auth, and any OIDC install — used to
// pass a nil CognitoProvider. UpdateProfile's Cognito name-sync is guarded on the
// account's sub being non-empty, and DevAuth creates its account with
// CognitoSub="dev-local", so the guard passed and the nil interface was called:
// PATCH /auth/me/profile panicked in every BYOC data plane. NoUserStore is what
// makes "no user store" a value rather than a nil.
func TestUpdateProfile_NoUserStoreDoesNotPanic(t *testing.T) {
	accounts := &mockAccountStore{
		getByIDFn: func(_ context.Context, id string) (*models.Account, error) {
			// Non-empty sub: this is what gets past the guard.
			return &models.Account{ID: id, CognitoSub: "dev-local"}, nil
		},
	}
	svc := service.NewAuthService(service.NoUserStore{}, accounts)

	err := svc.UpdateProfile(context.Background(), "acct-1", service.UpdateProfileRequest{
		FirstName: "Ada", LastName: "Lovelace",
	})
	// The local write is what matters; the IdP sync is best-effort and there is
	// no IdP to sync to, so a nil error is the whole assertion.
	if err != nil {
		t.Fatalf("UpdateProfile with NoUserStore: %v", err)
	}
}
