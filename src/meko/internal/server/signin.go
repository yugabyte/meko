package server

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
	"unicode"

	"github.com/labstack/echo/v4"

	"github.com/yugabyte/meko/src/meko/internal/cloudlink"
	"github.com/yugabyte/meko/src/meko/internal/config"
	"github.com/yugabyte/meko/src/meko/internal/envelope"
)

// Sign-in through Meko Cloud for remote access ([web] signin = "cloud", docs/web.md and
// docs/cloud.md): a browser on another computer gets the remote credential only with a session
// cookie, which it gets by proving, through the linked Meko Cloud, that it's signed in to the
// account this Meko is linked to (cloudlink's StartBrowserSignIn and FinishBrowserSignIn). Meko
// Cloud sends the browser back to the web UI's /signin/callback page, not to a route, since the
// browsers' checks refuse an API call another site sends; the page then POSTs finish from here.

// Sessions keeps the browsers' sessions (internal/store), by the SHA-256 of their cookie.
type Sessions interface {
	CreateSession(ctx context.Context, tokenSHA256 string, expires time.Time, userAgent, ip string) error
	SessionValid(ctx context.Context, tokenSHA256 string, now time.Time) (bool, error)
	DeleteSession(ctx context.Context, tokenSHA256 string) error
}

// SessionCookie holds a browser's session. __Host- makes the browser take it only from this host,
// over HTTPS, for every path, so no sibling host can set one.
const SessionCookie = "__Host-meko_session"

// SessionLength is how long a session lasts. Signing out ends it sooner, and a change to the Meko
// Cloud link ends them all.
const SessionLength = 7 * 24 * time.Hour

// signInBrowserCookie ties a sign-in to the browser that started it: a random nonce start sets and
// finish requires, so a callback address (code and state) copied before it's used signs no one else
// in. It lives as long as a started sign-in waits.
const signInBrowserCookie = "__Host-meko_signin"

// SignInCallbackPath is the web UI's page Meko Cloud sends a signing-in browser back to.
const SignInCallbackPath = "/signin/callback"

// The sign-in's routes, which the credential check lets through (start, finish) or holds to a
// browser's (signout).
const (
	signInStartRoute  = "/api/v1/auth/signin/start"
	signInFinishRoute = "/api/v1/auth/signin/finish"
	signOutRoute      = "/api/v1/auth/signout"
)

// signInRequired says whether a browser on another computer must sign in: remote access with
// [web] signin = "cloud", while the web UI is on.
func (s *Server) signInRequired() bool {
	w := s.opts.Web
	return w != nil && !w.Off && w.Access == config.AccessRemote && w.SignIn
}

func (s *Server) registerSignIn(v1 *echo.Group) {
	if !s.signInRequired() || s.opts.Cloud == nil {
		return
	}
	v1.POST("/auth/signin/start", s.signInStart)
	v1.POST("/auth/signin/finish", s.signInFinish)
	v1.POST("/auth/signout", s.signOut, ownerOnly)
}

// signedIn says whether r carries a session cookie that's still valid.
func (s *Server) signedIn(r *http.Request) bool {
	ck, err := r.Cookie(SessionCookie)
	if err != nil || ck.Value == "" || s.opts.Web.Sessions == nil {
		return false
	}
	ok, err := s.opts.Web.Sessions.SessionValid(r.Context(), tokenSHA256(ck.Value), time.Now())
	if err != nil {
		s.opts.Logger.Error("reading a browser session", "error", err)
	}
	return ok
}

