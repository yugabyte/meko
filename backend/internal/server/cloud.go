package server

import (
	"errors"
	"net/http"

	"github.com/labstack/echo/v4"

	"github.com/yugabyte/meko/backend/internal/cloudlink"
	"github.com/yugabyte/meko/backend/internal/envelope"
)

// cloudRoute is the Meko Cloud link's (docs/cloud.md): GET reads it, PUT links (cloudlink.Update),
// DELETE unlinks. The web listener takes the owner (local access or the session); the API listener
// only the admin token (adminOnly), for meko cloud_link. Replies never carry the key.
const cloudRoute = "/api/v1/settings/meko-cloud"

// cloudDatapacksRoute lists the linked Meko Cloud's datapacks, for the web UI's sidebar and meko
// datapacks --cloud. It takes the credential every data route does, the client token included: the
// reply carries datapacks' names and grants, never the key.
const cloudDatapacksRoute = "/api/v1/meko-cloud/datapacks"

func (s *Server) registerCloud(v1 *echo.Group) {
	if s.opts.Cloud == nil {
		return
	}
	v1.GET("/settings/meko-cloud", s.cloudStatus)
	v1.PUT("/settings/meko-cloud", s.cloudLink)
	v1.DELETE("/settings/meko-cloud", s.cloudUnlink)
	v1.GET("/meko-cloud/datapacks", s.cloudDatapacks)
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

// cloudDatapacks answers a listing Meko Cloud didn't make as a 502 with its code, never a 401,
// which the web UI would read as its own sign-in lost.
func (s *Server) cloudDatapacks(c echo.Context) error {
	list, err := s.opts.Cloud.Datapacks(c.Request().Context())
	var failed *cloudlink.ListError
	if errors.As(err, &failed) {
		return envelope.Fail(c, http.StatusBadGateway, failed.Code, failed.Error())
	}
	if err != nil {
		return err
	}
	return envelope.OK(c, list)
}

// cloudError turns a refused change into a 400 with its reason; others are internal.
func cloudError(c echo.Context, err error) error {
	var refused *cloudlink.Error
	if errors.As(err, &refused) {
		return envelope.Fail(c, http.StatusBadRequest, "VALIDATION_ERROR", refused.Error())
	}
	return err
}
