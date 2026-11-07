package server

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"sort"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/yugabyte/meko/src/meko/internal/config"
	"github.com/yugabyte/meko/src/meko/internal/envelope"
	"github.com/yugabyte/meko/src/meko/internal/extensions"
	"github.com/yugabyte/meko/src/meko/internal/extproxy"
	"github.com/yugabyte/meko/src/meko/internal/update"
)

// Web lets browsers into a Server, for Meko's web UI and its extensions' at /ext/<name>/, with no
// login: over plain HTTP, requests from this computer (LocalAccess); over TLS (remote access), any
// browser's that another site's page didn't send (SameSite).
type Web struct {
	Access string // config.AccessLocal or config.AccessRemote
	// Off keeps browsers out ([web] enabled = false, meko ui stop): the web UI and the extensions'
	// answer 404, and only tokens get in.
	Off bool
	// Extensions are the extensions served at /ext/<name>/, read at each request.
	Extensions func() []*extproxy.Proxy
	// Installed names the registered extensions (extensions.json), read at each request; nil means
	// the ones in Extensions.
	Installed func() []string
	// Run, if set, runs an action on a known extension for the Extensions page (install, update,
	// start or stop), writing its output to out, and returns when it's done or ctx is (Jobs.Cancel).
	Run func(ctx context.Context, name, action string, out io.Writer) error
	// ExtensionUpdates, if set, is what an installed extension's row shows about its updates.
	ExtensionUpdates func(name string) ExtensionUpdate
	// CheckExtensions, if set, checks for releases now (POST /api/v1/extensions/:name/check);
	// update.ErrBusy while an update holds the lock.
	CheckExtensions func(ctx context.Context) error
	// SetAutoUpdate, if set, saves an extension's Update automatically (PATCH
	// /api/v1/extensions/:name).
	SetAutoUpdate func(name string, on bool) error
	// Jobs are the actions Run runs, kept across the servers the listener's owner builds; nil gives
	// this server its own.
	Jobs *ExtensionJobs
	// Updates are Meko's own updates, for the Status page; nil for a build from source.
	Updates Updates
}

// The actions the Extensions page runs on an extension (POST /api/v1/extensions/:name/:action), as
// jobs; check (Check now) runs in the request.
var extensionActions = []string{"install", "update", "start", "stop"}

func (s *Server) proxy(name string) *extproxy.Proxy {
	for _, p := range s.opts.Web.Extensions() {
		if p.Name == name {
			return p
		}
	}
	return nil
}

// The browsers' routes the credential check knows by name.
const (
	authStatusRoute = "/api/v1/auth/status"
	extRootRoute    = "/ext/:name"
	extRoute        = "/ext/:name/*"
)

func (s *Server) registerWeb(v1 *echo.Group) {
	v1.GET("/auth/status", s.authStatus)
	v1.GET("/extensions", s.extensionList, ownerOnly)
	v1.POST("/extensions/:name/:action", s.extensionAction, ownerOnly)
	v1.PATCH("/extensions/:name", s.extensionPatch, ownerOnly)
	v1.GET("/update", s.updateStatus, ownerOnly)
	v1.POST("/update/check", s.updateCheck, ownerOnly)
	v1.POST("/update/apply", s.updateApply, ownerOnly)
	s.registerCloudSignIn(v1)
	s.e.Any(extRootRoute, s.extensionRoot)
	s.e.Any(extRoute, s.extension)
}

// browser is the credential a browser's request gets, or "" for none: over plain HTTP (only ever
// from this computer), a request from this computer (LocalAccess); over TLS, which only remote
// access serves, any request another site's page didn't send (SameSite). With the web UI off, none.
func (s *Server) browser(r *http.Request) string {
	switch {
	case s.opts.Web == nil || s.opts.Web.Off:
		return ""
	case r.TLS != nil && s.opts.Web.Access == config.AccessRemote && SameSite(r):
		return CredentialRemote
	case r.TLS == nil && LocalAccess(r):
		return CredentialLocal
	}
	return ""
}

// browserSent reports whether a request without a token looks like a browser's (Fetch Metadata) or
// a proxy's, which get the browsers' refusal rather than the one asking for a token.
func browserSent(r *http.Request) bool {
	if len(r.Header.Values("Sec-Fetch-Site")) > 0 {
		return true
	}
	for _, h := range forwarded {
		if len(r.Header.Values(h)) > 0 {
			return true
		}
	}
	return false
}