func (s *Server) signInStart(c echo.Context) error {
	var body struct {
		Next string `json:"next"`
	}
	if err := c.Bind(&body); err != nil {
		return envelope.Fail(c, http.StatusBadRequest, "VALIDATION_ERROR", "Send a JSON body.")
	}
	if refused, err := s.refuseSignIn(c); refused {
		return err
	}
	nonce, err := randomToken()
	if err != nil {
		return err
	}
	to, err := s.opts.Cloud.StartBrowserSignIn(c.Request().Context(),
		strings.TrimRight(s.opts.Web.Address, "/")+SignInCallbackPath, safeNext(body.Next), tokenSHA256(nonce))
	if err != nil {
		return signInError(c, err)
	}
	c.SetCookie(&http.Cookie{Name: signInBrowserCookie, Value: nonce, Path: "/",
		MaxAge: int(cloudlink.BrowserSignInWait / time.Second), Secure: true, HttpOnly: true,
		SameSite: http.SameSiteLaxMode})
	return envelope.OK(c, map[string]string{"url": to})
}

// refuseSignIn answers a sign-in that isn't over TLS (Meko on its own computer needs none) or that
// came to another address than the one Meko is set up with ([web] name, or this host's FQDN), and
// says it did: the caller then stops. The address Meko Cloud sends the code back to is that one,
// never the request's Host, which the client writes: a Host of another site's would have Meko
// Cloud send the code there.
func (s *Server) refuseSignIn(c echo.Context) (bool, error) {
	r := c.Request()
	if r.TLS == nil {
		return true, envelope.Fail(c, http.StatusBadRequest, "SIGNIN_NOT_NEEDED",
			"Meko on this computer needs no sign-in.")
	}
	if !sameAddress(r.Host, s.opts.Web.Address) {
		return true, envelope.Fail(c, http.StatusBadRequest, "WRONG_ADDRESS",
			"Sign in at "+s.opts.Web.Address+", the address Meko is set up with.")
	}
	return false, nil
}

// sameAddress says whether host (a request's Host) names the host and port of address.
func sameAddress(host, address string) bool {
	u, err := url.Parse(address)
	if err != nil || u.Host == "" {
		return false
	}
	withPort := func(h string) string {
		if _, _, err := net.SplitHostPort(h); err != nil {
			return net.JoinHostPort(strings.Trim(h, "[]"), "443")
		}
		return h
	}
	return strings.EqualFold(withPort(host), withPort(u.Host))
}

// safeNext is where a sign-in goes after: a path on this server, or "/" for anything else (another
// site, a scheme, a protocol-relative or backslashed address, control characters).
func safeNext(next string) string {
	if !strings.HasPrefix(next, "/") || strings.HasPrefix(next, "//") || strings.HasPrefix(next, "/\\") ||
		strings.ContainsFunc(next, unicode.IsControl) {
		return "/"
	}
	if u, err := url.Parse(next); err != nil || u.Scheme != "" || u.Host != "" {
		return "/"
	}
	return next
}

func (s *Server) signInFinish(c echo.Context) error {
	var body struct {
		Code  string `json:"code"`
		State string `json:"state"`
	}
	if err := c.Bind(&body); err != nil {
		return envelope.Fail(c, http.StatusBadRequest, "VALIDATION_ERROR", "Send a JSON body.")
	}
	if refused, err := s.refuseSignIn(c); refused {
		return err
	}
	r := c.Request()
	browser := ""
	if ck, err := r.Cookie(signInBrowserCookie); err == nil && ck.Value != "" {
		browser = tokenSHA256(ck.Value)
	}
	// The nonce is for this one sign-in, whatever it comes to.
	c.SetCookie(&http.Cookie{Name: signInBrowserCookie, Value: "", Path: "/", MaxAge: -1, Secure: true,
		HttpOnly: true, SameSite: http.SameSiteLaxMode})
	got, err := s.opts.Cloud.FinishBrowserSignIn(r.Context(), body.Code, body.State, browser)
	if err != nil {
		return signInError(c, err)
	}
	if !got.SameAccount {
		s.opts.Logger.Info("sign-in refused: another Meko Cloud account")
		return envelope.Fail(c, http.StatusForbidden, "NOT_THE_OWNER",
			"Signed in as someone else: sign in with the Meko Cloud account this Meko is linked to.")
	}
	if s.opts.Web.Sessions == nil {
		return errors.New("no session store")
	}
	cookie, err := randomToken()
	if err != nil {
		return err
	}
	expires := time.Now().Add(SessionLength)
	if err = s.opts.Web.Sessions.CreateSession(r.Context(), tokenSHA256(cookie), expires,
		clip(r.UserAgent(), 256), peerIP(r)); err != nil {
		return err
	}
	// The link may have changed while Meko Cloud was asked: then the owner it named is gone, and the
	// change's end of every session may have run before this one was stored.
	if !s.opts.Cloud.BrowserSignInCurrent(got) {
		if err = s.opts.Web.Sessions.DeleteSession(r.Context(), tokenSHA256(cookie)); err != nil {
			return err
		}
		return signInError(c, cloudlink.ErrSignInExpired)
	}
	c.SetCookie(&http.Cookie{Name: SessionCookie, Value: cookie, Path: "/", MaxAge: int(SessionLength / time.Second),
		Secure: true, HttpOnly: true, SameSite: http.SameSiteLaxMode})
	s.opts.Logger.Info("signed in through Meko Cloud")
	return envelope.OK(c, map[string]string{"next": got.Next})
}

