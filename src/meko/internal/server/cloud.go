package server

import (
	"errors"
	"net/http"

	"github.com/labstack/echo/v4"

	"github.com/yugabyte/meko/src/meko/internal/cloudlink"
	"github.com/yugabyte/meko/src/meko/internal/envelope"
)

// cloudRoute is the Meko Cloud link's (docs/cloud.md): GET reads it, PUT links (cloudlink.Update),
// DELETE unlinks. The web listener takes its browsers (local or remote access); the API listener
// only the admin token (adminOnly), for meko cloud_link. Replies never carry the key.
const cloudRoute = "/api/v1/settings/meko-cloud"

func (s *Server) registerCloud(v1 *echo.Group) {
	if s.opts.Cloud == nil {
		return
	}
	v1.GET("/settings/meko-cloud", s.cloudStatus)
	v1.PUT("/settings/meko-cloud", s.cloudLink)
	v1.DELETE("/settings/meko-cloud", s.cloudUnlink)
}

// registerCloudSignIn is the web UI's sign-in to Meko Cloud (docs/cloud.md, "Signing in"), on the web
// listener only: POST …/sign-in starts it and answers where the browser goes to sign in; Meko Cloud
// sends the browser back to the web UI's /settings/meko-cloud/callback, a page rather than a route
// (the web listener refuses an API call another site sends), whose POST …/sign-in/finish links.
func (s *Server) registerCloudSignIn(v1 *echo.Group) {
	if s.opts.Cloud == nil {
		return
	}
	v1.POST("/settings/meko-cloud/sign-in", s.cloudSignIn)
	v1.POST("/settings/meko-cloud/sign-in/finish", s.cloudSignInFinish)
}

// signInCallbackPath is the web UI's page Meko Cloud sends the browser back to.
const signInCallbackPath = "/settings/meko-cloud/callback"

func (s *Server) cloudSignIn(c echo.Context) error {
	var u cloudlink.Update
	if err := c.Bind(&u); err != nil {
		return envelope.Fail(c, http.StatusBadRequest, "VALIDATION_ERROR", "Send a JSON body.")
	}
	// The browser comes back to the address it's on: the request's own, which the web listener's
	// checks have let in (this computer's name with local access, the Origin's with remote).
	redirect := c.Scheme() + "://" + c.Request().Host + signInCallbackPath
	to, err := s.opts.Cloud.StartSignIn(c.Request().Context(), u, redirect)
	if err != nil {
		return cloudError(c, err)
	}
	s.opts.Logger.Info("Meko Cloud sign-in started")
	return envelope.OK(c, map[string]string{"authorize_url": to})
}

func (s *Server) cloudSignInFinish(c echo.Context) error {
	var body struct {
		Code  string `json:"code"`
		State string `json:"state"`
	}
	if err := c.Bind(&body); err != nil {
		return envelope.Fail(c, http.StatusBadRequest, "VALIDATION_ERROR", "Send a JSON body.")
	}
	st, err := s.opts.Cloud.FinishSignIn(c.Request().Context(), body.Code, body.State)
	if err != nil {
		return cloudError(c, err)
	}
	s.opts.Logger.Info("Meko Cloud linked by signing in", "url", st.EffectiveURL)
	return envelope.OK(c, st)
}

func (s *Server) cloudStatus(c echo.Context) error {
	st, err := s.opts.Cloud.Status()
	if err != nil {
		return err
	}
	return envelope.OK(c, st)
}

func (s *Server) cloudLink(c echo.Context) error {
	var u cloudlink.Update
	if err := c.Bind(&u); err != nil {
		return envelope.Fail(c, http.StatusBadRequest, "VALIDATION_ERROR", "Send a JSON body.")
	}
	st, err := s.opts.Cloud.Link(c.Request().Context(), u)
	if err != nil {
		return cloudError(c, err)
	}
	s.opts.Logger.Info("Meko Cloud linked", "url", st.EffectiveURL)
	return envelope.OK(c, st)
}

func (s *Server) cloudUnlink(c echo.Context) error {
	st, err := s.opts.Cloud.Unlink(c.Request().Context())
	if err != nil {
		return err
	}
	s.opts.Logger.Info("Meko Cloud unlinked")
	return envelope.OK(c, st)
}

// cloudError turns a refused change into a 400 with its reason; others are internal.
func cloudError(c echo.Context, err error) error {
	var refused *cloudlink.Error
	if errors.As(err, &refused) {
		return envelope.Fail(c, http.StatusBadRequest, "VALIDATION_ERROR", refused.Error())
	}
	return err
}
