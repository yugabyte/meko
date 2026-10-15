package models

import "time"

// PasswordResetRequest is one row of meko_system.password_reset_requests: a
// forgot-password request for an email. CodeHash is empty when Cognito sent
// the reset code; it is set when Meko issued its own code to an invitee who
// never finished first sign-in (MEKO-887).
type PasswordResetRequest struct {
	ExpiresAt   *time.Time
	UsedAt      *time.Time
	RequestedAt time.Time
	ID          string
	Email       string
	CodeHash    string
	Attempts    int
}
