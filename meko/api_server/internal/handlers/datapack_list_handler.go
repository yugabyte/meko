package handlers

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/labstack/echo/v4"

	"meko/api_server/internal/auth"
	"meko/api_server/internal/models"
	"meko/api_server/internal/repository"
	"meko/api_server/internal/service"
)

const (
	datapackListMaxLimit = 1000
	datapackListMaxQuery = 200
	// datapackStreamFlushEvery bounds how much of the streamed list sits in the
	// response buffer before it goes out.
	datapackStreamFlushEvery = 256
)

// DatapackSummary is the default item shape of a paged datapack list. It keeps
// what list callers read and drops the connection fields (always redacted) and
// the counts (never computed by the list; datapack describe has them).
type DatapackSummary struct {
	CreatedAt        time.Time `json:"created_at"`
	UpdatedAt        time.Time `json:"updated_at"`
	ID               string    `json:"datapack_id"`
	Name             string    `json:"datapack_name"`
	Description      string    `json:"description,omitempty"`
	Grant            string    `json:"grant,omitempty"`
	UserID           string    `json:"user_id"`
	CreatedByAgentID string    `json:"created_by_agent_id,omitempty"`
	OwnerName        string    `json:"datapack_owner_name,omitempty"`
	OwnerEmail       string    `json:"datapack_owner_email_id,omitempty"`
}

// DatapackListPage is the data of GET /datapacks when the request carries
// limit. Items are DatapackSummary, or redacted full datapacks with
// fields=full. NextCursor is "" when HasMore is false; Total is present only
// with include_total=true.
type DatapackListPage struct {
	Items      any    `json:"items" swaggertype:"array,object"`
	Total      *int64 `json:"total,omitempty"`
	NextCursor string `json:"next_cursor"`
	Limit      int    `json:"limit"`
	HasMore    bool   `json:"has_more"`
}

func summarize(d models.Datapack) DatapackSummary {
	return DatapackSummary{
		CreatedAt:        d.CreatedAt,
		UpdatedAt:        d.UpdatedAt,
		ID:               d.ID,
		Name:             d.Name,
		Description:      d.Description,
		Grant:            d.Grant,
		UserID:           d.UserID,
		CreatedByAgentID: d.CreatedByAgentID,
		OwnerName:        d.OwnerName,
		OwnerEmail:       d.OwnerEmail,
	}
}

// List handles GET /api/v1/datapacks.
//
// Without limit, including an empty value, it returns every visible datapack as a bare array, newest
// first, exactly as before paging existed: the ycp agent garbage-collects
// whatever that array leaves out, and BYOC agents upgrade on the customer's
// schedule. The array is streamed, so its size does not bound memory. Filters
// are ignored in this mode so old and new servers answer a legacy request the
// same way. Clients that read everything use this mode rather than paging, since
// paging 1M rows at the per-PAT rate limit takes minutes.
//
// With limit it returns one page in a DatapackListPage envelope.
//
//	@Summary		List datapacks
//	@Description	Without `limit`: every visible datapack as an array, newest first (streamed).
//	@Description	With `limit`: one page as {items, has_more, next_cursor, limit, total?}.
//	@Description	Follow next_cursor for the next page.
//	@Tags			datapacks
//	@Produce		json
//	@Param			limit			query		int		false	"Page size 1-1000 (larger is clamped). A non-empty value selects paged mode."
//	@Param			cursor			query		string	false	"next_cursor from the previous page, with the same sort and filters"
//	@Param			sort			query		string	false	"created (newest first, default) or name (bytewise)"	Enums(created, name)
//	@Param			q				query		string	false	"Case-insensitive substring of datapack_name (max 200 chars)"
//	@Param			name			query		string	false	"Exact datapack_name"
//	@Param			grant			query		string	false	"owner or shared (default both)"	Enums(owner, shared)
//	@Param			fields			query		string	false	"summary (default) or full"	Enums(summary, full)
//	@Param			include_total	query		bool	false	"Add total, the number of rows matching the filters"
//	@Success		200				{object}	APIResponse{data=DatapackListPage{items=[]DatapackSummary}}
//	@Failure		400				{object}	APIResponse
//	@Failure		401				{object}	APIResponse
//	@Security		BearerAuth
//	@Router			/datapacks [get]
func (h *DatapackHandler) List(c echo.Context) error {
	account, err := auth.GetAccount(c)
	if err != nil || account == nil {
		return apiError(c, http.StatusUnauthorized,
			"UNAUTHORIZED", "missing account context")
	}
	// An empty limit is the same as no limit. Some clients send the parameter
	// with an empty value when it is unset, and that request has to stay on
	// the legacy array rather than 400.
	if strings.TrimSpace(c.QueryParam("limit")) == "" {
		return h.streamAll(c, account.ID)
	}

	req, full, code, msg := parseDatapackListParams(c)
	if code != "" {
		return apiError(c, http.StatusBadRequest, code, msg)
	}
	page, err := h.svc.ListPage(c.Request().Context(), account.ID, req)
	if errors.Is(err, service.ErrInvalidDatapackCursor) {
		return apiError(c, http.StatusBadRequest, "INVALID_CURSOR",
			"cursor is invalid or was issued for a different sort or filters")
	}
	if err != nil {
		return apiError(c, http.StatusInternalServerError, "INTERNAL_ERROR", err.Error())
	}

	out := DatapackListPage{
		HasMore:    page.HasMore,
		NextCursor: page.NextCursor,
		Limit:      page.Limit,
		Total:      page.Total,
	}
	if full {
		redactDatapacks(page.Items)
		out.Items = page.Items
	} else {
		items := make([]DatapackSummary, len(page.Items))
		for i, d := range page.Items {
			items[i] = summarize(d)
		}
		out.Items = items
	}
	return ok(c, http.StatusOK, out)
}

