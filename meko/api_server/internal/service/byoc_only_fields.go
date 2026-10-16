package service

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"

	"meko/api_server/internal/models"
)

// byocOnlyEndpointFields are the amp endpoint-spec fields a MANAGED environment
// refuses. Both place the endpoint's pod on nodes, and on MANAGED the nodes are
// Yugabyte's: a toleration could put a customer's endpoint on a pool tainted for
// our own workloads. A cluster spec carries them too, for its with_writer
// endpoint.
var byocOnlyEndpointFields = []string{"tolerations", "topology_spread_constraints"}

// refuseByocOnlyFields is ErrValidation when an endpoint or cluster spec
// declares a byocOnlyEndpointFields field. A null one reads as undeclared.
func refuseByocOnlyFields(kind string, spec json.RawMessage) error {
	if (kind != "endpoint" && kind != "cluster") || len(bytes.TrimSpace(spec)) == 0 {
		return nil
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(spec, &fields) != nil {
		return nil // not an object: the caller's own shape check reports it
	}
	var found []string
	for _, f := range byocOnlyEndpointFields {
		if v, ok := fields[f]; ok && string(bytes.TrimSpace(v)) != "null" {
			found = append(found, f)
		}
	}
	if len(found) == 0 {
		return nil
	}
	return fmt.Errorf("%w: %s is available on BYOC environments only", ErrValidation, strings.Join(found, " and "))
}

// refuseByocOnlyResources applies refuseByocOnlyFields to a MANAGED
// environment's desired set; any other type takes them.
func refuseByocOnlyResources(typ string, res []models.DesiredResource) error {
	if typ != TypeManaged {
		return nil
	}
	for i, r := range res {
		if err := refuseByocOnlyFields(r.Kind, r.Spec); err != nil {
			return fmt.Errorf("resources[%d]: %w", i, err)
		}
	}
	return nil
}
