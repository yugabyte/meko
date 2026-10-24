package server

import (
	"errors"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/yugabyte/meko/backend/internal/config"
	"github.com/yugabyte/meko/backend/internal/envelope"
	"github.com/yugabyte/meko/backend/internal/extproxy"
	"github.com/yugabyte/meko/backend/internal/login"
)

// Web makes a Server the web listener: the port browsers use, for Meko's web UI and its extensions'
// at /ext/<name>/. It takes no Bearer token: with local access a request from this computer needs
// nothing (LocalAccess), and otherwise it needs the owner's session cookie.
type Web struct {
	Access string // config.AccessLocal or config.AccessRemote
	// Extensions are the extensions served at /ext/<name>/, read at each request.
	Extensions func() []*extproxy.Proxy
}

func (s *Server) proxy(name string) *extproxy.Proxy {
	for _, p := range s.opts.Web.Extensions() {
		if p.Name == name {
			return p
		}
	}
	return nil
}

// SessionCookie is the browser's session on the web listener.
const SessionCookie = "meko_session"

// The web listener's sign-in routes. status, setup and login take no credential (logout clears
// whatever cookie it gets); the rest need the owner signed in, or local access.
const (
	authStatusRoute = "/api/v1/auth/status"
	authSetupRoute  = "/api/v1/auth/setup"
	authLoginRoute  = "/api/v1/auth/login"
	authLogoutRoute = "/api/v1/auth/logout"
	// setupLinkRoute is the API listener's, for meko setup_link, with the admin token.
	setupLinkRoute = "/api/v1/auth/setup-link"
	extRootRoute   = "/ext/:name"
	extRoute       = "/ext/:name/*"
)

const sessionHashKey = "meko.session" //nolint:gosec // G101: a context key, not a secret

func (s *Server) registerWeb(v1 *echo.Group) {
	v1.GET("/auth/status", s.authStatus)
	v1.POST("/auth/setup", s.authSetup)
	v1.POST("/auth/login", s.authLogin)
	v1.POST("/auth/logout", s.authLogout)
	v1.POST("/auth/password", s.authPassword)
	v1.POST("/auth/totp/enroll", s.totpEnroll)
	v1.POST("/auth/totp/confirm", s.totpConfirm)
	v1.POST("/auth/totp/disable", s.totpDisable)
	v1.GET("/extensions", s.extensionList)
	s.e.Any(extRootRoute, s.extensionRoot)
	s.e.Any(extRoute, s.extension)
}

// requireBrowser is the web listener's credential check, keyed on the matched route as
// requireToken is: local access, else the session cookie.
func (s *Server) requireBrowser(next echo.HandlerFunc) echo.HandlerFunc {
	return func(c echo.Context) error {
		switch c.Path() {
		case "/healthz", authStatusRoute, authSetupRoute, authLoginRoute, authLogoutRoute, uiRoute, extRootRoute:
			return next(c)
		case uploadRoute, downloadRoute:
			c.Set(credentialKey, CredentialTicket)
			return next(c)
		}
		if s.local(c) {
			c.Set(credentialKey, CredentialLocal)
			return next(c)
		}
		if hash, ok := s.session(c); ok {
			c.Set(credentialKey, CredentialSession)
			c.Set(sessionHashKey, hash)
			return next(c)
		}
		r := c.Request()
		if c.Path() == extRoute && r.Method == http.MethodGet && strings.Contains(r.Header.Get("Accept"), "text/html") {
			return c.Redirect(http.StatusFound, "/signin?next="+url.QueryEscape(r.URL.RequestURI()))
		}
		return envelope.Fail(c, http.StatusUnauthorized, "UNAUTHORIZED", "Sign in first: open Meko's web UI on this port.")
	}
}

func (s *Server) local(c echo.Context) bool {
	return s.opts.Web.Access == config.AccessLocal && LocalAccess(c.Request())
}

// session checks the cookie, renewing it as its expiry slides forward.
func (s *Server) session(c echo.Context) (string, bool) {
	ck, err := c.Cookie(SessionCookie)
	if err != nil || ck.Value == "" || s.opts.Login == nil {
		return "", false
	}
	ok, renewed, err := s.opts.Login.Session(c.Request().Context(), ck.Value)
	if err != nil || !ok {
		return "", false
	}
	if renewed {
		s.setSessionCookie(c, ck.Value)
	}
	return login.Hash(ck.Value), true
}