// refuseBrowser answers a browser's request that browser didn't let in.
func (s *Server) refuseBrowser(c echo.Context) error {
	switch {
	case s.opts.Web == nil || s.opts.Web.Off:
		return webOff(c)
	case c.Request().TLS != nil:
		return envelope.Fail(c, http.StatusForbidden, "FORBIDDEN", "Only Meko's own pages may call it.")
	}
	return envelope.Fail(c, http.StatusUnauthorized, "UNAUTHORIZED",
		"Local access takes only requests made on this computer, straight to Meko.")
}

// webOff is the answer to a browser's page while the web UI is off.
func webOff(c echo.Context) error {
	return envelope.Fail(c, http.StatusNotFound, "WEB_UI_OFF", "The web UI is off. Turn it on with meko ui start.")
}

// AuthStatus is GET /api/v1/auth/status: how this browser stands with Meko. There's no
// login: signed_in is whether the request is let in, and setup_required, reset, username and
// totp_enabled are always empty. It stays for pages that read it, AgentK's among them, which go to
// a sign-in screen only when it says remote and not signed in.
type AuthStatus struct {
	Access        string  `json:"access"`       // local or remote
	LocalAccess   bool    `json:"local_access"` // this request came as a local one
	SignedIn      bool    `json:"signed_in"`
	SetupRequired bool    `json:"setup_required"`
	Reset         bool    `json:"reset"`
	Username      *string `json:"username"`
	TOTPEnabled   bool    `json:"totp_enabled"`
}

func (s *Server) authStatus(c echo.Context) error {
	cred := s.browser(c.Request())
	return envelope.OK(c, AuthStatus{Access: s.opts.Web.Access, LocalAccess: cred == CredentialLocal,
		SignedIn: cred != ""})
}

// ExtensionInfo is a row of GET /api/v1/extensions: a known extension, whether it's installed
// (registered in extensions.json), where its web UI is and whether that answers, and its last job.
type ExtensionInfo struct {
	Name        string        `json:"name"`
	Title       string        `json:"title"`
	Description string        `json:"description"`
	Installed   bool          `json:"installed"`
	WebUI       bool          `json:"web_ui"`
	Path        string        `json:"path"`      // "" while it isn't served
	Available   bool          `json:"available"` // its socket is there to connect to
	Job         *ExtensionJob `json:"job,omitempty"`
	ExtensionUpdate
}

// ExtensionUpdate is what an extension's row says about its updates.
type ExtensionUpdate struct {
	Version   string `json:"version,omitempty"` // the release Meko installed, when it manages it
	Updatable bool   `json:"updatable"`         // Meko can update it (the update action)
	// NotUpdatable says why an installed extension isn't updatable: a build from source, or an
	// AgentK that hasn't moved into Meko's data folder yet. An older Meko never sends it.
	NotUpdatable string `json:"not_updatable,omitempty"`
	// Latest is the newest release the last check found (versions rolled back left out), and
	// UpdateAvailable whether it's newer than Version and wasn't rolled back.
	Latest          string     `json:"latest,omitempty"`
	LatestURL       string     `json:"latest_url,omitempty"`
	UpdateAvailable bool       `json:"update_available"`
	CheckedAt       *time.Time `json:"checked_at,omitempty"`
	CheckError      string     `json:"check_error,omitempty"`
	// Pending is an update on probation: meko _launch counts its starts.
	Pending    *update.Pending `json:"pending,omitempty"`
	RolledBack []string        `json:"rolled_back"`
	AutoUpdate bool            `json:"auto_update"` // Update automatically
	Waiting    string          `json:"waiting,omitempty"`
}

func (s *Server) installed() map[string]bool {
	out := map[string]bool{}
	if s.opts.Web.Installed == nil {
		for _, p := range s.opts.Web.Extensions() {
			out[p.Name] = true
		}
		return out
	}
	for _, n := range s.opts.Web.Installed() {
		out[n] = true
	}
	return out
}

func (s *Server) extensionInfo(name string, installed bool) ExtensionInfo {
	cat := extensions.Known[name]
	row := ExtensionInfo{Name: name, Title: cat.Title, Description: cat.Description, Installed: installed,
		WebUI: cat.WebUI, Job: s.opts.Web.Jobs.get(name)}
	if p := s.proxy(name); p != nil {
		row.Path, row.Available = p.Path(), p.Available()
	}
	if overtaken(row) {
		row.Job = nil
	}
	row.RolledBack = []string{}
	if installed && s.opts.Web.ExtensionUpdates != nil {
		row.ExtensionUpdate = s.opts.Web.ExtensionUpdates(name)
	}
	return row
}

// overtaken reports whether row's failed job no longer says how the extension is: something
// outside the Extensions page (meko extension install, me_koder start) did what the job couldn't.
// The job is in memory until the next one, so without this a failed Install would stay on the card
// of an extension installed from a terminal until Meko restarts. Only a running extension
// overtakes one: an install can fail after registering it (it never answered on its socket, or a
// registered one's own install failed again), and that failure must still show.
func overtaken(row ExtensionInfo) bool {
	j := row.Job
	if j == nil || j.State != JobFailed || !row.Installed || !row.Available {
		return false
	}
	return j.Action == "install" || j.Action == "start"
}

