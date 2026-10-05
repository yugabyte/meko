package core

import (
	"fmt"

	"github.com/yugabyte/meko/meko/test/functional/framework/universe"
)

// Sharing is how an environment may be shared across tests.
type Sharing int

const (
	Shared     Sharing = iota // may co-tenant with other Shared tests
	Exclusive                 // runs alone on its own environment
	Persistent                // pinned, detached, health-checked on a schedule
)

func (s Sharing) String() string {
	switch s {
	case Shared:
		return "shared"
	case Exclusive:
		return "exclusive"
	case Persistent:
		return "persistent"
	default:
		return "unknown"
	}
}

// Shape is the kind of stack an environment provides.
type Shape int

const (
	SingleStack    Shape = iota // the run's shared meko stack
	DedicatedStack              // a whole stack per test
	Remote                      // attach to an existing deployment by URL
)

func (s Shape) String() string {
	switch s {
	case SingleStack:
		return "single_stack"
	case DedicatedStack:
		return "dedicated_stack"
	case Remote:
		return "remote"
	default:
		return "unknown"
	}
}

// UnsupportedShapeError is a provision refusal that is a capability gap, not a
// failure: the run's provider cannot serve the test's declared shape. The
// lifecycle maps it to a skip, so a test can never silently run, and pass,
// against a different shape.
type UnsupportedShapeError struct {
	Provider string
	Shape    Shape
	Why      string
}

func (e *UnsupportedShapeError) Error() string {
	return fmt.Sprintf("env provider %q cannot serve a %s-shaped test: %s",
		e.Provider, e.Shape, e.Why)
}

// EnvSpec declares the environment a test needs and how it may be shared.
type EnvSpec struct {
	Sharing Sharing
	Shape   Shape
	Weight  int // lease units; 0 → shape default
	// ClusterSlots is how many self-provisioned stacks the test stands up on the
	// run's k8s cluster. The scheduler holds that many cluster-wide slots, shared
	// with every other run on the cluster, before the body starts. 0 takes none.
	ClusterSlots int
	// FaultsSharedInfra marks a test that faults the shared stack or the
	// YugabyteDB under it. The scheduler runs such a test alone where the
	// provider is a SharedInfraProvider.
	FaultsSharedInfra bool
	// SharedLock names a stack-global resource this test takes for its whole
	// body. Tests sharing a key never run concurrently on a shared stack; a
	// dedicated stack has its own, so the key is ignored there.
	SharedLock string
	// Database is the database under a DedicatedStack, instead of the one the
	// run configuration (db.*) describes. A shared stack serves every test off
	// one database and ignores it.
	Database *universe.Spec
	// BackingConfig, when set, is a test's own backing shape: config overrides
	// (the -o key=value form, typed prefixes allowed) laid over the run config
	// before the deploy selector reads it. Only the provider standing up a
	// DedicatedStack reads it, and so does the provision-budget resolver, so it
	// must be pure. A shared stack serves every test off one backing and ignores
	// it.
	BackingConfig func(cfg *Config) map[string]string
}

// Dedicated reports a test that owns its whole stack, so nothing it does can
// reach another test and the shared-infra gate does not apply to it.
func (s EnvSpec) Dedicated() bool { return s.Shape == DedicatedStack }

// FaultsShared is FaultsSharedInfra as it matters to the scheduler: a test that
// faults infrastructure only it uses faults nothing shared.
func (s EnvSpec) FaultsShared() bool { return s.FaultsSharedInfra && !s.Dedicated() }

// LockProvisionerCRD serializes tests that touch the cluster-scoped provisioner
// CRD of the hosted topology.
const LockProvisionerCRD = "provisioner_crd"

// Locks is SharedLock as it matters to the scheduler.
func (s EnvSpec) Locks() string {
	if s.Dedicated() {
		return ""
	}
	return s.SharedLock
}

// StackOverrideKey is the run-level knob that moves every stack test between
// the two stack shapes: "dedicated" gives each test a stack of its own, "shared"
// packs them onto the run's. Remote tests are untouched.
const StackOverrideKey = "env.stack"

// ApplyStackOverride rewrites the tests' shapes per env.stack. cfg may be nil.
func ApplyStackOverride(tests []Test, cfg *Config) error {
	if cfg == nil {
		return nil
	}
	var want Shape
	switch v := cfg.GetString(StackOverrideKey, ""); v {
	case "":
		return nil
	case "shared":
		want = SingleStack
	case "dedicated":
		want = DedicatedStack
	default:
		return fmt.Errorf("%s=%q: want shared or dedicated", StackOverrideKey, v)
	}
	for i := range tests {
		e := &tests[i].Env
		if e.Shape != SingleStack && e.Shape != DedicatedStack || e.Shape == want {
			continue
		}
		// A dedicated test packed with co-tenants would share the stack it
		// was given, and a test moved onto the shared stack may break it,
		// so the gate has to keep the other tests off it meanwhile.
		if want == DedicatedStack && e.Sharing == Shared {
			e.Sharing = Exclusive
		}
		if want == SingleStack {
			e.FaultsSharedInfra = true
		}
		e.Shape = want
	}
	return nil
}
