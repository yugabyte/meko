package auth

// TokenValidator verifies a bearer credential and returns the claims the auth
// middleware needs: a subject to resolve an account by, and an expiry the
// verifier has already enforced.
//
// This is the seam between the API surface and whichever IdP issued the token.
// It is narrow on purpose — the middleware wants an identity and nothing else,
// so issuers are interchangeable here even though user management is not.
// *JWTValidator implements it for Cognito access tokens (which carry a
// Cognito-only token_use claim); *OIDCValidator implements it for any OIDC
// issuer's ID tokens. When Cognito goes, the interface and every caller stay
// exactly as they are and one implementation is deleted.
type TokenValidator interface {
	ValidateAccessToken(token string) (*AccessTokenClaims, error)
}

// Both implementations are checked at compile time rather than at the first
// request, so removing a method from either is a build failure here.
var (
	_ TokenValidator = (*JWTValidator)(nil)
	_ TokenValidator = (*OIDCValidator)(nil)
)
