package core

import (
	"fmt"
	"regexp"
	"sort"
	"sync"
	"time"
)

// Category is the test tier.
type Category int

const (
	Functional Category = iota
	Stress
	Perf
	LongRunning
)

func (c Category) String() string {
	switch c {
	case Functional:
		return "functional"
	case Stress:
		return "stress"
	case Perf:
		return "perf"
	case LongRunning:
		return "long_running"
	default:
		return "unknown"
	}
}

// Test is the atomic unit. Registered in an init() via core.Register.
type Test struct {
	Name     string
	Suite    string
	Category Category
	Env      EnvSpec
	Tags     []string
	// OwningTeam routes triage and regression notifications; one of the Team*
	// constants, or empty for a non-product test (e.g. canary, presets).
	OwningTeam string
	Versions   VersionSpec
	Timeout    time.Duration // 0 → category default
	// Description is a one-line human summary of what the test does, surfaced by
	// `ampqa describe`/`collect`/`manifest` so both humans and agents can discover
	// a test without reading its source.
	Description string
	// Params lists the config-key prefixes this test actually consumes (e.g.
	// "perf.pgbench", "chaos", "apps.ybk8s"). `ampqa describe <test>` resolves each
	// prefix to the matching flattened config keys (default + comment doc) as the
	// test-specific parameters. Empty for trivial tests that read only global
	// config. A prefix matches a leaf key exactly or any key under "<prefix>.".
	Params []string
	Run    func(t *T) error
	// Setup, when set, is the separable provision-and-prepare phase: it stands
	// the environment up, prepares fixtures, and returns a handle describing the
	// standing environment plus the teardown that reaps it. `ampqa setup` runs
	// ONLY this phase and leaves the environment up; the normal `run` lifecycle
	// runs Setup before Run (the body may consume the result via t.Standing) and
	// invokes the returned Teardown at the end, so nothing is left running.
	Setup func(t *T) (*SetupResult, error)
}

// Owning-team values, for OwningTeam and regression-notification routing.
const (
	TeamControlPlane = "control_plane"
	TeamCompute      = "compute"
	TeamVoyager      = "voyager"
	TeamPerfAdvisor  = "perf_advisor"
)

var validTeams = map[string]struct{}{
	TeamControlPlane: {},
	TeamCompute:      {},
	TeamVoyager:      {},
	TeamPerfAdvisor:  {},
}

// Registry is a name→Test map. The default registry backs package-level
// Register; unit tests construct their own with NewRegistry.
type Registry struct {
	mu    sync.Mutex
	tests map[string]Test
}

// NewRegistry returns an empty registry.
func NewRegistry() *Registry {
	return &Registry{tests: map[string]Test{}}
}

// Add registers a test. It errors on an empty name or a duplicate.
func (r *Registry) Add(t Test) error {
	if t.Name == "" {
		return fmt.Errorf("test has empty name")
	}
	if t.OwningTeam != "" {
		if _, ok := validTeams[t.OwningTeam]; !ok {
			return fmt.Errorf("test %q has invalid owning team %q", t.Name, t.OwningTeam)
		}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, dup := r.tests[t.Name]; dup {
		return fmt.Errorf("duplicate test name %q", t.Name)
	}
	r.tests[t.Name] = t
	return nil
}

// Get returns a test by name.
func (r *Registry) Get(name string) (Test, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	t, ok := r.tests[name]
	return t, ok
}

// List returns all tests sorted by name.
func (r *Registry) List() []Test {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]Test, 0, len(r.tests))
	for _, t := range r.tests {
		out = append(out, t)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Filter returns tests whose suite matches suiteRe, name matches testRe, and
// tags include every tag in tags. Empty regexes match all.
func (r *Registry) Filter(suiteRe, testRe string, tags []string) ([]Test, error) {
	sre, err := compileOrAll(suiteRe)
	if err != nil {
		return nil, fmt.Errorf("suite regex: %w", err)
	}
	tre, err := compileOrAll(testRe)
	if err != nil {
		return nil, fmt.Errorf("test regex: %w", err)
	}
	var out []Test
	for _, t := range r.List() {
		if !sre.MatchString(t.Suite) || !tre.MatchString(t.Name) {
			continue
		}
		if !hasAllTags(t.Tags, tags) {
			continue
		}
		out = append(out, t)
	}
	return out, nil
}

func compileOrAll(re string) (*regexp.Regexp, error) {
	if re == "" {
		return regexp.MustCompile(".*"), nil
	}
	return regexp.Compile(re)
}

func hasAllTags(have, want []string) bool {
	if len(want) == 0 {
		return true
	}
	set := make(map[string]struct{}, len(have))
	for _, t := range have {
		set[t] = struct{}{}
	}
	for _, w := range want {
		if _, ok := set[w]; !ok {
			return false
		}
	}
	return true
}

var defaultRegistry = NewRegistry()

// Register adds a test to the default registry, panicking on error (intended
// for init()).
func Register(t Test) {
	if err := defaultRegistry.Add(t); err != nil {
		panic(err)
	}
}

// Default returns the package-level registry.
func Default() *Registry { return defaultRegistry }