// setSessionCookie sends the session's cookie. It's Secure over HTTPS, which remote access always
// is; local access is plain HTTP on 127.0.0.1, where some browsers (Safari) drop a Secure cookie.
func (s *Server) setSessionCookie(c echo.Context, token string) {
	//nolint:gosec // G124: Secure follows TLS, as above
	c.SetCookie(&http.Cookie{Name: SessionCookie, Value: token, Path: "/", HttpOnly: true,
		Secure: c.Request().TLS != nil, SameSite: http.SameSiteStrictMode, MaxAge: int(login.SessionTTL / time.Second)})
}

// peerIP is the connection's address, which a header can't change: failed sign-ins are counted by it.
func peerIP(c echo.Context) string {
	host, _, err := net.SplitHostPort(c.Request().RemoteAddr)
	if err != nil {
		return c.Request().RemoteAddr
	}
	return host
}

// AuthStatus is GET /api/v1/auth/status: how this browser stands with the web listener.
type AuthStatus struct {
	Access        string  `json:"access"`       // local or remote
	LocalAccess   bool    `json:"local_access"` // this request needs no login
	SignedIn      bool    `json:"signed_in"`
	SetupRequired bool    `json:"setup_required"` // a login must be made (or remade) with a setup link
	Reset         bool    `json:"reset"`          // the setup link replaces an existing login
	Username      *string `json:"username"`
	TOTPEnabled   bool    `json:"totp_enabled"`
}

func (s *Server) authStatus(c echo.Context) error {
	ctx := c.Request().Context()
	o, err := s.opts.Login.Owner(ctx)
	if err != nil {
		return err
	}
	local := s.local(c)
	_, signed := s.session(c)
	signedIn := local || signed
	st := AuthStatus{Access: s.opts.Web.Access, LocalAccess: local, SignedIn: signedIn,
		SetupRequired: !local && (o == nil || (s.opts.Login.SetupOutstanding(ctx) && !signedIn)),
		TOTPEnabled:   o != nil && o.TOTPSecret != ""}
	st.Reset = st.SetupRequired && o != nil
	if o != nil && signedIn {
		st.Username = &o.Username
	}
	return envelope.OK(c, st)
}

type credentials struct {
	Token    string `json:"token"`
	Username string `json:"username"`
	Password string `json:"password"`
	TOTP     string `json:"totp"`
	Current  string `json:"current"`
	Next     string `json:"next"`
	Code     string `json:"code"`
}

func bindCredentials(c echo.Context) (credentials, error) {
	var b credentials
	if err := c.Bind(&b); err != nil {
		return b, envelope.Fail(c, http.StatusBadRequest, "VALIDATION_ERROR", "Send a JSON body.")
	}
	return b, nil
}

func (s *Server) authSetup(c echo.Context) error {
	b, err := bindCredentials(c)
	if err != nil {
		return err
	}
	token, err := s.opts.Login.Setup(c.Request().Context(), b.Token, b.Username, b.Password, peerIP(c),
		c.Request().UserAgent())
	if err != nil {
		return loginError(c, err)
	}
	s.setSessionCookie(c, token)
	return envelope.OK(c, map[string]bool{"signed_in": true})
}

func (s *Server) authLogin(c echo.Context) error {
	b, err := bindCredentials(c)
	if err != nil {
		return err
	}
	token, err := s.opts.Login.Login(c.Request().Context(), b.Username, b.Password, b.TOTP, peerIP(c),
		c.Request().UserAgent())
	if err != nil {
		return loginError(c, err)
	}
	s.setSessionCookie(c, token)
	return envelope.OK(c, map[string]bool{"signed_in": true})
}

func (s *Server) authLogout(c echo.Context) error {
	if ck, err := c.Cookie(SessionCookie); err == nil {
		if err := s.opts.Login.Logout(c.Request().Context(), ck.Value); err != nil {
			return err
		}
	}
	//nolint:gosec // G124: Secure follows TLS, as in setSessionCookie
	c.SetCookie(&http.Cookie{Name: SessionCookie, Value: "", Path: "/", HttpOnly: true,
		Secure: c.Request().TLS != nil, SameSite: http.SameSiteStrictMode, MaxAge: -1})
	return envelope.OK(c, map[string]bool{"signed_in": false})
}

func (s *Server) authPassword(c echo.Context) error {
	b, err := bindCredentials(c)
	if err != nil {
		return err
	}
	keep := ""
	if ck, err := c.Cookie(SessionCookie); err == nil {
		keep = ck.Value
	}
	if err := s.opts.Login.ChangePassword(c.Request().Context(), b.Current, b.Next, keep, peerIP(c)); err != nil {
		return loginError(c, err)
	}
	return envelope.OK(c, map[string]bool{"changed": true})
}

func (s *Server) totpEnroll(c echo.Context) error {
	secret, link, err := s.opts.Login.EnrollTOTP(c.Request().Context())
	if err != nil {
		return loginError(c, err)
	}
	return envelope.OK(c, map[string]string{"secret": secret, "otpauth_url": link})
}

