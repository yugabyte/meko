package service

import (
	"encoding/json"
	"errors"
	"testing"

	"meko/api_server/internal/repository"
)

const createTimeEndpoint = "pool-a/main-db/main/rw"

func observedRow(obs *repository.ObservedResource) func() (*repository.ObservedResource, error) {
	return func() (*repository.ObservedResource, error) { return obs, nil }
}

var listedEndpoint = &repository.ObservedResource{State: "ready", Attrs: map[string]string{"id": "e1"}}

const storedEndpoint = `{"role":"read_write","tags":{"team":"a"},` +
	`"connectivity":{"mode":"nlb_external","allowed_cidrs":["10.0.0.0/16"]}}`

// A field the controller fixes at create is refused once the endpoint exists,
// rather than stored and left as drift (CLOUDGA-37410).
func TestRefuseCreateTimeChange_RefusesEachField(t *testing.T) {
	for name, after := range map[string]string{
		"mode": `{"role":"read_write","tags":{"team":"a"},"connectivity":{"mode":"nlb_internal"}}`,
		"role": `{"role":"read_only","tags":{"team":"a"},"connectivity":{"mode":"nlb_external"}}`,
		"tags": `{"role":"read_write","tags":{"team":"b"},"connectivity":{"mode":"nlb_external"}}`,
		"extended_config": `{"role":"read_write","tags":{"team":"a"},"connectivity":{"mode":"nlb_external"},` +
			`"extended_config":{"x":1}}`,
	} {
		t.Run(name, func(t *testing.T) {
			err := refuseCreateTimeChange("endpoint", createTimeEndpoint,
				json.RawMessage(storedEndpoint), json.RawMessage(after), observedRow(listedEndpoint))
			if !errors.Is(err, ErrCreateTimeField) {
				t.Fatalf("err = %v, want ErrCreateTimeField", err)
			}
		})
	}
}

// An existing endpoint whose last update was refused reports rejected, exactly as
// a refused create does; it is still an existing endpoint, and the listed row's id
// is what says so.
func TestRefuseCreateTimeChange_RefusesOnAnExistingEndpointWhoseUpdateWasRejected(t *testing.T) {
	obs := &repository.ObservedResource{State: "rejected", Attrs: map[string]string{"id": "e1"}}
	err := refuseCreateTimeChange("endpoint", createTimeEndpoint, json.RawMessage(storedEndpoint),
		json.RawMessage(`{"role":"read_only"}`), observedRow(obs))
	if !errors.Is(err, ErrCreateTimeField) {
		t.Fatalf("err = %v, want ErrCreateTimeField", err)
	}
}

// Before the endpoint exists — not reported, or reported only as a refused create,
// which carries no attrs — rewriting the spec is how the create is fixed.
func TestRefuseCreateTimeChange_AllowsBeforeCreate(t *testing.T) {
	for name, obs := range map[string]*repository.ObservedResource{
		"not reported":   nil,
		"create refused": {State: "rejected"},
	} {
		t.Run(name, func(t *testing.T) {
			if err := refuseCreateTimeChange("endpoint", createTimeEndpoint, json.RawMessage(storedEndpoint),
				json.RawMessage(`{"role":"read_only"}`), observedRow(obs)); err != nil {
				t.Fatalf("err = %v, want the rewrite allowed", err)
			}
		})
	}
}

// allowed_cidrs is applied in place, and declaring the default for a field the
// stored spec omitted is not a change; neither even consults the inventory.
func TestRefuseCreateTimeChange_AllowsWhatIsNotAChange(t *testing.T) {
	for name, tc := range map[string]struct{ before, after string }{
		"allowed_cidrs": {storedEndpoint, `{"role":"read_write","tags":{"team":"a"},` +
			`"connectivity":{"mode":"nlb_external","allowed_cidrs":["10.0.0.0/16","192.0.2.10/32"]}}`},
		"declared defaults":   {`{}`, `{"connectivity":{"mode":"cluster_ip"},"tags":{},"extended_config":{}}`},
		"role first declared": {`{}`, `{"role":"read_write"}`},
	} {
		t.Run(name, func(t *testing.T) {
			lookup := func() (*repository.ObservedResource, error) {
				t.Error("inventory read for a write that changes nothing create-time")
				return listedEndpoint, nil
			}
			if err := refuseCreateTimeChange("endpoint", createTimeEndpoint,
				json.RawMessage(tc.before), json.RawMessage(tc.after), lookup); err != nil {
				t.Fatalf("err = %v, want allowed", err)
			}
		})
	}
}

func TestRefuseCreateTimeChange_OnlyEndpoints(t *testing.T) {
	if err := refuseCreateTimeChange("branch", "pool-a/main-db/main", json.RawMessage(`{"role":"a"}`),
		json.RawMessage(`{"role":"b"}`), observedRow(listedEndpoint)); err != nil {
		t.Fatalf("err = %v, want other kinds untouched", err)
	}
}

// A spec that had already drifted — stored nlb_internal for an endpoint that is
// really nlb_external — must be correctable: the new value is what the agent
// reports, so the write converges rather than drifts.
func TestRefuseCreateTimeChange_AllowsRevertingToWhatIsReported(t *testing.T) {
	obs := &repository.ObservedResource{State: "drift", Attrs: map[string]string{
		"id": "e1", "connectivity_mode": "nlb_external", "role": "read_write", "extended_config": `{"x":1}`,
	}}
	for name, tc := range map[string]struct{ before, after string }{
		"mode":            {`{"connectivity":{"mode":"nlb_internal"}}`, `{"connectivity":{"mode":"nlb_external"}}`},
		"role":            {`{"role":"read_only"}`, `{"role":"read_write"}`},
		"extended_config": {`{"extended_config":{"x":2}}`, `{"extended_config":{"x":1}}`},
	} {
		t.Run(name, func(t *testing.T) {
			if err := refuseCreateTimeChange("endpoint", createTimeEndpoint,
				json.RawMessage(tc.before), json.RawMessage(tc.after), observedRow(obs)); err != nil {
				t.Fatalf("err = %v, want the revert allowed", err)
			}
		})
	}
	// Tags are not reported, so there is nothing to revert to and the change stays refused.
	err := refuseCreateTimeChange("endpoint", createTimeEndpoint, json.RawMessage(`{"tags":{"a":"1"}}`),
		json.RawMessage(`{"tags":{"a":"2"}}`), observedRow(obs))
	if !errors.Is(err, ErrCreateTimeField) {
		t.Fatalf("tags: err = %v, want ErrCreateTimeField", err)
	}
}

// An endpoint the agent listed but could not read this tick has its attrs cleared;
// it still exists, so a change is refused rather than stored as drift.
func TestRefuseCreateTimeChange_RefusesOnAnUnreadableEndpoint(t *testing.T) {
	err := refuseCreateTimeChange("endpoint", createTimeEndpoint, json.RawMessage(storedEndpoint),
		json.RawMessage(`{"role":"read_only"}`), observedRow(&repository.ObservedResource{State: "unreadable"}))
	if !errors.Is(err, ErrCreateTimeField) {
		t.Fatalf("err = %v, want ErrCreateTimeField", err)
	}
}

// An inventory that cannot be read fails the write rather than waving it through.
func TestRefuseCreateTimeChange_FailsClosedOnAnInventoryError(t *testing.T) {
	boom := errors.New("decode observed resources")
	err := refuseCreateTimeChange("endpoint", createTimeEndpoint, json.RawMessage(storedEndpoint),
		json.RawMessage(`{"role":"read_only"}`), func() (*repository.ObservedResource, error) { return nil, boom })
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v, want the inventory error", err)
	}
}
