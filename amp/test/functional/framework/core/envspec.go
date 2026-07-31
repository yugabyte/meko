package core

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
type Shape int

const (
	SingleContainer    Shape = iota // whole amp in one docker container
	SharedControlPlane              // project-per-test on a shared control plane
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

// EnvSpec declares the environment a test needs and how it may be shared.
type EnvSpec struct {
	Sharing Sharing
	Shape   Shape
	Weight  int // lease units; 0 → shape default
	// PreferredYBDeploy is a test's HINT for which YugabyteDB deployment it would
	// like (e.g. "yb_k8s"). It is opaque to the engine — carried, never
	// interpreted here — and read only by the deploy selector (apps/ybdeploy).
	// The run-level env.yb_deploy knob OVERRIDES it: the knob always wins.
	PreferredYBDeploy string
}
