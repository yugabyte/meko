package service

import (
	"context"
	"errors"

	"meko/api_server/internal/auth"
)

// ErrUserStoreNotSupported is returned by every CognitoProvider method when the
// deployment authenticates against a generic OIDC issuer. The issuer owns its
// users, so creating one, resetting a password or verifying an email address is
// its job and happens on its pages.
//
// The handlers for those operations are not registered under OIDC (see
// registerOIDCAuthRoutes), so this is a backstop for the few paths AuthService
// reaches opportunistically — UpdateProfile's best-effort name sync is the live
// one — not an error any route returns.
var ErrUserStoreNotSupported = errors.New(
	"user management is handled by the identity provider, not by this server",
)

// NoUserStore is the CognitoProvider for a deployment that has no password
// database of its own.
//
// It exists so AuthService keeps one shape across providers instead of a nil
// check at seventeen call sites. When Cognito is dropped, CognitoProvider and
// this type are deleted together and AuthService loses the field — nothing else
// has to change, which is the point of routing every password operation through
// one interface.
type NoUserStore struct{}

// Compile-time proof that this stays in step with the interface: adding a
// method to CognitoProvider fails the build here rather than at runtime.
var _ CognitoProvider = NoUserStore{}

func (NoUserStore) SignUp(context.Context, string, string, string, string) (string, error) {
	return "", ErrUserStoreNotSupported
}
func (NoUserStore) ConfirmSignUp(context.Context, string, string) error {
	return ErrUserStoreNotSupported
}
func (NoUserStore) ResendConfirmationCode(context.Context, string) error {
	return ErrUserStoreNotSupported
}
func (NoUserStore) ForgotPassword(context.Context, string) error {
	return ErrUserStoreNotSupported
}
func (NoUserStore) ConfirmForgotPassword(context.Context, string, string, string) error {
	return ErrUserStoreNotSupported
}
func (NoUserStore) GetUserStatus(context.Context, string) (auth.UserStatus, error) {
	return "", ErrUserStoreNotSupported
}
func (NoUserStore) ResetTempPassword(context.Context, string, string) error {
	return ErrUserStoreNotSupported
}
func (NoUserStore) ResendInvite(context.Context, string) error {
	return ErrUserStoreNotSupported
}
func (NoUserStore) SetPermanentPassword(context.Context, string, string) error {
	return ErrUserStoreNotSupported
}
func (NoUserStore) Login(context.Context, string, string) (*auth.Result, error) {
	return nil, ErrUserStoreNotSupported
}
func (NoUserStore) CompleteNewPassword(
	context.Context, string, string, string, string, string,
) (*auth.Result, error) {
	return nil, ErrUserStoreNotSupported
}
func (NoUserStore) Logout(context.Context, string) error {
	return ErrUserStoreNotSupported
}
func (NoUserStore) ChangePassword(context.Context, string, string, string) error {
	return ErrUserStoreNotSupported
}
func (NoUserStore) UpdateUserName(context.Context, string, string, string) error {
	return ErrUserStoreNotSupported
}
func (NoUserStore) UpdateEmailAttribute(context.Context, string, string) error {
	return ErrUserStoreNotSupported
}
func (NoUserStore) SendEmailAttributeCode(context.Context, string) error {
	return ErrUserStoreNotSupported
}
func (NoUserStore) VerifyEmailAttribute(context.Context, string, string) error {
	return ErrUserStoreNotSupported
}
