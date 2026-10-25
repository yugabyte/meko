package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/yugabyte/meko/backend/internal/envelope"
	"github.com/yugabyte/meko/backend/internal/extensions"
	"github.com/yugabyte/meko/backend/internal/update"
)

// Updates is the Status page's way to Meko's updates: serve's update.Updater. Only the web
// listener has the routes (GET /api/v1/update, POST /update/check and /update/apply), behind its
// sign-in; the CLI has meko update.
type Updates interface {
	Status() update.Status
	CheckNow(ctx context.Context) (update.Status, error)
	StartApply() (update.Status, error)
}

// updateCheckTimeout bounds the Check now button's listing of the releases.
const updateCheckTimeout = time.Minute

// errFromSource is the routes' answer on a build from source.
const errFromSource = "This Meko was built from source, so it doesn't update itself: rebuild it, or install a release."

func (s *Server) updateStatus(c echo.Context) error {
	if s.opts.Web.Updates == nil {
		return envelope.OK(c, update.Status{Running: s.opts.Info.Version, RolledBack: []string{}})
	}
	return envelope.OK(c, s.opts.Web.Updates.Status())
}

func (s *Server) updateCheck(c echo.Context) error {
	u := s.opts.Web.Updates
	if u == nil {
		return envelope.Fail(c, http.StatusConflict, "CONFLICT", errFromSource)
	}
	ctx, cancel := context.WithTimeout(c.Request().Context(), updateCheckTimeout)
	defer cancel()
	st, err := u.CheckNow(ctx)
	if err != nil {
		return updateConflict(c, err)
	}
	return envelope.OK(c, st)
}

// updateApply starts installing the newest release and answers 202; GET /update follows the job,
// and the server restarts into the new version once it's current. It refuses while an
// extension's action from the Extensions page runs, which the restart would otherwise wait for.
func (s *Server) updateApply(c echo.Context) error {
	u := s.opts.Web.Updates
	if u == nil {
		return envelope.Fail(c, http.StatusConflict, "CONFLICT", errFromSource)
	}
	if name, action, ok := s.opts.Web.Jobs.Running(); ok {
		title := name
		if cat, known := extensions.Known[name]; known {
			title = cat.Title
		}
		return envelope.Fail(c, http.StatusConflict, "EXTENSION_BUSY",
			fmt.Sprintf("%s's %s hasn't finished. Update Meko once it has.", title, action))
	}
	st, err := u.StartApply()
	if err != nil {
		return updateConflict(c, err)
	}
	s.opts.Logger.Info("update started from the web UI", "running", st.Running)
	return c.JSON(http.StatusAccepted, envelope.Response{Data: st})
}

func updateConflict(c echo.Context, err error) error {
	switch {
	case errors.Is(err, update.ErrBusy):
		return envelope.Fail(c, http.StatusConflict, "UPDATE_BUSY", "Meko is already checking for or installing an update.")
	case errors.Is(err, update.ErrPending):
		return envelope.Fail(c, http.StatusConflict, "UPDATE_PENDING",
			"The last update hasn't finished: a new version counts as installed once it has run 5 minutes.")
	}
	return err
}
