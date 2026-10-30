package rest

import (
	"errors"
	"net/http"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/yugabyte/meko/backend/internal/envelope"
	"github.com/yugabyte/meko/backend/internal/memory"
)

// extractorStatus is GET /settings/memory-extractor: whether memories are extracted from turns, through
// what, and how its runs have gone (Meko's own; Cloud extracts on its own LLM).
func (a *api) extractorStatus(c echo.Context) error {
	st, err := a.Service.ExtractorStatus(ctx(c))
	if err != nil {
		return serviceError(c, err)
	}
	return envelope.OK(c, st)
}

// setExtractor is PUT /settings/memory-extractor {harness, command, model, interval_seconds}, for the
// owner only (the CLI's admin token, or the web UI's Settings): it makes the server run a program.
// harness "" turns it off; with no command, the server finds the harness's itself, and only the
// admin token may name one.
func (a *api) setExtractor(c echo.Context) error {
	if !a.owner(c) {
		return envelope.Fail(c, http.StatusForbidden, "FORBIDDEN",
			"Only Meko's owner can set up the memory extractor: meko memory-extractor use claude, or Settings.")
	}
	var body struct {
		memory.ExtractorSettings
		IntervalSeconds int `json:"interval_seconds"`
	}
	ok, berr := bind(c, &body)
	if !ok {
		return berr
	}
	body.Interval = time.Duration(body.IntervalSeconds) * time.Second
	if body.Command, ok = a.harnessCommand(c, body.Harness, body.Command); !ok {
		return namedCommand(c)
	}
	if err := a.Service.SetExtractor(ctx(c), body.ExtractorSettings); err != nil {
		return serviceError(c, err)
	}
	return a.extractorStatus(c)
}

// runExtractor is POST /settings/memory-extractor/run: a run now, and what it did. Calls that failed
// are in the reply (failed, last_error); only what stops the whole run is an error.
func (a *api) runExtractor(c echo.Context) error {
	run, err := a.Service.ExtractPending(ctx(c))
	if errors.Is(err, memory.ErrExtractorOff) {
		return envelope.Fail(c, http.StatusConflict, "EXTRACTOR_OFF", err.Error())
	}
	if err != nil {
		return serviceError(c, err)
	}
	return envelope.OK(c, run)
}

// datapackExtraction is GET /datapacks/:id/memory-extraction: whether the datapack opted out, and its
// profile and instructions.
func (a *api) datapackExtraction(c echo.Context) error {
	id, ok, err := a.pathDatapack(c)
	if !ok {
		return err
	}
	d, err := a.Service.DatapackExtraction(ctx(c), id)
	if err != nil {
		return serviceError(c, err)
	}
	return envelope.OK(c, map[string]any{"datapack_id": id, "opt_out": d.OptOut, "profile": d.Profile,
		"instructions": d.Instructions, "profiles": memory.ExtractionProfiles()})
}

// setDatapackExtraction is PATCH /datapacks/:id/memory-extraction {opt_out, profile, instructions}.
func (a *api) setDatapackExtraction(c echo.Context) error {
	id, ok, err := a.pathDatapack(c)
	if !ok {
		return err
	}
	var body memory.DatapackExtractionChange
	if ok, berr := bind(c, &body); !ok {
		return berr
	}
	if _, err = a.Service.SetDatapackExtraction(ctx(c), id, body); err != nil {
		return serviceError(c, err)
	}
	return a.datapackExtraction(c)
}
