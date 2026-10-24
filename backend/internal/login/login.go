// Package login is the owner's sign-in for the web listener with remote access: the login itself
// (internal/auth's hashes and TOTP, kept in internal/store), browser sessions behind the meko_session
// cookie, the setup link meko setup_link makes, and the rate limit on failed attempts. It follows
// AgentK's login, which it replaced, so an AgentK owner Meko imports signs in as before.
package login

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"sync"
	"time"

	"github.com/yugabyte/meko/backend/internal/auth"
	"github.com/yugabyte/meko/backend/internal/store"
)

// Timings, as AgentK had them.
const (
	SessionTTL     = 14 * 24 * time.Hour
	renewAfter     = time.Hour // a session's expiry slides forward at most this often
	SetupLinkTTL   = 7 * 24 * time.Hour
	keySetupToken  = "login_setup_token" //nolint:gosec // G101: a settings key, not a credential
	keyTOTPLast    = "login_totp_last_step"
	totpIssuer     = "Meko"
	sessionByteLen = 32
	passwordChecks = 2 // password checks at once; a sign-in past them is told to try again
)

// Errors a caller turns into replies.
var (
	ErrBadLogin     = errors.New("wrong username or password")
	ErrTOTPRequired = errors.New("enter the code from your authenticator app")
	ErrBadTOTP      = errors.New("that code is wrong or expired")
	ErrRateLimited  = errors.New("too many failed attempts: try again in 15 minutes")
	ErrBusy         = errors.New("too many sign-ins at once: try again in a moment")
	ErrBadSetupLink = errors.New("this setup link is invalid or already used: " +
		"run meko setup_link on the host for a new one")
	ErrNoOwner     = errors.New("no login is set up: run meko setup_link on the host to make one")
	ErrBadPassword = errors.New("the current password is wrong")
)

// Service is the sign-in. Its methods are safe to call at once.
type Service struct {
	st      *store.Store
	limiter *auth.LoginLimiter
	now     func() time.Time
	// setupMu keeps a setup link's check and its use from racing, and totpMu TOTP's replay check.
	setupMu, totpMu sync.Mutex
	// checking holds a slot per password check under way: each takes 32 MiB of scrypt, so a burst
	// of sign-ins mustn't run them all at once.
	checking chan struct{}
	// onRevoke, if set, hears that sessions ended (a sign-out, a password change, a new login), so the
	// server can close the WebSockets they opened.
	onRevoke func()
}

// New returns the sign-in over st.
func New(st *store.Store) *Service {
	return &Service{st: st, limiter: auth.NewLoginLimiter(), now: time.Now, checking: make(chan struct{}, passwordChecks)}
}

// OnRevoke sets what's called after sessions end.
func (s *Service) OnRevoke(f func()) { s.onRevoke = f }

func (s *Service) revoked() {
	if s.onRevoke != nil {
		s.onRevoke()
	}
}

