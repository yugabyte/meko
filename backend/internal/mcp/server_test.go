package mcp

import (
	"encoding/json"
	"log/slog"
	"testing"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/yugabyte/meko/backend/internal/engine"
)

// A call stopped partway says what it did beside the error, so a retry knows what's left.
func TestPartialErrorSaysWhatWasDone(t *testing.T) {
	o := Options{Log: slog.New(slog.DiscardHandler)}
	c := &call{}
	res := o.result(&tool{name: "memory_promote", failCode: "promote_failed"}, c, nil,
		&partial{err: engine.ErrUnavailable, done: map[string]any{"inserted_ids": []string{"m1"}}})
	text, _ := res.Content[0].(*sdk.TextContent)
	var reply map[string]any
	if err := json.Unmarshal([]byte(text.Text), &reply); err != nil {
		t.Fatal(err)
	}
	ids, _ := reply["inserted_ids"].([]any)
	if res.IsError || reply["error"] != "engine_unavailable" || len(ids) != 1 || ids[0] != "m1" {
		t.Errorf("reply %s", text.Text)
	}
	if c.rec.OK || c.rec.ErrorCode != "engine_unavailable" {
		t.Errorf("call log row %+v", c.rec)
	}
}
