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

	adminCreateUserFn func(
		ctx context.Context, params *cip.AdminCreateUserInput,
		optFns ...cipOpt,
	) (*cip.AdminCreateUserOutput, error)

	resendConfirmationFn func(
		ctx context.Context, params *cip.ResendConfirmationCodeInput,
		optFns ...cipOpt,
	) (*cip.ResendConfirmationCodeOutput, error)

	forgotPasswordFn func(
		ctx context.Context, params *cip.ForgotPasswordInput,
		optFns ...cipOpt,
	) (*cip.ForgotPasswordOutput, error)

	confirmForgotPasswordFn func(
		ctx context.Context, params *cip.ConfirmForgotPasswordInput,
		optFns ...cipOpt,
	) (*cip.ConfirmForgotPasswordOutput, error)

	respondToAuthChallengeFn func(
		ctx context.Context, params *cip.RespondToAuthChallengeInput,
		optFns ...cipOpt,
	) (*cip.RespondToAuthChallengeOutput, error)

	changePasswordFn func(
		ctx context.Context, params *cip.ChangePasswordInput,
		optFns ...cipOpt,
	) (*cip.ChangePasswordOutput, error)

	adminSetUserPasswordFn func(
		ctx context.Context, params *cip.AdminSetUserPasswordInput,
		optFns ...cipOpt,
	) (*cip.AdminSetUserPasswordOutput, error)

	adminGetUserFn func(
		ctx context.Context, params *cip.AdminGetUserInput,
		optFns ...cipOpt,
	) (*cip.AdminGetUserOutput, error)
	adminUpdateUserAttributesFn func(
		ctx context.Context, params *cip.AdminUpdateUserAttributesInput,
		optFns ...cipOpt,
	) (*cip.AdminUpdateUserAttributesOutput, error)
	listUsersFn func(
		ctx context.Context, params *cip.ListUsersInput,
		optFns ...cipOpt,
	) (*cip.ListUsersOutput, error)
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

func (m *mockCognitoAPI) AdminCreateUser(
	ctx context.Context,
	params *cip.AdminCreateUserInput,
	optFns ...func(*cip.Options),
) (*cip.AdminCreateUserOutput, error) {
	if m.adminCreateUserFn != nil {
		return m.adminCreateUserFn(ctx, params, optFns...)
	}
	return &cip.AdminCreateUserOutput{}, nil
}

func (m *mockCognitoAPI) ResendConfirmationCode(
	ctx context.Context,
	params *cip.ResendConfirmationCodeInput,
	optFns ...func(*cip.Options),
) (*cip.ResendConfirmationCodeOutput, error) {
	if m.resendConfirmationFn != nil {
		return m.resendConfirmationFn(ctx, params, optFns...)
	}
	return &cip.ResendConfirmationCodeOutput{}, nil
}

func (m *mockCognitoAPI) ForgotPassword(
	ctx context.Context,
	params *cip.ForgotPasswordInput,
	optFns ...func(*cip.Options),
) (*cip.ForgotPasswordOutput, error) {
	if m.forgotPasswordFn != nil {
		return m.forgotPasswordFn(ctx, params, optFns...)
	}
	return &cip.ForgotPasswordOutput{}, nil
}

func (m *mockCognitoAPI) ConfirmForgotPassword(
	ctx context.Context,
	params *cip.ConfirmForgotPasswordInput,
	optFns ...func(*cip.Options),
) (*cip.ConfirmForgotPasswordOutput, error) {
	if m.confirmForgotPasswordFn != nil {
		return m.confirmForgotPasswordFn(ctx, params, optFns...)
	}
	return &cip.ConfirmForgotPasswordOutput{}, nil
}

func (m *mockCognitoAPI) ChangePassword(
	ctx context.Context,
	params *cip.ChangePasswordInput,
	optFns ...func(*cip.Options),
) (*cip.ChangePasswordOutput, error) {
	if m.changePasswordFn != nil {
		return m.changePasswordFn(ctx, params, optFns...)
	}
	return &cip.ChangePasswordOutput{}, nil
}

func (m *mockCognitoAPI) RespondToAuthChallenge(
	ctx context.Context,
	params *cip.RespondToAuthChallengeInput,
	optFns ...func(*cip.Options),
) (*cip.RespondToAuthChallengeOutput, error) {
	if m.respondToAuthChallengeFn != nil {
		return m.respondToAuthChallengeFn(ctx, params, optFns...)
	}
	return &cip.RespondToAuthChallengeOutput{}, nil
}

