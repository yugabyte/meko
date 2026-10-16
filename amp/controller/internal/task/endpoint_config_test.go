package task

import (
	"encoding/json"
	"testing"

	"github.com/yugabyte/amp/controller/internal/core"
)

// A create that sends an empty list stores no key for it, so the row matches
// one created without the field.
func TestEndpointConfigOmitsEmptySchedulingLists(t *testing.T) {
	b, err := endpointConfig(CreateEndpointArgs{
		Tolerations:               json.RawMessage(`[]`),
		TopologySpreadConstraints: json.RawMessage(`[{"maxSkew":1}]`),
	}, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	var cfg map[string]json.RawMessage
	if err := json.Unmarshal(b, &cfg); err != nil {
		t.Fatal(err)
	}
	if _, ok := cfg[core.ConfigTolerations]; ok {
		t.Errorf("config = %s, want no %s key", b, core.ConfigTolerations)
	}
	if _, ok := cfg[core.ConfigTopologySpreadConstraints]; !ok {
		t.Errorf("config = %s, want the %s key", b, core.ConfigTopologySpreadConstraints)
	}
}
