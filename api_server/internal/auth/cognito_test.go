package auth_test

import (
	"context"
	"errors"
	"testing"

	cip "github.com/aws/aws-sdk-go-v2/service/cognitoidentityprovider"
	"github.com/aws/aws-sdk-go-v2/service/cognitoidentityprovider/types"

	"meko/api_server/internal/auth"
)

type cipOpt = func(*cip.Options)

// mockCognitoAPI implements auth.CognitoAPI for unit testing.
type mockCognitoAPI struct {
	signUpFn func(
		ctx context.Context, params *cip.SignUpInput,
		optFns ...cipOpt,
	) (*cip.SignUpOutput, error)

	confirmFn func(
		ctx context.Context, params *cip.ConfirmSignUpInput,
		optFns ...cipOpt,
	) (*cip.ConfirmSignUpOutput, error)

	initiateAuthFn func(
		ctx context.Context, params *cip.InitiateAuthInput,
		optFns ...cipOpt,
	) (*cip.InitiateAuthOutput, error)

	globalSignOutFn func(
		ctx context.Context, params *cip.GlobalSignOutInput,
		optFns ...cipOpt,
	) (*cip.GlobalSignOutOutput, error)
}

func (m *mockCognitoAPI) SignUp(
	ctx context.Context,
	params *cip.SignUpInput,
	optFns ...func(*cip.Options),
) (*cip.SignUpOutput, error) {
	return m.signUpFn(ctx, params, optFns...)
}

func (m *mockCognitoAPI) ConfirmSignUp(
	ctx context.Context,
	params *cip.ConfirmSignUpInput,
	optFns ...func(*cip.Options),
) (*cip.ConfirmSignUpOutput, error) {
	return m.confirmFn(ctx, params, optFns...)
}

func (m *mockCognitoAPI) InitiateAuth(
	ctx context.Context,
	params *cip.InitiateAuthInput,
	optFns ...func(*cip.Options),
) (*cip.InitiateAuthOutput, error) {
	return m.initiateAuthFn(ctx, params, optFns...)
}

func (m *mockCognitoAPI) GlobalSignOut(
	ctx context.Context,
	params *cip.GlobalSignOutInput,
	optFns ...func(*cip.Options),
) (*cip.GlobalSignOutOutput, error) {
	return m.globalSignOutFn(ctx, params, optFns...)
}

func strPtr(s string) *string { return &s }

func newTestClient(mock *mockCognitoAPI) *auth.CognitoClient {
	return auth.NewCognitoClientWithAPI(mock, "test-pool", "test-client", "")
}

// --------------- SignUp tests ---------------

func TestCognitoClient_SignUp_Success(t *testing.T) {
	mock := &mockCognitoAPI{
		signUpFn: func(
			_ context.Context,
			_ *cip.SignUpInput,
			_ ...func(*cip.Options),
		) (*cip.SignUpOutput, error) {
			return &cip.SignUpOutput{
				UserSub: strPtr("abc-123-sub"),
			}, nil
		},
	}
	client := newTestClient(mock)

	sub, err := client.SignUp(
		context.Background(),
		"user@example.com", "P@ssw0rd!", "Jane", "Doe",
	)
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
	if sub != "abc-123-sub" {
		t.Errorf("expected sub abc-123-sub, got %s", sub)
	}
}

func TestCognitoClient_SignUp_UsernameExists(t *testing.T) {
	mock := &mockCognitoAPI{
		signUpFn: func(
			_ context.Context,
			_ *cip.SignUpInput,
			_ ...func(*cip.Options),
		) (*cip.SignUpOutput, error) {
			return nil, &types.UsernameExistsException{
				Message: strPtr("user already exists"),
			}
		},
	}
	client := newTestClient(mock)

	_, err := client.SignUp(
		context.Background(),
		"user@example.com", "P@ssw0rd!", "Jane", "Doe",
	)
	if !errors.Is(err, auth.ErrUserExists) {
		t.Errorf("expected ErrUserExists, got %v", err)
	}
}

