package foundation

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"meko/foundation/ybuniverse"
)

// Phase is the foundation as a whole, for a reader who wants one word.
type Phase string

const (
	PhaseConverging Phase = "Converging"
	PhaseReady      Phase = "Ready"
	PhaseFailed     Phase = "Failed"
)

// ResourceState is one declared CR, reduced to the three answers a consumer
// acts on. The CR's own wording is kept alongside in Raw, because the
// vocabulary is YBA's: `KMSConfig.status.state` documents four values as
// examples with no enum while its reconciler writes five, and `universeState`
// reports "Error Creating", which the CRD never mentions.
type ResourceState string

const (
	ResourceAbsent  ResourceState = "Absent"
	ResourcePending ResourceState = "Pending"
	ResourceReady   ResourceState = "Ready"
	ResourceFailed  ResourceState = "Failed"
)

// ResourceStatus mirrors one CR's own status.
type ResourceStatus struct {
	Kind    string        `json:"kind"`
	Name    string        `json:"name"`
	State   ResourceState `json:"state"`
	Raw     string        `json:"raw,omitempty"`
	Message string        `json:"message,omitempty"`
	UUID    string        `json:"resource_uuid,omitempty"`
}

// YBAStatus is the one thing the bring-up blocks on.
type YBAStatus struct {
	Release   string `json:"release"`
	Installed bool   `json:"installed"`
	Customers *int   `json:"customers,omitempty"`
	Message   string `json:"message,omitempty"`
}

// Status is the whole picture: whether YBA is usable, what every declared CR
// says about itself, and how to reach the universe when there is an address.
//
// Each resource entry is the CR's OWN status rather than a verdict computed
// here. That is what makes a reference which never becomes ready visible: the
// entry sits at Pending or Failed carrying YBA's wording, instead of being
// invisible because no wait happened to be looking at the time.
type Status struct {
	Namespace string           `json:"namespace"`
	Phase     Phase            `json:"phase"`
	YBA       YBAStatus        `json:"yba"`
	Resources []ResourceStatus `json:"resources"`
	// ResourceStates is Resources keyed by kind, and exists only so that a
	// printer column can reach one: JSONPath cannot filter a list by one field
	// and project another, so `.status.resources[?(@.kind=="YBUniverse")].state`
	// is not expressible and the universe could not be a column without this.
	ResourceStates map[string]ResourceState `json:"resourceStates,omitempty"`
	Address        *Info                    `json:"address,omitempty"`
}

// resourceStates collapses the list to one state per kind, keeping the state
// FURTHEST from ready when a spec declares several of a kind. A column that
// read "Ready" while one of two providers was failing would be worse than no
// column, and the list underneath still has the per-object detail.
func resourceStates(rs []ResourceStatus) map[string]ResourceState {
	rank := map[ResourceState]int{ResourceReady: 0, ResourcePending: 1, ResourceAbsent: 2, ResourceFailed: 3}
	out := map[string]ResourceState{}
	for _, r := range rs {
		if cur, ok := out[r.Kind]; !ok || rank[r.State] > rank[cur] {
			out[r.Kind] = r.State
		}
	}
	return out
}

// crStatus is every status field any kind in the bundle carries that says
// whether it is done. No kind has all of them: the providers have `state`,
// Release/StorageConfig/YBCertificate have `success`, and the universe has its
// own pair. Decoding them together means a kind nobody has used yet still
// reports something, rather than needing a case here first.
type crStatus struct {
	State         string              `json:"state"`
	Success       *bool               `json:"success"`
	Message       string              `json:"message"`
	ResourceUUID  string              `json:"resourceUUID"`
	UniverseState string              `json:"universeState"`
	Actions       []ybuniverse.Action `json:"actions"`
}

// Status reads every declared CR and reports what each says about itself.
func (d *Deployer) Status(ctx context.Context) (*Status, error) {
	manifests, err := d.spec.Render()
	if err != nil {
		return nil, err
	}
	st := &Status{Namespace: d.spec.Namespace, YBA: d.ybaStatus(ctx)}
	for _, m := range manifests {
		st.Resources = append(st.Resources, d.resourceStatus(ctx, m.Kind, m.ObjectName))
	}
	// Best effort: the address exists only once the universe has produced its
	// Service, and not having one yet is a phase, not a failure.
	if info, err := d.Connect(ctx); err == nil {
		st.Address = info
	}
	st.ResourceStates = resourceStates(st.Resources)
	st.Phase = phaseOf(st)
	return st, nil
}

