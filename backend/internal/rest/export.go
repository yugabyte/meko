package rest

import (
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/yugabyte/meko/backend/internal/version"
)

// unsafeName is what a datapack name loses in the export's filename.
var unsafeName = regexp.MustCompile(`[^a-z0-9_-]+`)

// exportDatapack sends a datapack as one JSON file to download (Meko's own; Cloud has none yet):
// the format memory.ExportDatapack writes, streamed. Once the body has started an error can only
// cut it short, which leaves invalid JSON, so a reader can tell; the cause is logged.
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
	h := c.Response().Header()
	h.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	h.Set(echo.HeaderContentDisposition,
		fmt.Sprintf(`attachment; filename="meko-%s-%s.json"`, name, time.Now().UTC().Format("2006-01-02")))
	c.Response().WriteHeader(http.StatusOK)
	if err := a.Service.ExportDatapack(ctx(c), id, version.Version, c.Response()); err != nil {
		a.Log.Error("export failed partway", "datapack_id", id, "error", err)
	}
	return nil
}
