package main

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strings"
)

// defaultControllerChart is where the agent pulls the controller chart from. It
// cannot be baked into this image the way the controller image bakes its endpoint
// chart: the docker build context is ycp/, which cannot see amp/deploy/helm. So
// the chart is resolved at runtime, and this default is a promise the registry
// does not keep yet — publishing the chart as OCI is separate work (byoc_upgrades
// §10). Until it is published, CONTROLLER_CHART points the agent at one it can
// actually pull — an https URL to a packaged .tgz, or a local directory — which
// is the only way to exercise this against a real cluster.
const defaultControllerChart = "oci://quay.io/yugabyte/charts/yb-amp-controller"

// defaultHelmBin is absolute because the final image is distroless: there is no
// shell, and nothing else guarantees a PATH for the child process.
const defaultHelmBin = "/usr/local/bin/helm"

// The release and Deployment the chart installs by default. The release name is
// discovered from helm's own annotations first (k8s.releaseName); this is the
// fallback for an install whose objects helm did not apply. The Deployment name
// IS fixed by the chart, so it is a plain default.
const (
	defaultControllerRelease    = "yb-amp-controller"
	defaultControllerDeployment = "amp-controller"
)

// helmRunner shells out to the helm CLI. In-cluster helm authenticates with the
// pod's ServiceAccount — it reads the token file itself on each invocation, so
// unlike the Go client here it is immune to rotation.
type helmRunner struct{ bin string }

func (h helmRunner) Run(ctx context.Context, stdin []byte, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, h.bin, args...)
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("helm %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return stdout.Bytes(), nil
}
