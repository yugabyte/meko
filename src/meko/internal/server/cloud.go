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
