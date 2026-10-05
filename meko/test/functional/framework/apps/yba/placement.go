package yba

import (
	"context"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"
)

// Multi-region placement. A UniverseSpec with a Placement names the regions to
// spread over, how many nodes each takes, and which one tablet leaders prefer.
//
// It does NOT build a placementInfo body. YBA's own generator produces one from
// regionList/numNodes/RF during /universe_configure, and a hand-built one has to
// carry AZ UUIDs only YBA can resolve; getting a single field wrong is answered
// with "Unable to place replicas, not enough nodes in zones" naming nothing. So
// the create is the two-call flow and only what the generator cannot infer,
// leader preference, is patched onto the body it returns. defaultRegion is not
// that knob and is never set here; see applyPlacementPreference.

type ZonePlacement struct {
	Name   string // zone name/code (GCE: us-west1-b)
	UUID   string // provider zone UUID, filled by ResolvePlacement
	Subnet string // provider zone subnet, filled by ResolvePlacement when configured
	Nodes  int    // tservers placed in this zone
}

type RegionPlacement struct {
	Code  string // region code (GCE: us-west1)
	UUID  string // provider region UUID, filled by ResolvePlacement
	Zones []ZonePlacement
}

func placementNodeCount(p []RegionPlacement) int {
	n := 0
	for _, r := range p {
		for _, z := range r.Zones {
			n += z.Nodes
		}
	}
	return n
}

func (s UniverseSpec) PlacementRegionCodes() []string {
	out := make([]string, 0, len(s.Placement))
	for _, r := range s.Placement {
		out = append(out, r.Code)
	}
	return out
}

// ResolvePlacement fills the provider region and zone UUIDs (and subnets) into
// placement, matching on codes. A region or zone the provider does not carry is
// an error: a universe placed there would fail tens of minutes into the create.
func (c *Client) ResolvePlacement(ctx context.Context, providerUUID string, placement []RegionPlacement) ([]RegionPlacement, error) {
	regions, err := c.GetProviderRegions(ctx, providerUUID)
	if err != nil {
		return nil, fmt.Errorf("resolve placement: %w", err)
	}
	return resolvePlacementAgainst(providerUUID, regions, placement)
}

func resolvePlacementAgainst(providerUUID string, regions []Region, placement []RegionPlacement) ([]RegionPlacement, error) {
	byCode := make(map[string]Region, len(regions))
	for _, r := range regions {
		byCode[r.Code] = r
	}
	out := make([]RegionPlacement, 0, len(placement))
	for _, rp := range placement {
		r, ok := byCode[rp.Code]
		if !ok {
			return nil, fmt.Errorf("provider %s has no region %q", providerUUID, rp.Code)
		}
		rp.UUID = r.UUID
		zones := make([]ZonePlacement, 0, len(rp.Zones))
		for _, zp := range rp.Zones {
			var found *Zone
			for i := range r.Zones {
				if r.Zones[i].Code == zp.Name || r.Zones[i].Name == zp.Name {
					found = &r.Zones[i]
					break
				}
			}
			if found == nil {
				return nil, fmt.Errorf("provider %s region %q has no zone %q", providerUUID, rp.Code, zp.Name)
			}
			zp.UUID = found.UUID
			if zp.Subnet == "" {
				zp.Subnet = found.Subnet
			}
			zones = append(zones, zp)
		}
		rp.Zones = zones
		out = append(out, rp)
	}
	return out, nil
}

// applyPlacement shapes the userIntent for a multi-region create: the regions to
// spread over and the total node count. It deliberately does NOT hand-build
// placementInfo, YBA's own generator produces it from regionList/numNodes/RF
// during /universe_configure, and a hand-built one has to carry AZ UUIDs that
// only YBA can resolve. Getting one field of that wrong is answered with
// "Unable to place replicas, not enough nodes in zones" tens of seconds into a
// create, with nothing saying which field. Leader preference, which the
// generator cannot infer, is applied to the CONFIGURED body afterwards
// (applyPlacementPreference).
func applyPlacement(userIntent map[string]any, s UniverseSpec) {
	if len(s.Placement) == 0 {
		return
	}
	regionList := make([]any, 0, len(s.Placement))
	for _, r := range s.Placement {
		regionList = append(regionList, r.UUID)
	}
	userIntent["regionList"] = regionList
	userIntent["numNodes"] = placementNodeCount(s.Placement)
}

