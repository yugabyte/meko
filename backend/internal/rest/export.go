package rest

import (
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/labstack/echo/v4"

	"github.com/yugabyte/meko/backend/internal/version"
)

// unsafeName is what a datapack name loses in the export's filename.
var unsafeName = regexp.MustCompile(`[^a-z0-9_-]+`)

// exportDatapack sends a datapack as one zip to download (Meko's own; Cloud has none yet): the
// format memory.ExportDatapack writes, streamed. A failure before the first byte gets an error
// reply. One after it can't change the 200 already sent, so the connection is cut instead: the
// download then fails (an unexpected EOF) rather than ending cleanly with a short file.
func (a *api) exportDatapack(c echo.Context) error {
	id, ok, err := a.exportTarget(c)
	if !ok {
		return err
	}
	d, err := a.Service.Datapack(ctx(c), id)
	if err != nil {
		return serviceError(c, err)
	}
	name := strings.Trim(unsafeName.ReplaceAllString(strings.ToLower(d.Name), "-"), "-")
	if name == "" {
		name = "datapack"
	}
	started := false
	err = a.Service.ExportDatapack(ctx(c), id, version.Version, func() io.Writer {
		started = true
		h := c.Response().Header()
		h.Set(echo.HeaderContentType, "application/zip")
		h.Set(echo.HeaderContentDisposition,
			fmt.Sprintf(`attachment; filename="meko-%s-%s.zip"`, name, time.Now().UTC().Format("2006-01-02")))
		c.Response().WriteHeader(http.StatusOK)
		return c.Response()
	})
	if err == nil {
		return nil
	}
	if !started {
		return serviceError(c, err)
	}
	a.Log.Error("export failed partway", "datapack_id", id, "error", err)
	panic(http.ErrAbortHandler) // net/http closes the connection without ending the body
}

// exportTarget is the datapack the export route names: by its id, or by the name of one of this
// Meko's datapacks (unique here). meko-cli exports by name this way, so an export never asks Meko
// Cloud, which only the listing would: it works while Meko Cloud is down or refuses the key. A
// name or id this Meko doesn't have (a Meko Cloud datapack's) is a 404.
func (a *api) exportTarget(c echo.Context) (string, bool, error) {
	named := c.Param("id")
	// Echo leaves a parameter escaped when the path had a character it can't unescape safely (%2F).
	if c.Request().URL.RawPath != "" {
		if u, err := url.PathUnescape(named); err == nil {
			named = u
		}
	}
	if _, err := uuid.Parse(strings.TrimSpace(named)); err == nil {
		return a.datapack(c, named)
	}
	ds, err := a.Service.Datapacks(ctx(c))
	if err != nil {
		return "", false, err
	}
	for _, d := range ds {
		if d.Name == named {
			return d.ID, true, nil
		}
	}
	return "", false, notFound(c, fmt.Sprintf("no datapack on this computer is named %q", named))
}
