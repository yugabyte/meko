package api

import (
	"strings"
	"testing"

	"github.com/yugabyte/amp/controller/internal/deploy"
)

type schedOK struct{ deploy.Deployer }

func (schedOK) SupportsScheduling() bool { return true }

func TestEndpointSchedulingValidation(t *testing.T) {
	eq, exists := TolerationOperatorEqual, TolerationOperatorExists
	noExec, noSched := NoExecute, NoSchedule
	secs := int64(30)
	str := func(s string) *string { return &s }
	sel := LabelSelector{MatchLabels: &map[string]string{"amp.yugabyte.com/cluster-id": "c1"}}
	spread := func(mut func(*TopologySpreadConstraint)) TopologySpreadConstraint {
		c := TopologySpreadConstraint{MaxSkew: 1, TopologyKey: "topology.kubernetes.io/zone",
			WhenUnsatisfiable: DoNotSchedule, LabelSelector: sel}
		if mut != nil {
			mut(&c)
		}
		return c
	}
	two := int32(2)
	in, bad := LabelSelectorRequirementOperatorIn, LabelSelectorRequirementOperator("Matches")

	for name, tc := range map[string]struct {
		tols []Toleration
		tscs []TopologySpreadConstraint
		want string // "" = accepted
	}{
		"equal":                    {tols: []Toleration{{Key: str("dedicated"), Operator: &eq, Value: str("amp"), Effect: &noSched}}},
		"operator defaults equal":  {tols: []Toleration{{Key: str("dedicated"), Value: str("amp")}}},
		"exists any key":           {tols: []Toleration{{Operator: &exists}}},
		"no-execute seconds":       {tols: []Toleration{{Key: str("k"), Operator: &exists, Effect: &noExec, TolerationSeconds: &secs}}},
		"empty key needs exists":   {tols: []Toleration{{Operator: &eq}}, want: "empty key needs operator Exists"},
		"exists with value":        {tols: []Toleration{{Key: str("k"), Operator: &exists, Value: str("v")}}, want: "value must be empty"},
		"seconds without noexec":   {tols: []Toleration{{Key: str("k"), Operator: &exists, Effect: &noSched, TolerationSeconds: &secs}}, want: "tolerationSeconds"},
		"bad key":                  {tols: []Toleration{{Key: str("Bad_/x"), Operator: &exists}}, want: "key"},
		"empty effect":             {tols: []Toleration{{Key: str("k"), Operator: &exists, Effect: (*TolerationEffect)(str(""))}}},
		"bad effect":               {tols: []Toleration{{Key: str("k"), Operator: &exists, Effect: (*TolerationEffect)(str("Never"))}}, want: "effect"},
		"bad value":                {tols: []Toleration{{Key: str("k"), Value: str("not a label value")}}, want: "value"},
		"spread":                   {tscs: []TopologySpreadConstraint{spread(nil)}},
		"spread min domains":       {tscs: []TopologySpreadConstraint{spread(func(c *TopologySpreadConstraint) { c.MinDomains = &two })}},
		"zero skew":                {tscs: []TopologySpreadConstraint{spread(func(c *TopologySpreadConstraint) { c.MaxSkew = 0 })}, want: "maxSkew"},
		"no topology key":          {tscs: []TopologySpreadConstraint{spread(func(c *TopologySpreadConstraint) { c.TopologyKey = "" })}, want: "topologyKey"},
		"bad when":                 {tscs: []TopologySpreadConstraint{spread(func(c *TopologySpreadConstraint) { c.WhenUnsatisfiable = "Sometimes" })}, want: "whenUnsatisfiable"},
		"min domains schedule any": {tscs: []TopologySpreadConstraint{spread(func(c *TopologySpreadConstraint) { c.MinDomains = &two; c.WhenUnsatisfiable = ScheduleAnyway })}, want: "minDomains"},
		"empty selector":           {tscs: []TopologySpreadConstraint{spread(func(c *TopologySpreadConstraint) { c.LabelSelector = LabelSelector{} })}, want: "matchLabels or matchExpressions"},
		"in without values": {tscs: []TopologySpreadConstraint{spread(func(c *TopologySpreadConstraint) {
			c.LabelSelector = LabelSelector{MatchExpressions: &[]LabelSelectorRequirement{{Key: "k", Operator: in}}}
		})}, want: "needs values"},
		"bad expression operator": {tscs: []TopologySpreadConstraint{spread(func(c *TopologySpreadConstraint) {
			c.LabelSelector = LabelSelector{MatchExpressions: &[]LabelSelectorRequirement{{Key: "k", Operator: bad}}}
		})}, want: "operator"},
		"match label key in selector": {tscs: []TopologySpreadConstraint{spread(func(c *TopologySpreadConstraint) {
			c.MatchLabelKeys = &[]string{"amp.yugabyte.com/cluster-id"}
		})}, want: "already in labelSelector"},
		"match label key in expression": {tscs: []TopologySpreadConstraint{spread(func(c *TopologySpreadConstraint) {
			c.LabelSelector = LabelSelector{MatchExpressions: &[]LabelSelectorRequirement{{Key: "k", Operator: in, Values: &[]string{"v"}}}}
			c.MatchLabelKeys = &[]string{"k"}
		})}, want: "already in labelSelector"},
		"match label key elsewhere": {tscs: []TopologySpreadConstraint{spread(func(c *TopologySpreadConstraint) {
			c.MatchLabelKeys = &[]string{"pod-template-hash"}
		})}},
		"duplicate key and when": {tscs: []TopologySpreadConstraint{spread(nil), spread(nil)}, want: "duplicate"},
		"too many tolerations":   {tols: make([]Toleration, maxTolerations+1), want: "at most"},
	} {
		t.Run(name, func(t *testing.T) {
			var tols *[]Toleration
			var tscs *[]TopologySpreadConstraint
			if tc.tols != nil {
				tols = &tc.tols
			}
			if tc.tscs != nil {
				tscs = &tc.tscs
			}
			_, apiErr := endpointScheduling(tols, tscs, schedOK{})
			switch {
			case tc.want == "" && apiErr != nil:
				t.Errorf("refused: %s", apiErr.Error.Message)
			case tc.want != "" && apiErr == nil:
				t.Errorf("accepted, want a refusal mentioning %q", tc.want)
			case tc.want != "" && !strings.Contains(apiErr.Error.Message, tc.want):
				t.Errorf("refusal %q does not mention %q", apiErr.Error.Message, tc.want)
			}
		})
	}
}