// applyPlacementPreference patches the body /universe_configure returned: the
// per-zone node counts the caller asked for, and the leader preference.
//
// isAffinitized is how YBA expresses "put tablet leaders here" at create time;
// its generator marks every zone, so the non-preferred ones have to be cleared.
// Matching is by region CODE and zone NAME, both of which the configured body
// carries, so no UUID of ours has to line up with YBA's.
//
// defaultRegion is deliberately NOT set. It reads like the same intent and is
// not: it is the geo-partitioning knob, and YBA puts EVERY master in it, so a
// default region holding one node of an RF3 universe is rejected with "Could
// not pick 3 masters, only 1 nodes available in default region". Leader
// preference is affinity alone.
func applyPlacementPreference(configured map[string]any, s UniverseSpec) {
	if len(s.Placement) == 0 {
		return
	}
	want := make(map[string]map[string]int, len(s.Placement))
	for _, r := range s.Placement {
		zones := make(map[string]int, len(r.Zones))
		for _, z := range r.Zones {
			zones[z.Name] = z.Nodes
		}
		want[r.Code] = zones
	}
	for _, cl := range mapSlice(configured["clusters"]) {
		if t, _ := cl["clusterType"].(string); t != "" && t != "PRIMARY" {
			continue
		}
		pi, ok := cl["placementInfo"].(map[string]any)
		if !ok {
			continue
		}
		for _, cloud := range mapSlice(pi["cloudList"]) {
			for _, region := range mapSlice(cloud["regionList"]) {
				code, _ := region["code"].(string)
				preferred := s.PreferredRegion == "" || code == s.PreferredRegion
				for _, az := range mapSlice(region["azList"]) {
					az["isAffinitized"] = preferred
					name, _ := az["name"].(string)
					if n, ok := want[code][name]; ok && n > 0 {
						az["numNodesInAZ"] = n
					}
				}
			}
		}
	}
}

// mapSlice reads a JSON array of objects out of an any, tolerating both the
// []any a decoded body carries and a []map[string]any built in a test.
func mapSlice(v any) []map[string]any {
	switch t := v.(type) {
	case []map[string]any:
		return t
	case []any:
		out := make([]map[string]any, 0, len(t))
		for _, e := range t {
			if m, ok := e.(map[string]any); ok {
				out = append(out, m)
			}
		}
		return out
	}
	return nil
}

// readPlacement reads a placement list ({region, zones: [{name, nodes[, rf]}]})
// from config; nil when the key is absent or empty.
func readPlacement(c ConfigGetter, key string) []RegionPlacement {
	raw := c.GetMapSlice(key, nil)
	if len(raw) == 0 {
		return nil
	}
	out := make([]RegionPlacement, 0, len(raw))
	for _, rm := range raw {
		r := RegionPlacement{Code: mapStr(rm, "region")}
		if r.Code == "" {
			r.Code = mapStr(rm, "code")
		}
		for _, zm := range subMaps(rm["zones"]) {
			r.Zones = append(r.Zones, ZonePlacement{
				Name:   mapStr(zm, "name"),
				Subnet: mapStr(zm, "subnet"),
				Nodes:  mapInt(zm, "nodes", 1),
			})
		}
		out = append(out, r)
	}
	return out
}

func mapInt(m map[string]any, key string, def int) int {
	switch v := m[key].(type) {
	case int:
		return v
	case int64:
		return int(v)
	case float64:
		return int(v)
	case string:
		var n int
		if _, err := fmt.Sscanf(v, "%d", &n); err == nil {
			return n
		}
	}
	return def
}

// NodeAction is a per-node operation YBA runs as a task.
type NodeAction string

const (
	NodeStop  NodeAction = "STOP"
	NodeStart NodeAction = "START"
)

