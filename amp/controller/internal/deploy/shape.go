package deploy

import "fmt"

const (
	// DefaultEndpointCPU is the reservation an endpoint gets when a request names
	// no CPU.
	DefaultEndpointCPU = "2"
	// DefaultMemoryGiBPerVCPU is the memory-per-core shape an endpoint gets, and
	// must meet, when the installer has not said otherwise.
	//
	// Postgres on this compute is not a thin client of the pageserver — the LFC,
	// shared buffers and per-backend work memory all come out of the pod — so a
	// core without the memory to feed it produces an endpoint that starts,
	// serves, and then evicts its own cache under the first real query. That is
	// worse than a request refused, because it looks like a slow database rather
	// than a misconfigured one.
	DefaultMemoryGiBPerVCPU = 8
)

// gibCeil renders bytes as a whole-GiB Kubernetes quantity, rounding up so the
// result never falls below what was asked for.
func gibCeil(bytes float64) string {
	const gib = float64(1 << 30)
	g := int64((bytes + gib - 1) / gib)
	if g < 1 {
		g = 1
	}
	return fmt.Sprintf("%dGi", g)
}

// MeetsFloor reports whether a shape is at or above the memory-per-core floor.
// An unparseable pair reads as "does not meet it", which routes it to NoWorseThan
// rather than through the floor — and that refuses what it cannot compare.
func MeetsFloor(cpu, memory string, gibPerVCPU int) bool {
	_, _, errMsg := ResolveEndpointShape(cpu, memory, gibPerVCPU)
	return errMsg == ""
}

// ResolveEndpointShape fills in what a resources request left out and holds it to
// the memory-per-core floor, returning the resolved pair or a message for a 400.
//
// Memory follows CPU when it is not named: asking for 8 cores and getting the
// 2-core default's memory would be a shape the floor rejects, so the request
// would have to name both to name either.
//
// A floor, not a fixed ratio. More memory per core is a workload that needs it;
// less is an endpoint that starts and cannot hold its own cache, and refusing it
// here is the difference between a clear error and a database that looks slow.
//
// It lives in this package rather than beside the HTTP handler because the
// handler is not the only thing that sizes an endpoint: the one-call cluster
// create and the Voyager migration target build their own, and a rule enforced
// on one path is not a rule.
func ResolveEndpointShape(cpu, memory string, gibPerVCPU int) (string, string, string) {
	if gibPerVCPU <= 0 {
		gibPerVCPU = DefaultMemoryGiBPerVCPU
	}
	if cpu == "" {
		cpu = DefaultEndpointCPU
	}
	cores, err := ParseCPUQuantity(cpu)
	if err != nil || cores <= 0 {
		return "", "", `resources.cpu must be a positive Kubernetes CPU quantity (e.g. "2", "500m")`
	}
	const gib = float64(1 << 30)
	want := cores * float64(gibPerVCPU) * gib
	if memory == "" {
		return cpu, gibCeil(want), ""
	}
	got, err := ParseByteQuantity(memory)
	if err != nil || got <= 0 {
		return "", "", `resources.memory must be a positive Kubernetes memory quantity (e.g. "16Gi", "512Mi")`
	}
	// Relative tolerance, because the requirement is computed in floating point:
	// a cpu whose product is not exactly representable would otherwise be refused
	// by an ULP for a request that is precisely on the floor.
	if float64(got) < want*(1-1e-9) {
		return "", "", fmt.Sprintf(
			"resources.memory %s is below the %d GiB per vCPU this control plane requires: %s cpu needs at least %s",
			memory, gibPerVCPU, cpu, gibCeil(want))
	}
	return cpu, memory, ""
}

// NoWorseThan reports whether moving from one shape to another does not reduce
// the memory each core has, and a message for a 400 when it does.
//
// The rule for an endpoint that is already below the floor. Holding those to the
// floor outright would make them unresizable at all — including the shrink that
// moves them towards it — so what is refused is only a change that makes the
// starvation worse.
func NoWorseThan(fromCPU, fromMemory, toCPU, toMemory string) (bool, string) {
	from, err1 := gibPerCore(fromCPU, fromMemory)
	to, err2 := gibPerCore(toCPU, toMemory)
	if err1 != nil || err2 != nil {
		// No comparison is possible, so "not worse" cannot be established. A
		// dimension the request leaves out falls back to the endpoint's stored
		// value, and nothing validates that, so this is reachable with a row the
		// parser rejects — and returning true there would hand that value to the
		// deploy as the resolved shape.
		return false, fmt.Sprintf(
			"this endpoint's size cannot be read (%s cpu, %s), so a resize cannot be checked against the memory-per-vCPU floor: set both resources.cpu and resources.memory explicitly",
			fromCPU, fromMemory)
	}
	// A hair of tolerance, for the same reason the floor has one.
	if to < from*(1-1e-9) {
		return false, fmt.Sprintf(
			"this endpoint is below the memory-per-vCPU floor already (%.1f GiB per vCPU) and a resize may not lower it further: %s cpu with %s is %.1f GiB per vCPU",
			from, toCPU, toMemory, to)
	}
	return true, ""
}

func gibPerCore(cpu, memory string) (float64, error) {
	cores, err := ParseCPUQuantity(cpu)
	if err != nil || cores <= 0 {
		return 0, fmt.Errorf("cpu %q", cpu)
	}
	bytes, err := ParseByteQuantity(memory)
	if err != nil || bytes <= 0 {
		return 0, fmt.Errorf("memory %q", memory)
	}
	return float64(bytes) / float64(int64(1)<<30) / cores, nil
}
