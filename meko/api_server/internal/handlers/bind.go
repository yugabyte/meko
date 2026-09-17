package handlers

import (
	"encoding/json"
	"errors"
	"io"

	"github.com/labstack/echo/v4"
)

// bindStrict decodes the JSON request body, refusing a body that is missing or that
// carries a field the destination does not declare.
//
// Echo's DefaultBinder does neither: it returns nil for a zero-length body, and drops
// unknown keys silently. Both turn a truncated or misspelled request into the zero
// value, which a handler cannot tell from a deliberate one — on the desired-state
// write that cost an environment its whole resource tree (CLOUDGA-36999). Use this
// wherever the zero value is destructive; c.Bind is fine where it is not.
//
// The decoder's own message is returned rather than a fixed string, because naming
// the offending field is the point: "json: unknown field \"resourses\"" is what tells
// a caller their payload was ignored.
func bindStrict(c echo.Context, dst any) error {
	dec := json.NewDecoder(c.Request().Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		if errors.Is(err, io.EOF) {
			return errors.New("request body is required")
		}
		return err
	}
	return nil
}