// NodeAction plans a node action (STOP/START) on one universe node and returns
// its task reference. A stopped node stays a universe member with its processes
// down, which is what a region outage looks like to the rest of the universe.
func (c *Client) NodeAction(ctx context.Context, universeID, nodeName string, action NodeAction) (TaskRef, error) {
	if universeID == "" || nodeName == "" {
		return TaskRef{}, fmt.Errorf("node action %s: universe and node name are required", action)
	}
	var out TaskRef
	url := c.custURL("/universes/" + universeID + "/nodes/" + nodeName)
	if _, err := c.doJSON(ctx, http.MethodPut, url, map[string]any{"nodeAction": string(action)}, &out); err != nil {
		return TaskRef{}, fmt.Errorf("node %s %s: %w", nodeName, action, err)
	}
	c.log("yba: node %s %s (task %s)", nodeName, action, out.TaskUUID)
	return out, nil
}

func (c *Client) NodeActionAndWait(ctx context.Context, universeID, nodeName string, action NodeAction) error {
	ref, err := c.NodeAction(ctx, universeID, nodeName, action)
	if err != nil {
		return err
	}
	if _, err := c.WaitTask(ctx, ref.TaskUUID); err != nil {
		return fmt.Errorf("node %s %s: %w", nodeName, action, err)
	}
	return nil
}

// NodesActionAndWait runs one action on every named node, one task at a time
// (YBA serialises universe tasks anyway), then waits for the universe to settle.
func (c *Client) NodesActionAndWait(ctx context.Context, universeID string, nodes []string, action NodeAction, settle time.Duration) error {
	for _, n := range nodes {
		if err := c.NodeActionAndWait(ctx, universeID, n, action); err != nil {
			return err
		}
	}
	return c.WaitUniverseReady(ctx, universeID, settle)
}

// RegionNodes maps region code → tserver node names, from a universe's node set.
func (u Universe) RegionNodes() map[string][]string {
	out := map[string][]string{}
	for _, n := range u.UniverseDetails.NodeDetailsSet {
		if !n.IsTserver {
			continue
		}
		out[n.CloudInfo.Region] = append(out[n.CloudInfo.Region], n.NodeName)
	}
	for _, names := range out {
		sort.Strings(names)
	}
	return out
}

func (u Universe) Regions() []string {
	rn := u.RegionNodes()
	out := make([]string, 0, len(rn))
	for r := range rn {
		out = append(out, r)
	}
	sort.Strings(out)
	return out
}

func (u Universe) DescribeTopology() string {
	rn := u.RegionNodes()
	parts := make([]string, 0, len(rn))
	for _, r := range u.Regions() {
		parts = append(parts, fmt.Sprintf("%s=%d", r, len(rn[r])))
	}
	return strings.Join(parts, ",")
}

// HasPlacement reports whether the universe carries a placement at all, which
// tells an empty AffinitizedRegions apart from a placement that affinitized
// nothing, the first is a universe that reported none, the second is a real
// absence of leader preference.
func (u Universe) HasPlacement() bool {
	for _, cl := range u.UniverseDetails.Clusters {
		for _, cloud := range cl.PlacementInfo.CloudList {
			if len(cloud.RegionList) > 0 {
				return true
			}
		}
	}
	return false
}

// AffinitizedRegions lists the region codes whose zones YBA marks as leader
// preferred (isAffinitized) on the primary cluster, sorted. Every region when
// no preference was set; empty when the universe carries no placement, which
// HasPlacement distinguishes from a placement that preferred nothing.
func (u Universe) AffinitizedRegions() []string {
	seen := map[string]bool{}
	for _, cl := range u.UniverseDetails.Clusters {
		for _, cloud := range cl.PlacementInfo.CloudList {
			for _, r := range cloud.RegionList {
				for _, az := range r.AZList {
					if az.IsAffinitized {
						seen[r.Code] = true
					}
				}
			}
		}
	}
	out := make([]string, 0, len(seen))
	for r := range seen {
		out = append(out, r)
	}
	sort.Strings(out)
	return out
}
