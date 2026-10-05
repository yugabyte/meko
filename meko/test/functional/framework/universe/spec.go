// Package universe describes a YugabyteDB universe independently of what runs
// it. A Spec says what to build, a Universe is what was built, and the optional
// capability interfaces are what a provider can do with one beyond serving SQL.
// It depends on nothing else in the framework, so core.EnvSpec can carry a Spec.
package universe

import (
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// Provider names what builds a universe.
type Provider string

const (
	// Yugabyted runs yugabyted in pods.
	Yugabyted Provider = "yugabyted"
	// YBA drives YugabyteDB Anywhere.
	YBA Provider = "yba"
	// Chart is the meko chart's own bundled single-node database, which only a
	// stack can use, and which exists to test the install a BYOC customer gets.
	Chart Provider = "chart"
)

// ClusterType is the role of a cluster inside a universe.
type ClusterType string

const (
	Primary     ClusterType = "primary"
	ReadReplica ClusterType = "read_replica"
)

// Spec is a universe to build. A zero Spec is not valid, WithDefaults fills in
// the rest of a partial one.
type Spec struct {
	Name     string
	Provider Provider
	// Image is a full image reference, Version a release tag. Image wins.
	Image    string
	Version  string
	Clusters []ClusterSpec

	YSQLAuth     bool
	YSQLPassword string
	TLS          TLSSpec

	MasterGflags  map[string]string
	TServerGflags map[string]string
}

// TLSSpec turns on encryption in transit.
type TLSSpec struct {
	Nodes   bool
	Clients bool
}

// ClusterSpec is one cluster of a universe: the primary, or a read replica.
type ClusterSpec struct {
	Type ClusterType
	// RF is the replication factor. Any positive value is expressible, YugabyteDB
	// decides which it accepts.
	RF int
	// Placement is the tserver count per zone. A zone is either a bare name
	// ("us-west1-b") or cloud.region.zone.
	Placement map[string]int
	// Masters is how many masters run and where. The zero value means RF masters
	// for a primary and none for a read replica, spread over Placement.
	Masters MasterSpec
	Node    NodeSpec

	MasterGflags  map[string]string
	TServerGflags map[string]string
}

// MasterSpec places masters.
type MasterSpec struct {
	Count     int
	Placement map[string]int
}

// NodeSpec is the shape of one node. Zero fields take the provider's default.
type NodeSpec struct {
	CPU          float64
	MemoryGiB    float64
	DiskGiB      int
	StorageClass string
	// InstanceType names a cloud instance type, for providers that take one.
	InstanceType string
}

var nameRe = regexp.MustCompile(`^[a-z]([a-z0-9-]{0,28}[a-z0-9])?$`)

// Defaults for a spec left partial.
const (
	DefaultRF       = 3
	DefaultZone     = "zone-a"
	DefaultCPU      = 0.5
	DefaultMemory   = 2.0
	DefaultDiskGiB  = 10
	maxNameLength   = 30
	MaxReplicaCount = 9
)

// Or fills the fields n leaves zero from d.
func (n NodeSpec) Or(d NodeSpec) NodeSpec {
	if n.CPU == 0 {
		n.CPU = d.CPU
	}
	if n.MemoryGiB == 0 {
		n.MemoryGiB = d.MemoryGiB
	}
	if n.DiskGiB == 0 {
		n.DiskGiB = d.DiskGiB
	}
	if n.StorageClass == "" {
		n.StorageClass = d.StorageClass
	}
	if n.InstanceType == "" {
		n.InstanceType = d.InstanceType
	}
	return n
}

// WithNode fills every cluster's unset node fields from n.
func (s Spec) WithNode(n NodeSpec) Spec {
	out := s
	out.Clusters = make([]ClusterSpec, len(s.Clusters))
	for i, c := range s.Clusters {
		c.Node = c.Node.Or(n)
		out.Clusters[i] = c
	}
	return out
}

// Single is a one-cluster spec with rf tservers in one zone.
func Single(name string, rf, nodes int) Spec {
	return Spec{Name: name, Clusters: []ClusterSpec{{Type: Primary, RF: rf, Placement: map[string]int{DefaultZone: nodes}}}}
}

// WithDefaults returns a copy with the omitted parts filled in: RF3, one zone,
// RF masters, and the default node shape.
func (s Spec) WithDefaults() Spec {
	out := s
	out.Clusters = make([]ClusterSpec, len(s.Clusters))
	for i, c := range s.Clusters {
		if c.Type == "" {
			if i == 0 {
				c.Type = Primary
			} else {
				c.Type = ReadReplica
			}
		}
		if c.RF == 0 {
			c.RF = DefaultRF
		}
		if len(c.Placement) == 0 {
			c.Placement = map[string]int{DefaultZone: c.RF}
		}
		if c.Masters.Count == 0 && len(c.Masters.Placement) == 0 && c.Type == Primary {
			c.Masters.Count = c.RF
		}
		if c.Node.CPU == 0 {
			c.Node.CPU = DefaultCPU
		}
		if c.Node.MemoryGiB == 0 {
			c.Node.MemoryGiB = DefaultMemory
		}
		if c.Node.DiskGiB == 0 {
			c.Node.DiskGiB = DefaultDiskGiB
		}
		out.Clusters[i] = c
	}
	return out
}

// Primary returns the primary cluster, or the zero value when there is none.
func (s Spec) Primary() ClusterSpec {
	for _, c := range s.Clusters {
		if c.Type == Primary {
			return c
		}
	}
	return ClusterSpec{}
}

// ReadReplicas are the read replica clusters.
func (s Spec) ReadReplicas() []ClusterSpec {
	var out []ClusterSpec
	for _, c := range s.Clusters {
		if c.Type == ReadReplica {
			out = append(out, c)
		}
	}
	return out
}

// Tservers is how many tservers the cluster runs.
func (c ClusterSpec) Tservers() int {
	n := 0
	for _, v := range c.Placement {
		n += v
	}
	return n
}

// Zones lists the zones the cluster places tservers in, sorted.
func (c ClusterSpec) Zones() []string {
	var zs []string
	for z, n := range c.Placement {
		if n > 0 {
			zs = append(zs, z)
		}
	}
	sort.Strings(zs)
	return zs
}

// MasterCount is how many masters the cluster runs.
func (c ClusterSpec) MasterCount() int {
	if c.Masters.Count > 0 {
		return c.Masters.Count
	}
	n := 0
	for _, v := range c.Masters.Placement {
		n += v
	}
	return n
}

// ZoneName is the zone part of a placement key, which may be cloud.region.zone.
func ZoneName(z string) string {
	if i := strings.LastIndexByte(z, '.'); i >= 0 {
		return z[i+1:]
	}
	return z
}

// Validate checks what no provider can build, and nothing YugabyteDB itself
// decides: which replication factors it supports is its own rule.
func (s Spec) Validate() error {
	var errs []error
	if !nameRe.MatchString(s.Name) {
		errs = append(errs, fmt.Errorf("universe name %q must be 1 to %d lowercase letters, digits and dashes, starting with a letter", s.Name, maxNameLength))
	}
	if len(s.Clusters) == 0 {
		return errors.Join(append(errs, errors.New("a universe needs a primary cluster"))...)
	}
	primaries := 0
	for i, c := range s.Clusters {
		where := fmt.Sprintf("cluster %d (%s)", i, c.Type)
		switch c.Type {
		case Primary:
			primaries++
			if i != 0 {
				errs = append(errs, fmt.Errorf("%s: the primary must be the first cluster", where))
			}
		case ReadReplica:
			if primaries == 0 {
				errs = append(errs, fmt.Errorf("%s: a read replica needs a primary before it", where))
			}
		default:
			errs = append(errs, fmt.Errorf("%s: unknown cluster type", where))
			continue
		}
		if c.RF < 1 || c.RF > MaxReplicaCount {
			errs = append(errs, fmt.Errorf("%s: replication factor %d is outside 1 to %d", where, c.RF, MaxReplicaCount))
		}
		nodes := c.Tservers()
		if nodes < 1 {
			errs = append(errs, fmt.Errorf("%s: no tservers placed", where))
		}
		for z, n := range c.Placement {
			if z == "" || n < 0 {
				errs = append(errs, fmt.Errorf("%s: placement %q: %d", where, z, n))
			}
		}
		if c.Type == Primary && c.RF > nodes {
			errs = append(errs, fmt.Errorf("%s: replication factor %d needs at least %d tservers, there are %d", where, c.RF, c.RF, nodes))
		}
		if c.Type == ReadReplica && c.Masters.Count+len(c.Masters.Placement) > 0 {
			errs = append(errs, fmt.Errorf("%s: a read replica runs no masters", where))
		}
		if c.Type == Primary {
			m := c.MasterCount()
			if m < c.RF {
				errs = append(errs, fmt.Errorf("%s: %d masters cannot hold replication factor %d", where, m, c.RF))
			}
			if m > nodes {
				errs = append(errs, fmt.Errorf("%s: %d masters on %d tservers", where, m, nodes))
			}
			if len(c.Masters.Placement) > 0 {
				sum := 0
				for z, n := range c.Masters.Placement {
					sum += n
					if c.Placement[z] < n {
						errs = append(errs, fmt.Errorf("%s: %d masters in zone %q, which has %d tservers", where, n, z, c.Placement[z]))
					}
				}
				if c.Masters.Count > 0 && sum != c.Masters.Count {
					errs = append(errs, fmt.Errorf("%s: master placement adds up to %d, count says %d", where, sum, c.Masters.Count))
				}
			}
		}
		if c.Node.CPU < 0 || c.Node.MemoryGiB < 0 || c.Node.DiskGiB < 0 {
			errs = append(errs, fmt.Errorf("%s: negative node shape %+v", where, c.Node))
		}
	}
	if primaries != 1 {
		errs = append(errs, fmt.Errorf("a universe has exactly one primary cluster, this one has %d", primaries))
	}
	if s.YSQLPassword != "" && !s.YSQLAuth {
		errs = append(errs, errors.New("a YSQL password without YSQLAuth"))
	}
	return errors.Join(errs...)
}