func (s *Server) extensionList(c echo.Context) error {
	installed := s.installed()
	names := make([]string, 0, len(extensions.Known))
	for n := range extensions.Known {
		names = append(names, n)
	}
	sort.Strings(names)
	out := make([]ExtensionInfo, 0, len(names))
	for _, n := range names {
		out = append(out, s.extensionInfo(n, installed[n]))
	}
	return envelope.OK(c, out)
}

// extensionAction is POST /api/v1/extensions/:name/:action: it starts the action as the extension's
// job and answers 202 with its row; the page follows the job in the list.
func (s *Server) extensionAction(c echo.Context) error {
	name, action := c.Param("name"), c.Param("action")
	cat, known := extensions.Known[name]
	if !known || s.opts.Web.Run == nil || (!slices.Contains(extensionActions, action) && action != "check") {
		return echo.ErrNotFound
	}
	installed := s.installed()[name]
	if action != "install" && !installed {
		return envelope.Fail(c, http.StatusConflict, "CONFLICT", cat.Title+" isn't installed.")
	}
	if action == "check" {
		return s.extensionCheck(c, name)
	}
	jobs := s.opts.Web.Jobs
	j, ok := jobs.start(name, action, false)
	if !ok {
		return envelope.Fail(c, http.StatusConflict, "CONFLICT",
			fmt.Sprintf("%s is busy: its %s hasn't finished.", cat.Title, j.snapshot().Action))
	}
	s.opts.Logger.Info("extension action", "extension", name, "action", action)
	go func() {
		// Not the request's context: the job outlives the request that started it, and this server.
		err := s.opts.Web.Run(jobs.run(), name, action, j)
		jobs.done(j, err)
		if err != nil {
			s.opts.Logger.Warn("extension action failed", "extension", name, "action", action, "error", err)
		} else {
			s.opts.Logger.Info("extension action done", "extension", name, "action", action)
		}
	}()
	return c.JSON(http.StatusAccepted, envelope.Response{Data: s.extensionInfo(name, installed)})
}

// extensionCheck is POST /api/v1/extensions/:name/check: Check now, answered with the row once the
// check is done (within 60 s), or 409 UPDATE_BUSY with the row while an update holds the lock.
func (s *Server) extensionCheck(c echo.Context, name string) error {
	if s.opts.Web.CheckExtensions == nil {
		return envelope.Fail(c, http.StatusConflict, "CONFLICT", "This Meko doesn't check for extensions' updates.")
	}
	ctx, cancel := context.WithTimeout(c.Request().Context(), 60*time.Second)
	defer cancel()
	if err := s.opts.Web.CheckExtensions(ctx); err != nil {
		if errors.Is(err, update.ErrBusy) {
			return c.JSON(http.StatusConflict, envelope.Response{Data: s.extensionInfo(name, true),
				Error: &envelope.Error{Code: "UPDATE_BUSY", Message: "An update is running: check again when it's done."}})
		}
		return envelope.Fail(c, http.StatusConflict, "CONFLICT", err.Error())
	}
	return envelope.OK(c, s.extensionInfo(name, true))
}

// extensionPatch is PATCH /api/v1/extensions/:name with {"auto_update": bool}: the Extensions
// page's Update automatically.
func (s *Server) extensionPatch(c echo.Context) error {
	name := c.Param("name")
	if _, known := extensions.Known[name]; !known || s.opts.Web.SetAutoUpdate == nil {
		return echo.ErrNotFound
	}
	if !s.installed()[name] {
		return envelope.Fail(c, http.StatusConflict, "CONFLICT", extensions.Known[name].Title+" isn't installed.")
	}
	var body struct {
		AutoUpdate *bool `json:"auto_update"`
	}
	if err := c.Bind(&body); err != nil || body.AutoUpdate == nil {
		return envelope.Fail(c, http.StatusBadRequest, "BAD_REQUEST", `Send {"auto_update": true|false}.`)
	}
	if err := s.opts.Web.SetAutoUpdate(name, *body.AutoUpdate); err != nil {
		return err
	}
	s.opts.Logger.Info("extension setting", "extension", name, "auto_update", *body.AutoUpdate)
	return envelope.OK(c, s.extensionInfo(name, true))
}

func (s *Server) extensionRoot(c echo.Context) error {
	if s.opts.Web.Off {
		return webOff(c)
	}
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
	p.ServeHTTP(c.Response(), c.Request())
	return nil
}
