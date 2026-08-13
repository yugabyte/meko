package ampstack

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/yugabyte/amp/test/functional/framework/apps/controller"
	"github.com/yugabyte/amp/test/functional/framework/exec/k8s"
	"github.com/yugabyte/amp/test/functional/framework/kubemeta"
)

// The HTTP relay: a minimal pod in the stack's namespace through which the
// coordinator carries controller HTTP over `kubectl exec` + curl. A kubectl
// port-forward is a long-lived local process that dies silently and takes
// every client pinned to its socket with it — it has repeatedly cost runs
// their results while the controller itself was healthy. Each exec, by
// contrast, is one self-contained API-server round trip with no local state,
// the same channel every other kubectl command already rides.
const (
	relayPodName   = "ampqa-http-relay"
	relayContainer = "relay"
	relayWait      = 3 * time.Minute
)

// DefaultRelayImage carries curl and nothing else matters. The perf client
// pods already run this exact public image on every cluster the suites use,
// so it pulls without a secret and is usually node-cached.
const DefaultRelayImage = "eclipse-temurin:21-jdk"

func relayManifest(namespace, image string) string {
	labels := kubemeta.TestLabels(map[string]string{ManagedByLabel: "ampstack"})
	var lb strings.Builder
	for k, v := range labels {
		fmt.Fprintf(&lb, "    %s: %q\n", k, v)
	}
	return fmt.Sprintf(`apiVersion: v1
kind: Pod
metadata:
  name: %s
  namespace: %s
  labels:
%s
spec:
  restartPolicy: Always
  containers:
    - name: %s
      image: %s
      command: ["sleep", "infinity"]
      resources:
        requests:
          cpu: 10m
          memory: 32Mi
        limits:
          cpu: 100m
          memory: 128Mi
`, relayPodName, namespace, strings.TrimRight(lb.String(), "\n"), relayContainer, image)
}

// ensureRelay stands the relay pod up (idempotent) and waits for it to be
// Ready. Also the recovery path when the pod is evicted mid-run: the pod is
// stateless, so recreation is always safe.
func (a *App) ensureRelay(ctx context.Context) error {
	image := a.cfg.RelayImage
	if image == "" {
		image = DefaultRelayImage
	}
	if err := a.kube.Apply(ctx, relayManifest(a.cfg.Namespace, image)); err != nil {
		return fmt.Errorf("apply HTTP relay pod: %w", err)
	}
	if err := a.kube.WaitFor(ctx, "pod/"+relayPodName, "condition=Ready", relayWait); err != nil {
		return fmt.Errorf("HTTP relay pod never became ready: %w", err)
	}
	return nil
}

// execControllerURL is the in-cluster address curl dials from the relay pod.
func (a *App) execControllerURL() string {
	return fmt.Sprintf("http://%s:%d", a.cfg.ControllerService, a.cfg.ControllerPort)
}

// execClient is a controller client whose HTTP rides the relay.
func (a *App) execClient() *controller.Client {
	tr := &k8s.HTTPExecTransport{
		Kube:      a.kube.ForPod(relayPodName, relayContainer),
		BaseURL:   a.execControllerURL(),
		EnsurePod: a.ensureRelay,
		Logf:      a.log,
	}
	return controller.New(a.execControllerURL(), controller.WithHTTPClient(&http.Client{Transport: tr}))
}
