package service_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
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
	signUpFn  func(ctx context.Context, email, password, firstName, lastName string) (string, error)
	confirmFn func(ctx context.Context, email, code string) error
	loginFn   func(ctx context.Context, email, password string) (*auth.Result, error)
	logoutFn  func(ctx context.Context, accessToken string) error
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

func (m *mockCognito) Login(
	ctx context.Context, email, password string,
) (*auth.Result, error) {
	return m.loginFn(ctx, email, password)
}

func (m *mockCognito) Logout(
	ctx context.Context, accessToken string,
) error {
	return m.logoutFn(ctx, accessToken)
}

// --- mock AccountStore ---

type mockAccountStore struct {
	createFn          func(ctx context.Context, params repository.CreateAccountParams) (*models.Account, error)
	getByCognitoSubFn func(ctx context.Context, sub string) (*models.Account, error)
	verifyEmailFn     func(ctx context.Context, email string) error
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
