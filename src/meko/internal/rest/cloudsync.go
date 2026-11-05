package rest

import (
	"errors"
	"net/http"
	"strings"

	"github.com/google/uuid"
	"github.com/labstack/echo/v4"

	"github.com/yugabyte/meko/src/meko/internal/cloudlink"
	"github.com/yugabyte/meko/src/meko/internal/cloudsync"
	"github.com/yugabyte/meko/src/meko/internal/envelope"
)

// Syncing one of this Meko's datapacks to the linked Meko Cloud (internal/cloudsync), the owner's
// to do: it moves the datapack's contents to the account the link's key opens, and deletes it here.
// Meko's own routes, never sent on to Meko Cloud.

func (a *api) registerCloudSync(g *echo.Group) {
	g.GET("/settings/cloud-sync", a.cloudSyncSetting)
	g.PUT("/settings/cloud-sync", a.setCloudSyncSetting)
	if a.Sync == nil {
		return
	}
	g.POST("/datapacks/:id/cloud-sync/precheck", a.syncPrecheck)
	g.POST("/datapacks/:id/cloud-sync", a.startSync)
	g.GET("/datapacks/:id/cloud-sync", a.syncStatus)
}

func (a *api) syncOwner(c echo.Context) error {
	return envelope.Fail(c, http.StatusForbidden, "FORBIDDEN",
		"Only Meko's owner can sync a datapack to Meko Cloud: the web UI, or meko cloud_sync.")
}

// cloudSyncSetting is GET /settings/cloud-sync: {enabled}, whether datapacks can be synced to Meko
// Cloud. Off until the owner turns it on.
func (a *api) cloudSyncSetting(c echo.Context) error {
	return envelope.OK(c, map[string]bool{"enabled": a.Service.CloudSyncEnabled(ctx(c))})
}

// setCloudSyncSetting is PUT /settings/cloud-sync {enabled}, for the owner only.
func (a *api) setCloudSyncSetting(c echo.Context) error {
	if !a.owner(c) {
		return envelope.Fail(c, http.StatusForbidden, "FORBIDDEN", "Only Meko's owner can change this, in Settings.")
	}
	var body struct {
		Enabled *bool `json:"enabled"`
	}
	if ok, err := bind(c, &body); !ok {
		return err
	}
	if body.Enabled == nil {
		return badRequest(c, "enabled is true or false")
	}
	if err := a.Service.SetCloudSyncEnabled(ctx(c), *body.Enabled); err != nil {
		return err
	}
	return a.cloudSyncSetting(c)
}

// syncOff refuses a precheck or a sync while Cloud Sync is off.
func (a *api) syncOff(c echo.Context) (bool, error) {
	if a.Service.CloudSyncEnabled(ctx(c)) {
		return false, nil
	}
	return true, envelope.Fail(c, http.StatusConflict, "CLOUD_SYNC_OFF",
		"Cloud Sync is off: turn it on in Settings to sync a datapack to Meko Cloud.")
}

// syncError replies to a sync that couldn't start or a precheck that couldn't run.
func syncError(c echo.Context, err error) error {
	var refused *cloudsync.CloudError
	switch {
	case errors.Is(err, cloudsync.ErrNotLinked):
		return envelope.Fail(c, http.StatusConflict, "CLOUD_NOT_LINKED", err.Error())
	case errors.Is(err, cloudsync.ErrUnsupported):
		return envelope.Fail(c, http.StatusConflict, "CLOUD_SYNC_UNSUPPORTED", err.Error())
	case errors.As(err, &refused) && refused.Status/100 == 4:
		return envelope.Fail(c, refused.Status, refused.Code, refused.Error())
	case errors.As(err, &refused):
		return envelope.Fail(c, http.StatusBadGateway, "CLOUD_CALL_FAILED", refused.Error())
	}
	var link *cloudlink.CallError
	if errors.As(err, &link) {
		return cloudError(c, err)
	}
	return serviceError(c, err)
}

// syncPrecheck is POST /datapacks/:id/cloud-sync/precheck: whether the datapack fits the linked
// account's plan, {ok, plan, checks}.
func (a *api) syncPrecheck(c echo.Context) error {
	if !a.owner(c) {
		return a.syncOwner(c)
	}
	if off, err := a.syncOff(c); off {
		return err
	}
	id, ok, err := a.pathDatapack(c)
	if !ok {
		return err
	}
	pc, err := a.Sync.Precheck(ctx(c), id)
	if err != nil {
		return syncError(c, err)
	}
	return envelope.OK(c, pc)
}

// startSync is POST /datapacks/:id/cloud-sync: a precheck, then, when the datapack fits, its sync in
// the background (202 with the run). One that doesn't fit is a failed run with its precheck, and
// nothing is synced.
func (a *api) startSync(c echo.Context) error {
	if !a.owner(c) {
		return a.syncOwner(c)
	}
	if off, err := a.syncOff(c); off {
		return err
	}
	id, ok, err := a.pathDatapack(c)
	if !ok {
		return err
	}
	run, err := a.Sync.Start(ctx(c), id)
	if err != nil {
		return syncError(c, err)
	}
	return c.JSON(http.StatusAccepted, envelope.Response{Data: run})
}

// syncStatus is GET /datapacks/:id/cloud-sync: the datapack's latest sync since Meko started. It's
// read by id, since a synced datapack is no longer here; the owner's, like the sync.
func (a *api) syncStatus(c echo.Context) error {
	if !a.owner(c) {
		return a.syncOwner(c)
	}
	id, err := uuid.Parse(strings.TrimSpace(c.Param("id")))
	if err != nil {
		return badRequest(c, "datapack_id must be a UUID")
	}
	run, ok := a.Sync.Status(id.String())
	if !ok {
		return notFound(c, "no sync of this datapack since Meko started")
	}
	return envelope.OK(c, run)
}
