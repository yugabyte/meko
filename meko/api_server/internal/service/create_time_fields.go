package service

import (
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"

	"meko/api_server/internal/repository"
)

// ErrCreateTimeField is returned when an update changes a field the resource's
// backend sets at create and cannot change afterwards. Mapped to 400 by the handler.
var ErrCreateTimeField = errors.New("field cannot be changed after create")

// endpointCreateTimeFields are the amp endpoint-spec fields the controller sets at
// create and has no verb to change, each with the value an omitted one stands for
// and the inventory attr the agent reports it under ("" when it reports none).
// The reconcile agent can only report a change to one as drift (ampbackend's
// unappliedEndpoint), so the write is refused instead of being accepted and left
// unapplied (CLOUDGA-37410). connectivity.allowed_cidrs is deliberately absent: it
// is applied in place. A field with no default is compared only when the stored
// spec declared it, since this side cannot tell what create defaulted it to.
var endpointCreateTimeFields = []struct {
	path []string
	dflt any
	attr string
}{
	{[]string{"connectivity", "mode"}, "cluster_ip", "connectivity_mode"},
	{[]string{"role"}, nil, "role"},
	{[]string{"tags"}, map[string]any{}, ""},
	{[]string{"extended_config"}, map[string]any{}, "extended_config"},
}

// refuseCreateTimeChange is the repository.UpdateGuard for the region PATCH: an
// update changing an endpoint's create-time field is ErrCreateTimeField once the
// endpoint exists. Before that — not yet reported, or reported only as a create the
// backend refused — the spec is still the create's input, and rewriting it is how a
// refused create is fixed.
//
// A new value the agent reports the endpoint as already having is allowed: that is
// a spec which had drifted being put back, and refusing it would leave the endpoint
// in drift with no way out but recreating it.
func refuseCreateTimeChange(
	kind, name string, before, after json.RawMessage, observed func() (*repository.ObservedResource, error),
) error {
	if kind != "endpoint" {
		return nil
	}
	var was, now map[string]json.RawMessage
	if json.Unmarshal(before, &was) != nil || json.Unmarshal(after, &now) != nil {
		return nil
	}
	type change struct {
		field string
		value any
		attr  string
	}
	var changes []change
	for _, f := range endpointCreateTimeFields {
		a, aSet := specField(was, f.path)
		b, bSet := specField(now, f.path)
		if !aSet && f.dflt == nil {
			continue
		}
		if !aSet {
			a = f.dflt
		}
		if !bSet {
			b = f.dflt
		}
		if !reflect.DeepEqual(a, b) {
			changes = append(changes, change{strings.Join(f.path, "."), b, f.attr})
		}
	}
	if len(changes) == 0 {
		return nil
	}
	obs, err := observed()
	if err != nil {
		return err
	}
	// The state cannot say whether the endpoint exists: one whose last update was
	// refused reads rejected or blocked exactly as a refused create does. A row the
	// backend listed carries the endpoint's id; one it listed but could not read
	// this tick is "unreadable" with its attrs cleared, and exists all the same.
	if obs == nil || (obs.Attrs["id"] == "" && obs.State != "unreadable") {
		return nil
	}
	var refused []string
	for _, c := range changes {
		if !matchesReported(c.value, obs.Attrs, c.attr) {
			refused = append(refused, c.field)
		}
	}
	if len(refused) == 0 {
		return nil
	}
	return fmt.Errorf("%w: %s/%s: %s", ErrCreateTimeField, kind, name, strings.Join(refused, ", "))
}

// matchesReported reports whether v is what the agent reports under attr. A string
// attr is compared as is; the settings surfaces are reported as a JSON object.
func matchesReported(v any, attrs map[string]string, attr string) bool {
	reported, ok := attrs[attr]
	if attr == "" || !ok {
		return false
	}
	if s, isString := v.(string); isString {
		return s == reported
	}
	var r any
	return json.Unmarshal([]byte(reported), &r) == nil && reflect.DeepEqual(v, r)
}

// specField decodes the value at path in a spec, reporting false for a missing or
// null one so the two read alike.
func specField(spec map[string]json.RawMessage, path []string) (any, bool) {
	raw, ok := spec[path[0]]
	if !ok {
		return nil, false
	}
	if len(path) > 1 {
		var inner map[string]json.RawMessage
		if json.Unmarshal(raw, &inner) != nil {
			return nil, false
		}
		return specField(inner, path[1:])
	}
	var v any
	if json.Unmarshal(raw, &v) != nil || v == nil {
		return nil, false
	}
	return v, true
}