// Hash is what a session's cookie is kept and tracked as.
func Hash(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// Owner is the login, or nil.
func (s *Service) Owner(ctx context.Context) (*store.Owner, error) { return s.st.Owner(ctx) }

// Session checks a cookie. It reports whether it's signed in, and renews its expiry (at most hourly);
// renewed says the cookie's Max-Age should be sent again.
func (s *Service) Session(ctx context.Context, token string) (ok, renewed bool, err error) {
	if token == "" {
		return false, false, nil
	}
	h := Hash(token)
	exp, ok, err := s.st.AuthSessionExpiry(ctx, h)
	now := s.now()
	if err != nil || !ok || now.After(exp) {
		return false, false, err
	}
	if exp.Sub(now) < SessionTTL-renewAfter {
		if err := s.st.RenewAuthSession(ctx, h, now.Add(SessionTTL)); err != nil {
			return false, false, err
		}
		return true, true, nil
	}
	return true, false, nil
}

// Live reports whether a session (by its hash) is still signed in, without renewing it.
func (s *Service) Live(ctx context.Context, hash string) bool {
	exp, ok, err := s.st.AuthSessionExpiry(ctx, hash)
	return err == nil && ok && s.now().Before(exp)
}

// Login checks a sign-in from ip and returns a new session's cookie. The attempt counts as failed
// from the start (LoginLimiter.Take), and only a right one takes that back, so a burst of requests
// gets no more guesses than one at a time; and at most passwordChecks run scrypt at once.
func (s *Service) Login(ctx context.Context, username, password, totp, ip, userAgent string) (string, error) {
	now := s.now()
	if !s.limiter.Take(ip, now) {
		return "", ErrRateLimited
	}
	if !s.slot() {
		s.limiter.Refund(ip, now)
		return "", ErrBusy
	}
	ok, o, err := s.checkPassword(ctx, username, password)
	<-s.checking
	if err != nil {
		s.limiter.Refund(ip, now)
		return "", err
	}
	if !ok {
		return "", ErrBadLogin
	}
	if o.TOTPSecret != "" {
		if totp == "" {
			s.limiter.Refund(ip, now) // the password was right: asking for the code isn't a failure
			return "", ErrTOTPRequired
		}
		ok, err := s.acceptTOTP(ctx, o.TOTPSecret, totp)
		if err != nil {
			s.limiter.Refund(ip, now)
			return "", err
		}
		if !ok {
			return "", ErrBadTOTP
		}
	}
	s.limiter.Refund(ip, now)
	s.limiter.Reset(ip)
	return s.newSession(ctx, ip, userAgent)
}

// slot takes one of the passwordChecks slots for a scrypt run, or reports there's none free; the
// caller gives it back with <-s.checking.
func (s *Service) slot() bool {
	select {
	case s.checking <- struct{}{}:
		return true
	default:
		return false
	}
}

// checkPassword checks username and password. A password is checked either way, so the time taken
// doesn't say whether the username was right.
func (s *Service) checkPassword(ctx context.Context, username, password string) (bool, *store.Owner, error) {
	o, err := s.st.Owner(ctx)
	if err != nil {
		return false, nil, err
	}
	if o == nil || subtle.ConstantTimeCompare([]byte(o.Username), []byte(username)) != 1 {
		auth.BurnPasswordCheck(password)
		return false, nil, nil
	}
	return auth.VerifyPassword(password, o.PWScrypt), o, nil
}

// Logout ends a session.
func (s *Service) Logout(ctx context.Context, token string) error {
	if token == "" {
		return nil
	}
	if err := s.st.DeleteAuthSession(ctx, Hash(token)); err != nil {
		return err
	}
	s.revoked()
	return nil
}

// IssueSetupLink makes a new one-time setup token, replacing any outstanding one. Using it creates
// the owner, or replaces the login when there is one (the way back in after a lost password).
func (s *Service) IssueSetupLink(ctx context.Context) (string, error) {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	token := base64.RawURLEncoding.EncodeToString(b)
	v, err := json.Marshal(setupToken{SHA256: Hash(token), ExpiresAt: s.now().Add(SetupLinkTTL)})
	if err != nil {
		return "", err
	}
	return token, s.st.SetSetting(ctx, keySetupToken, string(v))
}

type setupToken struct {
	SHA256    string    `json:"sha256"`
	ExpiresAt time.Time `json:"expires_at"`
}

// SetupOutstanding reports whether an unused, unexpired setup link exists.
func (s *Service) SetupOutstanding(ctx context.Context) bool {
	t, err := s.liveSetupToken(ctx)
	return err == nil && t != nil
}

func (s *Service) liveSetupToken(ctx context.Context) (*setupToken, error) {
	raw, ok, err := s.st.Setting(ctx, keySetupToken)
	if err != nil || !ok {
		return nil, err
	}
	var t setupToken
	if json.Unmarshal([]byte(raw), &t) != nil || !s.now().Before(t.ExpiresAt) {
		return nil, s.st.DeleteSetting(ctx, keySetupToken)
	}
	return &t, nil
}

func (s *Service) setupTokenValid(ctx context.Context, token string) (bool, error) {
	if token == "" {
		return false, nil
	}
	t, err := s.liveSetupToken(ctx)
	if err != nil || t == nil {
		return false, err
	}
	return subtle.ConstantTimeCompare([]byte(t.SHA256), []byte(Hash(token))) == 1, nil
}

// Setup uses a setup link: it sets the login (two-step sign-in off, every browser signed out) and
// returns a session's cookie. A valid link isn't held back by the rate limit, so a locked-out owner
// gets in at once; the token is 192 random bits, so that helps no one guess it. A bad username or
// password comes back as a CheckError before the link is used.
func (s *Service) Setup(ctx context.Context, token, username, password, ip, userAgent string) (string, error) {
	s.setupMu.Lock()
	defer s.setupMu.Unlock()
	now := s.now()
	valid, err := s.setupTokenValid(ctx, token)
	if err != nil {
		return "", err
	}
	if !valid && s.limiter.Blocked(ip, now) {
		return "", ErrRateLimited
	}
	if msg := auth.CheckUsername(username); msg != "" {
		return "", CheckError(msg)
	}
	if msg := auth.CheckPassword(password); msg != "" {
		return "", CheckError(msg)
	}
	if !valid {
		s.limiter.Fail(ip, now)
		return "", ErrBadSetupLink
	}
	if !s.slot() {
		return "", ErrBusy
	}
	hash, err := auth.HashPassword(password)
	<-s.checking
	if err != nil {
		return "", err
	}
	if err := s.st.DeleteSetting(ctx, keySetupToken); err != nil {
		return "", err
	}
	if err := s.st.SetOwner(ctx, username, hash); err != nil {
		return "", err
	}
	s.revoked()
	return s.newSession(ctx, ip, userAgent)
}

// CheckError is a username or password a login refuses, with what to fix.
type CheckError string

func (e CheckError) Error() string { return string(e) }

// ChangePassword changes the password and signs out every browser but the one with keepToken.
func (s *Service) ChangePassword(ctx context.Context, current, next, keepToken, ip string) error {
	o, err := s.st.Owner(ctx)
	if err != nil {
		return err
	}
	if o == nil {
		return ErrNoOwner
	}
	if msg := auth.CheckPassword(next); msg != "" {
		return CheckError(msg)
	}
	// The current password is a guess like a sign-in's: counted, and checked in a scrypt slot.
	now := s.now()
	if !s.limiter.Take(ip, now) {
		return ErrRateLimited
	}
	if !s.slot() {
		s.limiter.Refund(ip, now)
		return ErrBusy
	}
	ok := auth.VerifyPassword(current, o.PWScrypt)
	var hash string
	if ok {
		hash, err = auth.HashPassword(next)
	}
	<-s.checking
	if !ok {
		return ErrBadPassword
	}
	s.limiter.Refund(ip, now)
	if err != nil {
		return err
	}
	keep := ""
	if keepToken != "" {
		keep = Hash(keepToken)
	}
	if err := s.st.SetPassword(ctx, hash, keep); err != nil {
		return err
	}
	s.revoked()
	return nil
}

// EnrollTOTP starts two-step sign-in: a new secret, kept pending until a code from it confirms it.
func (s *Service) EnrollTOTP(ctx context.Context) (secret, otpauthURL string, err error) {
	o, err := s.st.Owner(ctx)
	if err != nil {
		return "", "", err
	}
	if o == nil {
		return "", "", ErrNoOwner
	}
	if secret, err = auth.NewTOTPSecret(); err != nil {
		return "", "", err
	}
	if err = s.st.SetTOTPPending(ctx, secret); err != nil {
		return "", "", err
	}
	return secret, auth.OTPAuthURL(totpIssuer, o.Username, secret), nil
}

// ConfirmTOTP turns two-step sign-in on when code matches the pending secret.
func (s *Service) ConfirmTOTP(ctx context.Context, code string) error {
	o, err := s.st.Owner(ctx)
	if err != nil {
		return err
	}
	if o == nil {
		return ErrNoOwner
	}
	if o.TOTPPending == "" {
		return ErrBadTOTP
	}
	ok, err := s.acceptTOTP(ctx, o.TOTPPending, code)
	if err != nil {
		return err
	}
	if !ok {
		return ErrBadTOTP
	}
	return s.st.ConfirmTOTP(ctx)
}

// DisableTOTP turns two-step sign-in off when code matches.
func (s *Service) DisableTOTP(ctx context.Context, code string) error {
	o, err := s.st.Owner(ctx)
	if err != nil {
		return err
	}
	if o == nil {
		return ErrNoOwner
	}
	if o.TOTPSecret == "" {
		return ErrBadTOTP
	}
	ok, err := s.acceptTOTP(ctx, o.TOTPSecret, code)
	if err != nil {
		return err
	}
	if !ok {
		return ErrBadTOTP
	}
	return s.st.DisableTOTP(ctx)
}

// acceptTOTP checks a code and uses it up: a step at or before the last one accepted for this secret
// is refused, so a code seen over someone's shoulder can't be used again within its window.
func (s *Service) acceptTOTP(ctx context.Context, secret, code string) (bool, error) {
	step, ok := auth.TOTPMatch(secret, code, s.now())
	if !ok {
		return false, nil
	}
	s.totpMu.Lock()
	defer s.totpMu.Unlock()
	key := Hash(secret)
	var last struct {
		Key  string `json:"key"`
		Step int64  `json:"step"`
	}
	if raw, found, err := s.st.Setting(ctx, keyTOTPLast); err != nil {
		return false, err
	} else if found && json.Unmarshal([]byte(raw), &last) == nil && last.Key == key && step <= last.Step {
		return false, nil
	}
	last.Key, last.Step = key, step
	v, err := json.Marshal(last)
	if err != nil {
		return false, err
	}
	return true, s.st.SetSetting(ctx, keyTOTPLast, string(v))
}

func (s *Service) newSession(ctx context.Context, ip, userAgent string) (string, error) {
	b := make([]byte, sessionByteLen)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	token := base64.RawURLEncoding.EncodeToString(b)
	now := s.now()
	if len(userAgent) > 512 {
		userAgent = userAgent[:512]
	}
	if err := s.st.CreateAuthSession(ctx, Hash(token), userAgent, ip, now, now.Add(SessionTTL)); err != nil {
		return "", err
	}
	return token, nil
}

// ImportOwner adds an owner from AgentK when Meko has none. It reports whether it did.
func (s *Service) ImportOwner(ctx context.Context, o store.Owner) (bool, error) {
	if o.Username == "" || o.PWScrypt == "" {
		return false, nil
	}
	return s.st.ImportOwner(ctx, o)
}