// Nil stays nil (an update leaves the list alone); empty is kept as `[]`, which
// is how an update says "clear".
func TestEndpointSchedulingNilVersusEmpty(t *testing.T) {
	got, apiErr := endpointScheduling(nil, &[]TopologySpreadConstraint{}, schedOK{})
	if apiErr != nil {
		t.Fatal(apiErr.Error.Message)
	}
	if got.Tolerations != nil {
		t.Errorf("tolerations = %s, want nil", got.Tolerations)
	}
	if string(got.TopologySpreadConstraints) != "[]" {
		t.Errorf("spread = %s, want []", got.TopologySpreadConstraints)
	}
}

// A deployer that cannot render the lists refuses them, but still lets `[]`
// through: clearing nothing is not a claim about the pod.
func TestEndpointSchedulingNeedsSchedulingDeployer(t *testing.T) {
	exists := TolerationOperatorExists
	_, apiErr := endpointScheduling(&[]Toleration{{Operator: &exists}}, nil, nil)
	if apiErr == nil || apiErr.Error.Code != "unsupported_scheduling" {
		t.Fatalf("got %+v, want unsupported_scheduling", apiErr)
	}
	if _, apiErr := endpointScheduling(&[]Toleration{}, &[]TopologySpreadConstraint{}, nil); apiErr != nil {
		t.Fatalf("empty lists refused: %s", apiErr.Error.Message)
	}
}