func (m *mockCognitoAPI) AdminSetUserPassword(
	ctx context.Context,
	params *cip.AdminSetUserPasswordInput,
	optFns ...func(*cip.Options),
) (*cip.AdminSetUserPasswordOutput, error) {
	if m.adminSetUserPasswordFn != nil {
		return m.adminSetUserPasswordFn(ctx, params, optFns...)
	}
	return &cip.AdminSetUserPasswordOutput{}, nil
}

func (m *mockCognitoAPI) AdminGetUser(
	ctx context.Context,
	params *cip.AdminGetUserInput,
	optFns ...func(*cip.Options),
) (*cip.AdminGetUserOutput, error) {
	if m.adminGetUserFn != nil {
		return m.adminGetUserFn(ctx, params, optFns...)
	}
	return &cip.AdminGetUserOutput{}, nil
}

func (m *mockCognitoAPI) AdminUpdateUserAttributes(
	ctx context.Context,
	params *cip.AdminUpdateUserAttributesInput,
	optFns ...func(*cip.Options),
) (*cip.AdminUpdateUserAttributesOutput, error) {
	if m.adminUpdateUserAttributesFn != nil {
		return m.adminUpdateUserAttributesFn(ctx, params, optFns...)
	}
	return &cip.AdminUpdateUserAttributesOutput{}, nil
}

