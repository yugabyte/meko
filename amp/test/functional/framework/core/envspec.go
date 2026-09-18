package core

import "fmt"

// Sharing is how an environment may be shared across tests (architecture §4.2).
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
//
// This framework tests amp. Every shape except Universe puts an amp endpoint in
// the request path, and Universe exists only so a test can deploy amp ON that
// cluster (the amp_on_* E2Es) or drive amp's own workloads against it — never
// to test raw YugabyteDB itself, which other frameworks own. That is why the
// universe provider refuses every other shape instead of serving its raw YSQL.
type Shape int

const (
	SingleContainer    Shape = iota // whole amp data path in one container (the endpoint image)
	SharedControlPlane              // cluster-per-test on a shared control plane
	DedicatedStack                  // a whole dedicated stack (pod-kill access)
	Remote                          // attach to a pre-existing stack by URL
	Universe                        // a YB universe/cluster as the target (env/universe)
)

func (s Shape) String() string {
	switch s {
	case SingleContainer:
		return "single_container"
	case SharedControlPlane:
		return "shared_control_plane"
	case DedicatedStack:
		return "dedicated_stack"
	case Remote:
		return "remote"
	case Universe:
		return "universe"
	default:
		return "unknown"
	}
}

// UnsupportedShapeError is a provision refusal that is a capability gap, not a
// failure: the run's provider cannot serve the test's declared shape. The
// lifecycle maps it to a skip, so a test whose shape needs an amp endpoint can
// never silently run — and pass — against something else.
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
	// FaultsSharedInfra marks a test that faults the control plane or the
	// YugabyteDB under it. Sharing: Exclusive does not cover this — under
	// sharedk8s it buys a cluster, not a control plane. The scheduler runs such a
	// test alone where the provider is a SharedInfraProvider.
	FaultsSharedInfra bool
	// SharedLock names a control-plane-global resource this test takes for its
	// whole body. Tests sharing a key never run concurrently on a shared control
	// plane; a dedicated stack has its own, so the key is ignored there.
	SharedLock string
	// PreferredYBDeploy is a test's HINT for which YugabyteDB deployment it would
	// like (e.g. "yb_k8s"). It is opaque to the engine — carried, never
	// interpreted here — and read only by the deploy selector (apps/ybdeploy).
	// The run-level env.yb_deploy knob OVERRIDES it: the knob always wins.
	PreferredYBDeploy string
	// BackingConfig, when set, is a test's own backing shape: config overrides
	// (the -o key=value form, typed prefixes allowed) laid over the run config
	// before the deploy selector reads it. Like PreferredYBDeploy it is opaque
	// here and read only by the provider standing up a DedicatedStack (and by
	// the provision-budget resolver, so it must be pure), which
	// then gives that test a YugabyteDB of its own shape (a multi-region
	// universe, say) on a YBA of its own. A shared control plane serves every
	// test off one backing and ignores it.
	BackingConfig func(cfg *Config) map[string]string
}

// Dedicated reports a test that owns its whole stack, so nothing it does can
// reach another test and the shared-infra gate does not apply to it.
func (s EnvSpec) Dedicated() bool { return s.Shape == DedicatedStack }

// FaultsShared is FaultsSharedInfra as it matters to the scheduler: a test that
// faults infrastructure only it uses faults nothing shared.
func (s EnvSpec) FaultsShared() bool { return s.FaultsSharedInfra && !s.Dedicated() }

// LockEndpointFleetUpgrade: the controller admits one fleet endpoint upgrade at
// a time and 409s the rest, and a roll retargets endpoints another test owns.
const LockEndpointFleetUpgrade = "endpoint_fleet_upgrade"

// Locks is SharedLock as it matters to the scheduler.
func (s EnvSpec) Locks() string {
	if s.Dedicated() {
		return ""
	}
	return s.SharedLock
}

// StackOverrideKey is the run-level knob that moves every control-plane test
// between the two stack shapes: "dedicated" gives each test its own backing and
// control plane, "shared" packs them onto the run-scoped one. Tests of other
// shapes (SingleContainer, Universe) are untouched.
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
		want = SharedControlPlane
	case "dedicated":
		want = DedicatedStack
	default:
		return fmt.Errorf("%s=%q: want shared or dedicated", StackOverrideKey, v)
	}
	for i := range tests {
		switch tests[i].Env.Shape {
		case SharedControlPlane, DedicatedStack:
			tests[i].Env.Shape = want
		}
	}
	return nil
}