// --------------- ConfirmSignUp tests ---------------

func TestCognitoClient_ConfirmSignUp_Success(t *testing.T) {
	mock := &mockCognitoAPI{
		confirmFn: func(
			_ context.Context,
			_ *cip.ConfirmSignUpInput,
			_ ...func(*cip.Options),
		) (*cip.ConfirmSignUpOutput, error) {
			return &cip.ConfirmSignUpOutput{}, nil
		},
	}
	client := newTestClient(mock)

	err := client.ConfirmSignUp(
		context.Background(), "user@example.com", "123456",
	)
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
}

func TestCognitoClient_ConfirmSignUp_CodeMismatch(t *testing.T) {
	mock := &mockCognitoAPI{
		confirmFn: func(
			_ context.Context,
			_ *cip.ConfirmSignUpInput,
			_ ...func(*cip.Options),
		) (*cip.ConfirmSignUpOutput, error) {
			return nil, &types.CodeMismatchException{
				Message: strPtr("invalid code"),
			}
		},
	}
	client := newTestClient(mock)

	err := client.ConfirmSignUp(
		context.Background(), "user@example.com", "000000",
	)
	if !errors.Is(err, auth.ErrCodeMismatch) {
		t.Errorf("expected ErrCodeMismatch, got %v", err)
	}
}

// --------------- Login tests ---------------

func TestCognitoClient_Login_Success(t *testing.T) {
	mock := &mockCognitoAPI{
		initiateAuthFn: func(
			_ context.Context,
			_ *cip.InitiateAuthInput,
			_ ...func(*cip.Options),
		) (*cip.InitiateAuthOutput, error) {
			return &cip.InitiateAuthOutput{
				AuthenticationResult: &types.AuthenticationResultType{
					AccessToken:  strPtr("access-tok"),
					IdToken:      strPtr("id-tok"),
					RefreshToken: strPtr("refresh-tok"),
					ExpiresIn:    3600,
				},
			}, nil
		},
	}
	client := newTestClient(mock)

	result, err := client.Login(
		context.Background(), "user@example.com", "P@ssw0rd!",
	)
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
	if result.AccessToken != "access-tok" {
		t.Errorf("expected access-tok, got %s", result.AccessToken)
	}
	if result.IDToken != "id-tok" {
		t.Errorf("expected id-tok, got %s", result.IDToken)
	}
	if result.RefreshToken != "refresh-tok" {
		t.Errorf("expected refresh-tok, got %s", result.RefreshToken)
	}
	if result.ExpiresIn != 3600 {
		t.Errorf("expected 3600, got %d", result.ExpiresIn)
	}
}

func TestCognitoClient_Login_InvalidCredentials(t *testing.T) {
	mock := &mockCognitoAPI{
		initiateAuthFn: func(
			_ context.Context,
			_ *cip.InitiateAuthInput,
			_ ...func(*cip.Options),
		) (*cip.InitiateAuthOutput, error) {
			return nil, &types.NotAuthorizedException{
				Message: strPtr("incorrect username or password"),
			}
		},
	}
	client := newTestClient(mock)

	_, err := client.Login(
		context.Background(), "user@example.com", "wrong",
	)
	if !errors.Is(err, auth.ErrNotAuthorized) {
		t.Errorf("expected ErrNotAuthorized, got %v", err)
	}
}

// --------------- Logout tests ---------------

func TestCognitoClient_Logout_Success(t *testing.T) {
	mock := &mockCognitoAPI{
		globalSignOutFn: func(
			_ context.Context,
			_ *cip.GlobalSignOutInput,
			_ ...func(*cip.Options),
		) (*cip.GlobalSignOutOutput, error) {
			return &cip.GlobalSignOutOutput{}, nil
		},
	}
	client := newTestClient(mock)

	err := client.Logout(context.Background(), "some-access-token")
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
}
