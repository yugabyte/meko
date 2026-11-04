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

// Web makes a Server the web listener: the port browsers use, for Meko's web UI and its extensions'
// at /ext/<name>/. It takes no Bearer token and has no login: with local access it takes requests
// from this computer (LocalAccess), and with remote access any browser's that another site's page
// didn't send (SameSite).
type Web struct {
	Access string // config.AccessLocal or config.AccessRemote
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

// The web listener's routes its credential check knows by name.
const (
	authStatusRoute = "/api/v1/auth/status"
	extRootRoute    = "/ext/:name"
	extRoute        = "/ext/:name/*"
)

func (s *Server) registerWeb(v1 *echo.Group) {
	v1.GET("/auth/status", s.authStatus)
	v1.GET("/extensions", s.extensionList)
	v1.POST("/extensions/:name/:action", s.extensionAction)
	v1.PATCH("/extensions/:name", s.extensionPatch)
	v1.GET("/update", s.updateStatus)
	v1.POST("/update/check", s.updateCheck)
	v1.POST("/update/apply", s.updateApply)
	s.registerCloud(v1)
	s.e.Any(extRootRoute, s.extensionRoot)
	s.e.Any(extRoute, s.extension)
}

// requireBrowser is the web listener's credential check, keyed on the matched route as
// requireToken is: with local access, a request from this computer (LocalAccess); with remote
// access, any request another site's page didn't send (SameSite).
func (s *Server) requireBrowser(next echo.HandlerFunc) echo.HandlerFunc {
	return func(c echo.Context) error {
		switch c.Path() {
		case "/healthz", authStatusRoute, uiRoute, extRootRoute:
			return next(c)
		case uploadRoute, downloadRoute:
			c.Set(credentialKey, CredentialTicket)
			return next(c)
		}
		r := c.Request()
		if s.opts.Web.Access == config.AccessRemote {
			if !SameSite(r) {
				return envelope.Fail(c, http.StatusForbidden, "FORBIDDEN", "Only Meko's own pages may call it.")
			}
			c.Set(credentialKey, CredentialRemote)
			return next(c)
		}
		if s.local(c) {
			c.Set(credentialKey, CredentialLocal)
			return next(c)
		}
		return envelope.Fail(c, http.StatusUnauthorized, "UNAUTHORIZED",
			"Local access takes only requests made on this computer, straight to Meko.")
	}
}

func (s *Server) local(c echo.Context) bool {
	return s.opts.Web.Access == config.AccessLocal && LocalAccess(c.Request())
}

// AuthStatus is GET /api/v1/auth/status: how this browser stands with the web listener. There's no
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
	local := s.local(c)
	signedIn := local || (s.opts.Web.Access == config.AccessRemote && SameSite(c.Request()))
	return envelope.OK(c, AuthStatus{Access: s.opts.Web.Access, LocalAccess: local, SignedIn: signedIn})
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
	row.RolledBack = []string{}
	if installed && s.opts.Web.ExtensionUpdates != nil {
		row.ExtensionUpdate = s.opts.Web.ExtensionUpdates(name)
	}
	return row
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
