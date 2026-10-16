package api

import (
	"encoding/json"
	"fmt"

	"github.com/yugabyte/amp/controller/internal/core"
	"github.com/yugabyte/amp/controller/internal/deploy"
)

// Caps on the scheduling lists (the request schemas' maxItems).
const (
	maxTolerations               = 32
	maxTopologySpreadConstraints = 16
)

// endpointScheduling validates the scheduling lists of a create or update and
// returns them as the canonical JSON the rest of the controller carries. The
// rules are the API server's own (ValidateTolerations,
// validateTopologySpreadConstraints): a list it would refuse otherwise fails
// the endpoint's deploy long after the 202.
//
// A nil list stays nil. An empty one becomes `[]`, which an update reads as
// "clear"; a create passes it on and stores nothing.
//
// A non-empty list is refused unless dep renders it (only the Kubernetes
// deployer does), so it is never persisted and reported for a pod that lacks it.
func endpointScheduling(
	tols *[]Toleration, tscs *[]TopologySpreadConstraint, dep deploy.Deployer,
) (core.Scheduling, *Error) {
	var out core.Scheduling
	if (tols != nil && len(*tols) > 0) || (tscs != nil && len(*tscs) > 0) {
		if ss, ok := dep.(deploy.SchedulingSupporter); !ok || !ss.SupportsScheduling() {
			e := apiError("unsupported_scheduling",
				"tolerations and topology_spread_constraints need the kubernetes deploy driver")
			return out, &e
		}
	}
	if tols != nil {
		if len(*tols) > maxTolerations {
			return out, invalidScheduling("tolerations: at most %d are allowed, got %d", maxTolerations, len(*tols))
		}
		for i, t := range *tols {
			if err := validateToleration(t); err != nil {
				return out, invalidScheduling("tolerations[%d]: %v", i, err)
			}
		}
		out.Tolerations = mustCanonical(*tols)
	}
	if tscs != nil {
		if len(*tscs) > maxTopologySpreadConstraints {
			return out, invalidScheduling("topology_spread_constraints: at most %d are allowed, got %d",
				maxTopologySpreadConstraints, len(*tscs))
		}
		type pair struct{ key, when string }
		seen := map[pair]bool{}
		for i, c := range *tscs {
			if err := validateTopologySpreadConstraint(c); err != nil {
				return out, invalidScheduling("topology_spread_constraints[%d]: %v", i, err)
			}
			p := pair{c.TopologyKey, string(c.WhenUnsatisfiable)}
			if seen[p] {
				return out, invalidScheduling("topology_spread_constraints[%d]: duplicate topologyKey %q with whenUnsatisfiable %q",
					i, c.TopologyKey, c.WhenUnsatisfiable)
			}
			seen[p] = true
		}
		out.TopologySpreadConstraints = mustCanonical(*tscs)
	}
	return out, nil
}

func validateToleration(t Toleration) error {
	key := derefString(t.Key)
	if key != "" {
		if err := validateQualifiedName(key); err != nil {
			return fmt.Errorf("key %q: %w", key, err)
		}
	}
	op := TolerationOperatorEqual
	if t.Operator != nil {
		op = *t.Operator
	}
	switch op {
	case TolerationOperatorEqual:
		if key == "" {
			return fmt.Errorf("an empty key needs operator Exists")
		}
		if err := validateLabelValue(derefString(t.Value)); err != nil {
			return err
		}
	case TolerationOperatorExists:
		if derefString(t.Value) != "" {
			return fmt.Errorf("value must be empty with operator Exists")
		}
	default:
		return fmt.Errorf("operator %q must be Equal or Exists", op)
	}
	if t.Effect != nil {
		switch *t.Effect {
		case "", NoSchedule, PreferNoSchedule, NoExecute:
		default:
			return fmt.Errorf("effect %q must be NoSchedule, PreferNoSchedule or NoExecute", *t.Effect)
		}
	}
	if t.TolerationSeconds != nil && (t.Effect == nil || *t.Effect != NoExecute) {
		return fmt.Errorf("tolerationSeconds applies only with effect NoExecute")
	}
	return nil
}

