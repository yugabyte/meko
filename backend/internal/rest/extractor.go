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
// CLI's admin token only: it points the server at a program to run. harness "" turns it off.
func (a *api) setExtractor(c echo.Context) error {
	if a.Credential(c) != "admin" {
		return envelope.Fail(c, http.StatusForbidden, "FORBIDDEN",
			"Set up the memory extractor with meko memory-extractor use claude.")
	}
	var body struct {
		memory.ExtractorSettings
		IntervalSeconds int `json:"interval_seconds"`
	}
	if ok, berr := bind(c, &body); !ok {
		return berr
	}
	body.Interval = time.Duration(body.IntervalSeconds) * time.Second
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