func (m *mockCognitoAPI) ListUsers(
	ctx context.Context,
	params *cip.ListUsersInput,
	optFns ...func(*cip.Options),
) (*cip.ListUsersOutput, error) {
	if m.listUsersFn != nil {
		return m.listUsersFn(ctx, params, optFns...)
	}
	return &cip.ListUsersOutput{}, nil
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

func TestCognitoClient_Login_NewPasswordRequiredChallenge(t *testing.T) {
	mock := &mockCognitoAPI{
		initiateAuthFn: func(
			_ context.Context,
			_ *cip.InitiateAuthInput,
			_ ...func(*cip.Options),
		) (*cip.InitiateAuthOutput, error) {
			return &cip.InitiateAuthOutput{
				ChallengeName: types.ChallengeNameTypeNewPasswordRequired,
				Session:       strPtr("session-token-xyz"),
			}, nil
		},
	}
	client := newTestClient(mock)

	result, err := client.Login(
		context.Background(), "user@example.com", "Temp-PW-1!",
	)
	if err != nil {
		t.Fatalf("expected no error on challenge, got %v", err)
	}
	if result == nil {
		t.Fatal("expected non-nil result on challenge")
	}
	if result.ChallengeName != string(types.ChallengeNameTypeNewPasswordRequired) {
		t.Errorf("expected ChallengeName=%s, got %q",
			types.ChallengeNameTypeNewPasswordRequired,
			result.ChallengeName,
		)
	}
	if result.Session != "session-token-xyz" {
		t.Errorf("expected Session=session-token-xyz, got %q", result.Session)
	}
	if result.AccessToken != "" || result.IDToken != "" || result.RefreshToken != "" {
		t.Errorf("expected no tokens on challenge, got %+v", result)
	}
}

// --------------- CompleteNewPassword tests ---------------

func TestCognitoClient_CompleteNewPassword_Success(t *testing.T) {
	var capturedInput *cip.RespondToAuthChallengeInput
	mock := &mockCognitoAPI{
		respondToAuthChallengeFn: func(
			_ context.Context,
			params *cip.RespondToAuthChallengeInput,
			_ ...func(*cip.Options),
		) (*cip.RespondToAuthChallengeOutput, error) {
			capturedInput = params
			return &cip.RespondToAuthChallengeOutput{
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

	result, err := client.CompleteNewPassword(
		context.Background(),
		"user@example.com", "NewP@ssw0rd!", "session-token-xyz",
		"Jane", "Doe",
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
	if capturedInput == nil {
		t.Fatal("expected RespondToAuthChallenge to be called")
	}
	if capturedInput.ChallengeName != types.ChallengeNameTypeNewPasswordRequired {
		t.Errorf("expected ChallengeName=NEW_PASSWORD_REQUIRED, got %s",
			capturedInput.ChallengeName,
		)
	}
	if capturedInput.Session == nil || *capturedInput.Session != "session-token-xyz" {
		t.Errorf("expected Session=session-token-xyz, got %v", capturedInput.Session)
	}
	if capturedInput.ChallengeResponses["USERNAME"] != "user@example.com" {
		t.Errorf("expected USERNAME=user@example.com, got %q",
			capturedInput.ChallengeResponses["USERNAME"],
		)
	}
	if capturedInput.ChallengeResponses["NEW_PASSWORD"] != "NewP@ssw0rd!" {
		t.Errorf("expected NEW_PASSWORD set, got %q",
			capturedInput.ChallengeResponses["NEW_PASSWORD"],
		)
	}
	// Required-attribute pass-through: Cognito demands these whenever
	// the User Pool marks given_name / family_name as required and
	// the admin-created user does not have them yet.
	if capturedInput.ChallengeResponses["userAttributes.given_name"] != "Jane" {
		t.Errorf("expected userAttributes.given_name=Jane, got %q",
			capturedInput.ChallengeResponses["userAttributes.given_name"],
		)
	}
	if capturedInput.ChallengeResponses["userAttributes.family_name"] != "Doe" {
		t.Errorf("expected userAttributes.family_name=Doe, got %q",
			capturedInput.ChallengeResponses["userAttributes.family_name"],
		)
	}
}

func TestCognitoClient_CompleteNewPassword_OmitsEmptyNames(t *testing.T) {
	var capturedInput *cip.RespondToAuthChallengeInput
	mock := &mockCognitoAPI{
		respondToAuthChallengeFn: func(
			_ context.Context,
			params *cip.RespondToAuthChallengeInput,
			_ ...func(*cip.Options),
		) (*cip.RespondToAuthChallengeOutput, error) {
			capturedInput = params
			return &cip.RespondToAuthChallengeOutput{
				AuthenticationResult: &types.AuthenticationResultType{
					AccessToken:  strPtr("access-tok"),
					IdToken:      strPtr("id-tok"),
					RefreshToken: strPtr("refresh-tok"),
				},
			}, nil
		},
	}
	client := newTestClient(mock)

	_, err := client.CompleteNewPassword(
		context.Background(),
		"user@example.com", "NewP@ssw0rd!", "session-token-xyz",
		"", "  ",
	)
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
	if _, ok := capturedInput.ChallengeResponses["userAttributes.given_name"]; ok {
		t.Error("expected userAttributes.given_name to be omitted when empty")
	}
	if _, ok := capturedInput.ChallengeResponses["userAttributes.family_name"]; ok {
		t.Error("expected userAttributes.family_name to be omitted when whitespace only")
	}
}

func TestCognitoClient_CompleteNewPassword_InvalidPassword(t *testing.T) {
	mock := &mockCognitoAPI{
		respondToAuthChallengeFn: func(
			_ context.Context,
			_ *cip.RespondToAuthChallengeInput,
			_ ...func(*cip.Options),
		) (*cip.RespondToAuthChallengeOutput, error) {
			return nil, &types.InvalidPasswordException{
				Message: strPtr("password too weak"),
			}
		},
	}
	client := newTestClient(mock)

	_, err := client.CompleteNewPassword(
		context.Background(),
		"user@example.com", "weak", "session-token-xyz",
		"Jane", "Doe",
	)
	if !errors.Is(err, auth.ErrInvalidPassword) {
		t.Errorf("expected ErrInvalidPassword, got %v", err)
	}
}

func TestCognitoClient_CompleteNewPassword_MissingArgs(t *testing.T) {
	mock := &mockCognitoAPI{}
	client := newTestClient(mock)

	tests := []struct {
		name     string
		email    string
		password string
		session  string
	}{
		{"empty email", "", "P@ssw0rd!", "session"},
		{"empty password", "u@example.com", "", "session"},
		{"empty session", "u@example.com", "P@ssw0rd!", ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := client.CompleteNewPassword(
				context.Background(),
				tc.email, tc.password, tc.session,
				"Jane", "Doe",
			)
			if err == nil {
				t.Error("expected validation error, got nil")
			}
		})
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

// --------------- GetUserStatus tests ---------------

func TestCognitoClient_GetUserStatus_Success(t *testing.T) {
	var captured *cip.AdminGetUserInput
	mock := &mockCognitoAPI{
		adminGetUserFn: func(
			_ context.Context,
			params *cip.AdminGetUserInput,
			_ ...func(*cip.Options),
		) (*cip.AdminGetUserOutput, error) {
			captured = params
			return &cip.AdminGetUserOutput{
				UserStatus: types.UserStatusTypeForceChangePassword,
			}, nil
		},
	}
	client := newTestClient(mock)

	status, err := client.GetUserStatus(context.Background(), "user@example.com")
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
	if status != auth.UserStatusForceChangePassword {
		t.Errorf("expected status %s, got %q", auth.UserStatusForceChangePassword, status)
	}
	if captured == nil {
		t.Fatal("expected AdminGetUser to be called")
	}
	if captured.UserPoolId == nil || *captured.UserPoolId != "test-pool" {
		t.Errorf("expected UserPoolId=test-pool, got %v", captured.UserPoolId)
	}
	if captured.Username == nil || *captured.Username != "user@example.com" {
		t.Errorf("expected Username=user@example.com, got %v", captured.Username)
	}
}

func TestCognitoClient_GetUserStatus_NotFound(t *testing.T) {
	mock := &mockCognitoAPI{
		adminGetUserFn: func(
			_ context.Context,
			_ *cip.AdminGetUserInput,
			_ ...func(*cip.Options),
		) (*cip.AdminGetUserOutput, error) {
			return nil, &types.UserNotFoundException{
				Message: strPtr("user does not exist"),
			}
		},
	}
	client := newTestClient(mock)

	_, err := client.GetUserStatus(context.Background(), "missing@example.com")
	if !errors.Is(err, auth.ErrUserNotFound) {
		t.Errorf("expected ErrUserNotFound, got %v", err)
	}
}

// --------------- ResendInvite tests ---------------

func TestCognitoClient_ResendInvite_Success(t *testing.T) {
	var captured *cip.AdminCreateUserInput
	mock := &mockCognitoAPI{
		adminCreateUserFn: func(
			_ context.Context,
			params *cip.AdminCreateUserInput,
			_ ...func(*cip.Options),
		) (*cip.AdminCreateUserOutput, error) {
			captured = params
			return &cip.AdminCreateUserOutput{}, nil
		},
	}
	client := newTestClient(mock)

	if err := client.ResendInvite(context.Background(), "user@example.com"); err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
	if captured == nil {
		t.Fatal("expected AdminCreateUser to be called")
	}
	// RESEND regenerates the temp password and re-sends the invite, keeping the
	// user in FORCE_CHANGE_PASSWORD. We must NOT supply a temp password (Cognito
	// generates a fresh one) — mirrors the bare `--message-action RESEND` CLI call.
	if captured.MessageAction != types.MessageActionTypeResend {
		t.Errorf("expected MessageAction=RESEND, got %q", captured.MessageAction)
	}
	if captured.TemporaryPassword != nil {
		t.Errorf("expected no TemporaryPassword on resend, got %q", *captured.TemporaryPassword)
	}
	if captured.UserPoolId == nil || *captured.UserPoolId != "test-pool" {
		t.Errorf("expected UserPoolId=test-pool, got %v", captured.UserPoolId)
	}
	if captured.Username == nil || *captured.Username != "user@example.com" {
		t.Errorf("expected Username=user@example.com, got %v", captured.Username)
	}
}

// --------------- expired temp password classification ---------------

func TestCognitoClient_Login_TempPasswordExpired(t *testing.T) {
	mock := &mockCognitoAPI{
		initiateAuthFn: func(
			_ context.Context,
			_ *cip.InitiateAuthInput,
			_ ...func(*cip.Options),
		) (*cip.InitiateAuthOutput, error) {
			return nil, &types.NotAuthorizedException{
				Message: strPtr("Temporary password has expired and must be reset by an administrator."),
			}
		},
	}
	client := newTestClient(mock)

	_, err := client.Login(context.Background(), "user@example.com", "Expired-1!")
	if !errors.Is(err, auth.ErrTempPasswordExpired) {
		t.Errorf("expected ErrTempPasswordExpired, got %v", err)
	}
}

func TestCognitoClient_Login_WrongPasswordNotExpired(t *testing.T) {
	// A plain wrong password must NOT be classified as expired (otherwise a typo
	// on a still-valid temp password would trigger the rescue).
	mock := &mockCognitoAPI{
		initiateAuthFn: func(
			_ context.Context,
			_ *cip.InitiateAuthInput,
			_ ...func(*cip.Options),
		) (*cip.InitiateAuthOutput, error) {
			return nil, &types.NotAuthorizedException{
				Message: strPtr("Incorrect username or password."),
			}
		},
	}
	client := newTestClient(mock)

	_, err := client.Login(context.Background(), "user@example.com", "wrong")
	if !errors.Is(err, auth.ErrNotAuthorized) {
		t.Errorf("expected ErrNotAuthorized, got %v", err)
	}
	if errors.Is(err, auth.ErrTempPasswordExpired) {
		t.Error("a wrong password must not be classified as expired")
	}
}