func (s *Server) signOut(c echo.Context) error {
	r := c.Request()
	if ck, err := r.Cookie(SessionCookie); err == nil && ck.Value != "" && s.opts.Web.Sessions != nil {
		if err = s.opts.Web.Sessions.DeleteSession(r.Context(), tokenSHA256(ck.Value)); err != nil {
			return err
		}
	}
	c.SetCookie(&http.Cookie{Name: SessionCookie, Value: "", Path: "/", MaxAge: -1,
		Secure: true, HttpOnly: true, SameSite: http.SameSiteLaxMode})
	return envelope.OK(c, map[string]bool{"signed_out": true})
}

// signInError answers a sign-in that couldn't go on: an expired one, or Meko Cloud's refusal with
// its status (a 502 when it didn't answer like its API, never a 401) and code. Others are internal.
func signInError(c echo.Context, err error) error {
	if errors.Is(err, cloudlink.ErrSignInExpired) {
		return envelope.Fail(c, http.StatusBadRequest, "SIGNIN_EXPIRED", "That sign-in has expired. Sign in again.")
	}
	var failed *cloudlink.CallError
	if !errors.As(err, &failed) {
		return err
	}
	status := failed.Status
	if status == 0 || status == http.StatusUnauthorized {
		status = http.StatusBadGateway
	}
	return envelope.Fail(c, status, failed.Code, failed.Error())
}

// signInFirst answers a browser on another computer with no session: a page of an extension's
// opened in the browser goes to the sign-in page and back; anything else gets a 401 the web UI and
// Kode read as "sign in".
func signInFirst(c echo.Context) error {
	r := c.Request()
	if c.Path() == extRoute && navigation(r) {
		return c.Redirect(http.StatusSeeOther, "/signin?next="+url.QueryEscape(r.URL.RequestURI()))
	}
	return envelope.Fail(c, http.StatusUnauthorized, "SIGNIN_REQUIRED", "Sign in to Meko with Meko Cloud first.")
}

// navigation says whether r is the browser opening a page (a link, the address bar), not a script's
// request or a WebSocket.
func navigation(r *http.Request) bool {
	if r.Method != http.MethodGet {
		return false
	}
	if mode := r.Header.Get("Sec-Fetch-Mode"); mode != "" {
		return mode == "navigate"
	}
	return strings.Contains(r.Header.Get("Accept"), "text/html")
}

// randomToken is 32 random bytes, base64url: a session cookie's value, or a sign-in's nonce.
func randomToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

func tokenSHA256(cookie string) string {
	sum := sha256.Sum256([]byte(cookie))
	return hex.EncodeToString(sum[:])
}

// peerIP is the address the connection came from (no proxy's header is trusted).
func peerIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return ""
	}
	return host
}

// clip cuts s to at most n bytes.
func clip(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}