// parseDatapackListParams validates the paged-mode query. A non-empty code is
// a 400.
func parseDatapackListParams(c echo.Context) (req service.DatapackListRequest, full bool, code, msg string) {
	limit, err := strconv.Atoi(strings.TrimSpace(c.QueryParam("limit")))
	if err != nil || limit < 1 {
		return req, false, "INVALID_LIMIT", "limit must be an integer from 1 to 1000"
	}
	req.Limit = min(limit, datapackListMaxLimit)

	switch req.Sort = strings.TrimSpace(c.QueryParam("sort")); req.Sort {
	case "":
		req.Sort = repository.DatapackSortCreated
	case repository.DatapackSortCreated, repository.DatapackSortName:
	default:
		return req, false, "INVALID_SORT", "sort must be created or name"
	}

	switch req.Grant = strings.TrimSpace(c.QueryParam("grant")); req.Grant {
	case "", repository.DatapackGrantOwner, repository.DatapackGrantShared:
	default:
		return req, false, "INVALID_GRANT", "grant must be owner or shared"
	}

	switch f := strings.TrimSpace(c.QueryParam("fields")); f {
	case "", "summary":
	case "full":
		full = true
	default:
		return req, false, "INVALID_FIELDS", "fields must be summary or full"
	}

	req.Q = strings.TrimSpace(c.QueryParam("q"))
	if utf8.RuneCountInString(req.Q) > datapackListMaxQuery {
		return req, false, "INVALID_QUERY", "q must be at most 200 characters"
	}
	req.Name = strings.TrimSpace(c.QueryParam("name"))

	if v := strings.TrimSpace(c.QueryParam("include_total")); v != "" {
		b, pErr := strconv.ParseBool(v)
		if pErr != nil {
			return req, false, "INVALID_INCLUDE_TOTAL", "include_total must be true or false"
		}
		req.IncludeTotal = b
	}
	req.Cursor = strings.TrimSpace(c.QueryParam("cursor"))
	return req, full, "", ""
}

// streamAll writes {"data":[...]} one row at a time, byte for byte what
// ok(c, 200, datapacks) wrote for the same rows. A failure before the first
// byte is the normal 500 envelope. A failure after it aborts the connection,
// so the client reads a truncated body it cannot parse rather than a short
// list it would take as complete.
func (h *DatapackHandler) streamAll(c echo.Context, userID string) error {
	res := c.Response()
	started := false
	n := 0
	err := h.svc.StreamAll(c.Request().Context(), userID, func(d models.Datapack) error {
		redactDatapack(&d)
		b, mErr := json.Marshal(d)
		if mErr != nil {
			return mErr
		}
		sep := []byte(",")
		if !started {
			res.Header().Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
			res.WriteHeader(http.StatusOK)
			sep = []byte(`{"data":[`)
			started = true
		}
		if _, wErr := res.Write(sep); wErr != nil {
			return wErr
		}
		if _, wErr := res.Write(b); wErr != nil {
			return wErr
		}
		if n++; n%datapackStreamFlushEvery == 0 {
			res.Flush()
		}
		return nil
	})
	if err != nil {
		if !started {
			return apiError(c, http.StatusInternalServerError, "INTERNAL_ERROR", err.Error())
		}
		slog.Error("datapack list stream failed mid-response; aborting connection",
			"user_id", userID, "rows_written", n, "error", err)
		panic(http.ErrAbortHandler)
	}
	if !started {
		return ok(c, http.StatusOK, []models.Datapack{})
	}
	_, err = res.Write([]byte("]}\n"))
	return err
}

// Count handles GET /api/v1/datapacks/count.
//
//	@Summary	Count datapacks
//	@Tags		datapacks
//	@Produce	json
//	@Success	200	{object}	APIResponse{data=service.DatapackCounts}
//	@Failure	401	{object}	APIResponse
//	@Security	BearerAuth
//	@Router		/datapacks/count [get]
func (h *DatapackHandler) Count(c echo.Context) error {
	account, err := auth.GetAccount(c)
	if err != nil || account == nil {
		return apiError(c, http.StatusUnauthorized,
			"UNAUTHORIZED", "missing account context")
	}
	counts, err := h.svc.Counts(c.Request().Context(), account.ID)
	if err != nil {
		return apiError(c, http.StatusInternalServerError, "INTERNAL_ERROR", err.Error())
	}
	return ok(c, http.StatusOK, counts)
}
