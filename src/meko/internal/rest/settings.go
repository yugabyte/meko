package rest

import (
	"github.com/labstack/echo/v4"

	"github.com/yugabyte/meko/src/meko/internal/envelope"
)

// usageStats is GET /settings/usage-stats: whether usage stats are sent to YugabyteDB, and what
// turned them off.
func (a *api) usageStats(c echo.Context) error {
	st, err := a.UsageStats.State(ctx(c))
	if err != nil {
		return err
	}
	return envelope.OK(c, st)
}

// setUsageStats is PATCH /settings/usage-stats {enabled}: the web UI's setting. With them turned
// off by config.toml or the environment, it's saved but they stay off (forced_off in the reply).
func (a *api) setUsageStats(c echo.Context) error {
	var body struct {
		Enabled *bool `json:"enabled"`
	}
	if ok, err := bind(c, &body); !ok {
		return err
	}
	if body.Enabled == nil {
		return badRequest(c, "enabled is true or false")
	}
	st, err := a.UsageStats.SetEnabled(ctx(c), *body.Enabled)
	if err != nil {
		return err
	}
	return envelope.OK(c, st)
}