func validateTopologySpreadConstraint(c TopologySpreadConstraint) error {
	if c.MaxSkew < 1 {
		return fmt.Errorf("maxSkew must be at least 1")
	}
	if c.TopologyKey == "" {
		return fmt.Errorf("topologyKey is required")
	}
	if err := validateQualifiedName(c.TopologyKey); err != nil {
		return fmt.Errorf("topologyKey %q: %w", c.TopologyKey, err)
	}
	switch c.WhenUnsatisfiable {
	case DoNotSchedule, ScheduleAnyway:
	default:
		return fmt.Errorf("whenUnsatisfiable %q must be DoNotSchedule or ScheduleAnyway", c.WhenUnsatisfiable)
	}
	if c.MinDomains != nil {
		if *c.MinDomains < 1 {
			return fmt.Errorf("minDomains must be at least 1")
		}
		if c.WhenUnsatisfiable != DoNotSchedule {
			return fmt.Errorf("minDomains applies only with whenUnsatisfiable DoNotSchedule")
		}
	}
	for name, p := range map[string]*string{
		"nodeAffinityPolicy": (*string)(c.NodeAffinityPolicy),
		"nodeTaintsPolicy":   (*string)(c.NodeTaintsPolicy),
	} {
		if p != nil && *p != "Honor" && *p != "Ignore" {
			return fmt.Errorf("%s %q must be Honor or Ignore", name, *p)
		}
	}
	if err := validateLabelSelector(c.LabelSelector); err != nil {
		return fmt.Errorf("labelSelector: %w", err)
	}
	if c.MatchLabelKeys != nil {
		for _, k := range *c.MatchLabelKeys {
			if err := validateQualifiedName(k); err != nil {
				return fmt.Errorf("matchLabelKeys %q: %w", k, err)
			}
			// The API server refuses a key the selector already names.
			if selectorHasKey(c.LabelSelector, k) {
				return fmt.Errorf("matchLabelKeys %q: the key is already in labelSelector", k)
			}
		}
	}
	return nil
}

// validateLabelSelector also refuses an empty selector, which Kubernetes
// accepts: it matches no pods, and each endpoint is a single-pod Deployment, so
// the constraint would do nothing.
func validateLabelSelector(s LabelSelector) error {
	if (s.MatchLabels == nil || len(*s.MatchLabels) == 0) && (s.MatchExpressions == nil || len(*s.MatchExpressions) == 0) {
		return fmt.Errorf("matchLabels or matchExpressions is required; endpoint pods carry amp.yugabyte.com/cluster-id, branch-id and the endpoint's tags")
	}
	if s.MatchLabels != nil {
		for k, v := range *s.MatchLabels {
			if err := validateQualifiedName(k); err != nil {
				return fmt.Errorf("matchLabels key %q: %w", k, err)
			}
			if err := validateLabelValue(v); err != nil {
				return fmt.Errorf("matchLabels[%q]: %w", k, err)
			}
		}
	}
	if s.MatchExpressions != nil {
		for i, r := range *s.MatchExpressions {
			if err := validateQualifiedName(r.Key); err != nil {
				return fmt.Errorf("matchExpressions[%d] key %q: %w", i, r.Key, err)
			}
			n := 0
			if r.Values != nil {
				n = len(*r.Values)
			}
			switch r.Operator {
			case LabelSelectorRequirementOperatorIn, LabelSelectorRequirementOperatorNotIn:
				if n == 0 {
					return fmt.Errorf("matchExpressions[%d]: operator %s needs values", i, r.Operator)
				}
			case LabelSelectorRequirementOperatorExists, LabelSelectorRequirementOperatorDoesNotExist:
				if n > 0 {
					return fmt.Errorf("matchExpressions[%d]: operator %s takes no values", i, r.Operator)
				}
			default:
				return fmt.Errorf("matchExpressions[%d]: operator %q must be In, NotIn, Exists or DoesNotExist", i, r.Operator)
			}
			if r.Values != nil {
				for _, v := range *r.Values {
					if err := validateLabelValue(v); err != nil {
						return fmt.Errorf("matchExpressions[%d]: %w", i, err)
					}
				}
			}
		}
	}
	return nil
}

func selectorHasKey(s LabelSelector, key string) bool {
	if s.MatchLabels != nil {
		if _, ok := (*s.MatchLabels)[key]; ok {
			return true
		}
	}
	if s.MatchExpressions != nil {
		for _, r := range *s.MatchExpressions {
			if r.Key == key {
				return true
			}
		}
	}
	return false
}

// schedulingToAPI renders the stored lists for a response. Only a hand edit can
// store one that does not decode; it is reported as absent.
func schedulingToAPI(sch core.Scheduling) (*[]Toleration, *[]TopologySpreadConstraint) {
	var tols *[]Toleration
	var tscs *[]TopologySpreadConstraint
	if len(sch.Tolerations) > 0 {
		var t []Toleration
		if json.Unmarshal(sch.Tolerations, &t) == nil && len(t) > 0 {
			tols = &t
		}
	}
	if len(sch.TopologySpreadConstraints) > 0 {
		var c []TopologySpreadConstraint
		if json.Unmarshal(sch.TopologySpreadConstraints, &c) == nil && len(c) > 0 {
			tscs = &c
		}
	}
	return tols, tscs
}

func mustCanonical(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		// The generated request types are plain structs, maps and slices.
		panic(fmt.Sprintf("marshal scheduling: %v", err))
	}
	return b
}

func invalidScheduling(format string, args ...any) *Error {
	e := apiError("invalid_request", fmt.Sprintf(format, args...))
	return &e
}
