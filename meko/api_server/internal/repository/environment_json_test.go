package repository

import (
	"encoding/json"
	"testing"
)

// json.Marshal of a nil slice yields `null` -- four bytes, so the length check this
// replaced waved it through and the column ended up holding a null where every
// reader expects a list. ApplyStatus guards both fields on nil so a status report
// cannot reach it today; this is the floor under every other caller.
func TestJSONArray_CoercesAbsentAndNullToAnEmptyArray(t *testing.T) {
	nilSlice, err := json.Marshal([]string(nil))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	for name, raw := range map[string]json.RawMessage{
		"nil":               nil,
		"empty":             json.RawMessage(""),
		"blank":             json.RawMessage("  \n"),
		"literal null":      json.RawMessage("null"),
		"padded null":       json.RawMessage(" null "),
		"marshalled nil":    nilSlice,
		"already empty set": json.RawMessage("[]"),
	} {
		t.Run(name, func(t *testing.T) {
			if got := string(jsonArray(raw)); got != "[]" {
				t.Errorf("jsonArray(%q) = %q, want []", raw, got)
			}
		})
	}
}

// A real document has to survive untouched, byte for byte -- the coercion is a
// floor, not a filter.
func TestJSONArray_LeavesARealDocumentAlone(t *testing.T) {
	for _, raw := range []string{
		`[{"name":"api-server","healthy":1,"total":1}]`,
		`[{"kind":"datapack","id":"dp-1"}]`,
		// "null" only as a value, not as the whole document.
		`[{"name":"rag-worker","error":null}]`,
	} {
		if got := string(jsonArray(json.RawMessage(raw))); got != raw {
			t.Errorf("jsonArray(%s) = %s, want it unchanged", raw, got)
		}
	}
}
