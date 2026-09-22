package handlers

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/labstack/echo/v4"
)

// Echo's DefaultBinder returns nil -- leaving the target struct zero-valued -- when
// ContentLength is 0, BEFORE it looks at Content-Type at all. So a write endpoint
// that only calls c.Bind treats "no body" as "a report of all-empty fields" and
// happily persists it. That is CLOUDGA-36984: a status POST whose body was dropped
// upstream answered 204 while blanking the environment's state.
//
// Content-Type is checked first, so a client that sent the wrong type is told that
// rather than that its body was empty -- which, the body having been dropped in
// transit, is true but useless.

// jsonBodyProblem reports why a request body cannot be a JSON document, if it
// cannot. It is the single source of truth behind both JSONBodyGuard and bindJSON,
// so the middleware and the per-handler helper cannot drift apart.
func jsonBodyProblem(c echo.Context) (status int, code, message string, bad bool) {
	req := c.Request()
	base, _, _ := strings.Cut(req.Header.Get(echo.HeaderContentType), ";")
	if !strings.EqualFold(strings.TrimSpace(base), echo.MIMEApplicationJSON) {
		return http.StatusUnsupportedMediaType, "UNSUPPORTED_MEDIA_TYPE",
			"this endpoint accepts application/json only", true
	}
	// A negative ContentLength is a chunked body of unknown size: let it through and
	// leave the verdict to the decoder, which rejects an empty stream on its own.
	if req.ContentLength == 0 {
		return http.StatusBadRequest, "EMPTY_BODY", "request body is required", true
	}
	return 0, "", "", false
}

// JSONBodyGuard rejects a request whose body cannot be JSON before any handler runs,
// so a route added to a guarded group later cannot forget the check.
//
// Body-less methods are skipped rather than exempted by route, which keeps the guard
// correct for GET /agent/desired-state without the group having to enumerate it, and
// keeps it correct for a body-less route added to the group later. Skipping OPTIONS
// also makes the guard independent of where CORS preflight is handled.
func JSONBodyGuard() echo.MiddlewareFunc {
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) error {
			switch c.Request().Method {
			case http.MethodGet, http.MethodHead, http.MethodDelete, http.MethodOptions:
				return next(c)
			}
			if status, code, message, bad := jsonBodyProblem(c); bad {
				return apiError(c, status, code, message)
			}
			return next(c)
		}
	}
}

// bindJSON decodes a JSON request body, refusing an absent body and reporting a
// wrong media type as the 415 it is. It returns false having already written the
// error response -- the same convention as resolveAgent. A caller testing
// apiError's own return for nil would run on past a request it had just
// rejected: apiError's return is c.JSON's write error, nil on any successful
// write regardless of status. writeAndFail (response.go) fixes that trap for
// new call sites by returning a real sentinel instead of apiError's own
// result (see MEKO-656/MEKO-690, where trusting that result let a rejected
// request fall through to the real handler logic). bindJSON/resolveAgent
// predate writeAndFail and solve the same problem their own way, with a bool
// return -- either shape is fine, this one just isn't obligated to switch.
//
// Handlers call it even on routes JSONBodyGuard already covers: a handler invoked
// directly (as most tests in this package do) or registered outside a guarded group
// still gets the same answer, and both paths derive it from jsonBodyProblem.
func bindJSON(c echo.Context, v any) bool {
	if status, code, message, bad := jsonBodyProblem(c); bad {
		_ = apiError(c, status, code, message)
		return false
	}
	if err := c.Bind(v); err != nil {
		// Echo answers a non-JSON media type with 415. The check above catches that
		// first, but a binder error still carries a status worth preserving rather
		// than flattening every failure to "malformed".
		var he *echo.HTTPError
		if errors.As(err, &he) && he.Code == http.StatusUnsupportedMediaType {
			_ = apiError(c, http.StatusUnsupportedMediaType, "UNSUPPORTED_MEDIA_TYPE",
				"this endpoint accepts application/json only")
			return false
		}
		_ = apiError(c, http.StatusBadRequest, "INVALID_JSON", "malformed request body")
		return false
	}
	return true
}

// bindJSONStrict is bindJSON that also refuses a field the target does not declare,
// for a handler where a silently dropped key changes what the request means.
//
// SetDesired is the case it exists for: desired state is replaced, so a key the
// decoder ignores is a resource the agent deletes. `{"resourses": [...]}` bound to an
// empty set and emptied the environment (CLOUDGA-36999). The strictness reaches every
// level, since one decoder walks the whole document -- a typo beside a resource's
// `kind` is refused like one beside `resources`. `spec` is the deliberate exception:
// json.RawMessage, so the decoder never descends into it and a field only a newer
// agent knows still reaches it.
//
// Not the default. Rejecting unknown fields on an agent-facing route would refuse the
// heartbeats of an agent newer than the control plane, and the customer controls that
// upgrade order (REVIEW.md's rule for this contract); AgentStatus therefore keeps
// bindJSON and validates the fields it cares about instead.
func bindJSONStrict(c echo.Context, v any) bool {
	if status, code, message, bad := jsonBodyProblem(c); bad {
		_ = apiError(c, status, code, message)
		return false
	}
	dec := json.NewDecoder(c.Request().Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		// The decoder's own message names the offending field -- "json: unknown field
		// \"resourses\"" is precisely what tells a caller its payload was ignored --
		// so it is worth more here than the fixed string bindJSON returns.
		_ = apiError(c, http.StatusBadRequest, "INVALID_JSON", err.Error())
		return false
	}
	return true
}