func phaseOf(st *Status) Phase {
	for _, r := range st.Resources {
		if r.State == ResourceFailed {
			return PhaseFailed
		}
	}
	if !st.YBA.Installed {
		return PhaseConverging
	}
	for _, r := range st.Resources {
		if r.State != ResourceReady {
			return PhaseConverging
		}
	}
	// The address is the last thing to appear, and a consumer cannot use the
	// foundation without it, so Ready means reachable and not merely applied.
	if st.Address == nil {
		return PhaseConverging
	}
	return PhaseReady
}

func (d *Deployer) ybaStatus(ctx context.Context) YBAStatus {
	y := YBAStatus{Release: d.spec.HelmRelease()}
	names, err := d.yugawareStatefulSets(ctx)
	if err != nil || len(names) == 0 {
		y.Message = "no yugaware StatefulSet matching " + d.yugawareSelector()
		return y
	}
	y.Installed = true
	body, where, ok := d.readCustomerCount(ctx, "statefulset/"+names[0])
	if !ok {
		y.Message = "could not read the customer count from " + where
		return y
	}
	var doc struct {
		Count *int `json:"count"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(body)), &doc); err != nil {
		y.Message = "could not parse a customer count from " + strings.TrimSpace(body)
		return y
	}
	y.Customers = doc.Count
	// Exactly one or the operator refuses every CR, so anything else is worth
	// saying here rather than only at install time.
	if doc.Count != nil && *doc.Count != 1 {
		y.Message = fmt.Sprintf("YBA has %d customers and the operator requires exactly one",
			*doc.Count)
	}
	return y
}

func (d *Deployer) resourceStatus(ctx context.Context, kind, name string) ResourceStatus {
	r := ResourceStatus{Kind: kind, Name: name, State: ResourceAbsent}
	res, err := d.kubectl(ctx, nil, "get", strings.ToLower(kind), name, "-o", "json")
	if err != nil || res.ExitCode != 0 {
		return r
	}
	var doc struct {
		Status crStatus `json:"status"`
	}
	if err := json.Unmarshal([]byte(res.Stdout), &doc); err != nil {
		r.State = ResourcePending
		r.Message = "could not parse the CR's status"
		return r
	}
	s := doc.Status
	r.Message = strings.TrimSpace(s.Message)
	r.UUID = s.ResourceUUID

	switch {
	case kind == ybuniverse.KindUniverse:
		r.Raw = s.UniverseState
		universe := ybuniverse.Status{UniverseState: s.UniverseState, Actions: s.Actions}
		if settled, why := ybuniverse.Settled(universe); settled {
			r.State = ResourceReady
		} else {
			// Error* is reported rather than adjudicated, for the same reason
			// FailureWatch does not end the wait on it: YBA requeues CREATE
			// forever, so a cause that resolves heals on its own.
			r.State = ResourcePending
			if r.Message == "" {
				r.Message = why
			}
		}
	case s.State != "":
		r.Raw = s.State
		switch {
		// InUse is a registration a universe holds, which is as usable as
		// Ready; Editing is a change in flight on something still referenceable.
		case strings.EqualFold(s.State, "Ready"), strings.EqualFold(s.State, "InUse"):
			r.State = ResourceReady
		case strings.EqualFold(s.State, "Error"), strings.EqualFold(s.State, "Failed"):
			r.State = ResourceFailed
		default:
			r.State = ResourcePending
		}
	case s.Success != nil:
		if *s.Success {
			r.State = ResourceReady
		} else {
			r.State = ResourceFailed
		}
	default:
		// Present, with nothing in its status that says whether it is done —
		// the action and schedule kinds are like this. Saying Pending for ever
		// would be a lie; saying Ready would be a guess.
		r.State = ResourcePending
		if r.Message == "" {
			r.Message = "the CR exists and its status says nothing about readiness"
		}
	}
	return r
}
