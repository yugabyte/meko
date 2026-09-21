package ampbackend

import "context"

// noCtl is the controller config for a test that is not about the controller
// kind. Every Backend has one — the agent always runs inside a release it can
// converge — so there is no "without a controller" construction to express; a
// test that does not care just needs the reads not to panic.
func noCtl() ControllerConfig {
	return ControllerConfig{
		Helm:       &fakeHelm{},
		Rollout:    func(context.Context, string) (bool, string, error) { return false, "", nil },
		Release:    ctlRelease,
		Namespace:  ctlNamespace,
		Chart:      ctlChart,
		Deployment: "amp-controller",
	}
}