func (s *Server) totpConfirm(c echo.Context) error {
	b, err := bindCredentials(c)
	if err != nil {
		return err
	}
	if err := s.opts.Login.ConfirmTOTP(c.Request().Context(), b.Code); err != nil {
		return loginError(c, err)
	}
	return envelope.OK(c, map[string]bool{"totp_enabled": true})
}

func (s *Server) totpDisable(c echo.Context) error {
	b, err := bindCredentials(c)
	if err != nil {
		return err
	}
	if err := s.opts.Login.DisableTOTP(c.Request().Context(), b.Code); err != nil {
		return loginError(c, err)
	}
	return envelope.OK(c, map[string]bool{"totp_enabled": false})
}

// loginError turns the sign-in's errors into replies; others are internal.
func loginError(c echo.Context, err error) error {
	var check login.CheckError
	switch {
	case errors.As(err, &check):
		return envelope.Fail(c, http.StatusBadRequest, "VALIDATION_ERROR", check.Error())
	case errors.Is(err, login.ErrBadLogin):
		return envelope.Fail(c, http.StatusUnauthorized, "BAD_LOGIN", "Wrong username or password.")
	case errors.Is(err, login.ErrTOTPRequired):
		return envelope.Fail(c, http.StatusUnauthorized, "TOTP_REQUIRED", "Enter the code from your authenticator app.")
	case errors.Is(err, login.ErrBadTOTP) && c.Path() == authLoginRoute:
		return envelope.Fail(c, http.StatusUnauthorized, "TOTP_REQUIRED", "That code is wrong or expired.")
	case errors.Is(err, login.ErrBadTOTP):
		return envelope.Fail(c, http.StatusBadRequest, "VALIDATION_ERROR",
			"That code doesn't match. Check the time on your phone and try again.")
	case errors.Is(err, login.ErrBusy):
		return envelope.Fail(c, http.StatusTooManyRequests, "RATE_LIMITED",
			"Too many sign-ins at once. Try again in a moment.")
	case errors.Is(err, login.ErrRateLimited):
		return envelope.Fail(c, http.StatusTooManyRequests, "RATE_LIMITED",
			"Too many failed attempts. Try again in 15 minutes.")
	case errors.Is(err, login.ErrBadSetupLink):
		return envelope.Fail(c, http.StatusForbidden, "FORBIDDEN",
			"This setup link is invalid or already used. Run meko setup_link on the host for a new one.")
	case errors.Is(err, login.ErrBadPassword):
		return envelope.Fail(c, http.StatusBadRequest, "VALIDATION_ERROR", "The current password is wrong.")
	case errors.Is(err, login.ErrNoOwner):
		return envelope.Fail(c, http.StatusConflict, "CONFLICT",
			"No login is set up. Run meko setup_link on the host to make one.")
	}
	return err
}

// ExtensionInfo is a row of GET /api/v1/extensions.
type ExtensionInfo struct {
	Name      string `json:"name"`
	Title     string `json:"title"`
	Path      string `json:"path"`
	Available bool   `json:"available"`
}

func (s *Server) extensionList(c echo.Context) error {
	exts := s.opts.Web.Extensions()
	out := make([]ExtensionInfo, 0, len(exts))
	for _, p := range exts {
		out = append(out, ExtensionInfo{Name: p.Name, Title: p.Title, Path: p.Path(), Available: p.Available()})
	}
	return envelope.OK(c, out)
}

func (s *Server) extensionRoot(c echo.Context) error {
	if s.proxy(c.Param("name")) == nil {
		return echo.ErrNotFound
	}
	extproxy.RedirectToSlash(c.Response(), c.Request())
	return nil
}

func (s *Server) extension(c echo.Context) error {
	p := s.proxy(c.Param("name"))
	if p == nil {
		return echo.ErrNotFound
	}
	r := c.Request()
	if hash, _ := c.Get(sessionHashKey).(string); hash != "" {
		r = extproxy.WithSession(r, hash)
	}
	p.ServeHTTP(c.Response(), r)
	return nil
}

// issueSetupLink is POST /api/v1/auth/setup-link on the API listener, for meko setup_link.
func (s *Server) issueSetupLink(c echo.Context) error {
	token, err := s.opts.Login.IssueSetupLink(c.Request().Context())
	if err != nil {
		return err
	}
	link := ""
	if s.opts.WebURL != nil {
		link = s.opts.WebURL() + "/setup?token=" + token
	}
	return envelope.OK(c, map[string]any{"token": token, "url": link,
		"expires_in": int(login.SetupLinkTTL / time.Second)})
}
