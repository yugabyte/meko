package functional

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/yugabyte/meko/meko/test/functional/framework/core"
	"github.com/yugabyte/meko/meko/test/functional/framework/universe"
	"github.com/yugabyte/meko/meko/test/functional/suites/internal/stacktest"
)

func env() core.EnvSpec {
	return core.EnvSpec{Sharing: core.Shared, Shape: core.SingleStack}
}

// need returns u as capability T, or skips the test with why the provider cannot.
func need[T any](t *core.T, u universe.Universe) T {
	c, err := universe.Capability[T](u)
	if err != nil {
		t.Skip(err.Error())
	}
	return c
}

// finish skips the test when err says the provider cannot, outside any step.
func finish(t *core.T, err error) error {
	if universe.IsUnsupported(err) {
		t.Skip(err.Error())
	}
	return err
}

// step is t.Step, except a provider's "cannot" skips the test once the step is over.
func step(t *core.T, name string, fn func(*core.StepHandle) error) error {
	err := t.Step(name, fn)
	return finish(t, err)
}

func cleanup() (context.Context, context.CancelFunc) { return stacktest.CleanupCtx() }

// waitRow polls u until a query answers want.
func waitRow(t *core.T, u universe.Universe, node, database, query, want string, within time.Duration) error {
	var last string
	err := t.WaitFor(fmt.Sprintf("%s to answer %q on %s", query, want, u.Name()), within, 3*time.Second, func() (bool, error) {
		out, err := u.SQL(t, node, database, query)
		if err != nil {
			last = err.Error()
			return false, nil
		}
		lines := strings.Split(strings.TrimSpace(out), "\n")
		last = strings.TrimSpace(lines[len(lines)-1])
		return last == want, nil
	})
	if err != nil {
		return fmt.Errorf("%w (last answer %q)", err, last)
	}
	return nil
}

// pair creates a source and a target with the run's database settings but one
// node each, which is all a replication test needs. check runs on the source
// before the target is built, so a provider that cannot do the test skips early.
func pair(t *core.T, check func(src universe.Universe)) (src, dst universe.Universe, err error) {
	set := stacktest.Universes(t)
	one := func(name string) universe.Spec { return universe.Single(name, 1, 1) }
	if src, err = set.Create(t, one("src")); err != nil {
		return nil, nil, err
	}
	check(src)
	if dst, err = set.Create(t, one("dst")); err != nil {
		return nil, nil, err
	}
	return src, dst, nil
}
