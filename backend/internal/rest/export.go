package rest

import (
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"

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
	id, ok, err := a.pathDatapack(c)
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
