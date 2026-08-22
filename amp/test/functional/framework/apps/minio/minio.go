// Package minio stands a single-node MinIO up in a kubernetes namespace to be
// an S3-compatible backup target for tests that need one without cloud
// credentials — a fresh YBA in the run's own cluster has nowhere else to write.
// The credentials are throwaway constants: the service is in-cluster only and
// dies with the run.
package minio

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/yugabyte/amp/test/functional/framework/exec/k8s"
	"github.com/yugabyte/amp/test/functional/framework/kubemeta"
)

const (
	// AccessKey / SecretKey are the fixed root credentials the server is
	// started with (in-cluster, throwaway).
	AccessKey = "ampqa-minio"
	SecretKey = "ampqa-minio-secret"

	// DefaultServerImage / DefaultClientImage run the server and the one-shot
	// bucket bootstrap.
	DefaultServerImage = "minio/minio:latest"
	DefaultClientImage = "minio/mc:latest"

	port      = 9000
	readyPoll = 3 * time.Second

	// bucketAttempts/bucketRetry bound the in-pod retry of the bucket
	// bootstrap. Their product must stay under Spec.ReadyTimeout, which is
	// what gives up on the pod as a whole.
	bucketAttempts = 60
	bucketRetry    = 2 * time.Second
)

// Spec configures one deployment.
type Spec struct {
	// Name prefixes the pod/service names (suffix it per-run for uniqueness).
	Name        string
	Bucket      string
	ServerImage string
	ClientImage string
	// ReadyTimeout bounds the server-pod and bucket-bootstrap waits.
	ReadyTimeout time.Duration
}

// Server is a running MinIO.
type Server struct {
	kube *k8s.Kubectl
	name string
}

// Endpoint is the in-cluster URL (http; MinIO here runs without TLS) that YBA
// and the DB nodes reach the server on.
func (s *Server) Endpoint() string {
	return fmt.Sprintf("http://%s.%s.svc.cluster.local:%d", s.name, s.kube.Namespace(), port)
}

// Delete tears the pod and service down (best-effort).
func (s *Server) Delete(ctx context.Context) {
	_ = s.kube.Delete(ctx, "pod", s.name)
	_ = s.kube.Delete(ctx, "pod", s.name+"-mb")
	_ = s.kube.Delete(ctx, "service", s.name)
}

// Deploy stands the server up in kube's namespace, waits for it to serve, and
// creates spec.Bucket through a one-shot mc pod (modern MinIO registers
// buckets in its own metadata, so a mkdir under /data is not enough).
func Deploy(ctx context.Context, kube *k8s.Kubectl, spec Spec) (*Server, error) {
	if spec.Name == "" || spec.Bucket == "" {
		return nil, fmt.Errorf("minio: name and bucket are required")
	}
	if spec.ServerImage == "" {
		spec.ServerImage = DefaultServerImage
	}
	if spec.ClientImage == "" {
		spec.ClientImage = DefaultClientImage
	}
	if spec.ReadyTimeout <= 0 {
		spec.ReadyTimeout = 5 * time.Minute
	}
	s := &Server{kube: kube, name: spec.Name}

	if err := kube.Apply(ctx, serverManifest(spec)); err != nil {
		return nil, fmt.Errorf("minio: apply server: %w", err)
	}
	if err := kube.WaitFor(ctx, "pod/"+spec.Name, "condition=Ready", spec.ReadyTimeout); err != nil {
		return nil, fmt.Errorf("minio: server pod never became ready: %w", err)
	}
	if err := makeBucket(ctx, kube, spec, s.Endpoint()); err != nil {
		return nil, err
	}
	return s, nil
}

// makeBucket runs mc in a one-shot pod and polls it to Succeeded. `mc mb
// --ignore-existing` keeps a kept-env re-run idempotent.
func makeBucket(ctx context.Context, kube *k8s.Kubectl, spec Spec, endpoint string) error {
	name := spec.Name + "-mb"
	_ = kube.Delete(ctx, "pod", name)
	if err := kube.Apply(ctx, oneShotManifest(name, spec.ClientImage, bucketScript(spec, endpoint))); err != nil {
		return fmt.Errorf("minio: apply bucket bootstrap: %w", err)
	}
	deadline := time.Now().Add(spec.ReadyTimeout)
	for {
		phase, err := kube.Get(ctx, "pod", name, "{.status.phase}")
		if err == nil {
			switch strings.TrimSpace(phase) {
			case "Succeeded":
				return nil
			case "Failed":
				logs, _ := kube.Logs(ctx, "pod/"+name, "", 20)
				return fmt.Errorf("minio: bucket bootstrap failed%s", k8s.LogTail("logs", logs))
			}
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("minio: bucket bootstrap did not finish in %s", spec.ReadyTimeout)
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("minio: waiting for bucket bootstrap: %w", ctx.Err())
		case <-time.After(readyPoll):
		}
	}
}

// bucketScript retries in-pod: kube-proxy publishes the Service's endpoint
// some moment AFTER the server pod reports Ready, and until it does the
// ClusterIP refuses the connection outright. `mc` does not retry that itself,
// so a single-shot script dies on a race the caller cannot see.
func bucketScript(spec Spec, endpoint string) string {
	return fmt.Sprintf(
		"for i in $(seq 1 %d); do mc alias set target %s %s %s && mc mb --ignore-existing target/%s && exit 0; sleep %d; done; echo minio unreachable after %d attempts; exit 1",
		bucketAttempts, endpoint, AccessKey, SecretKey, spec.Bucket,
		int(bucketRetry.Seconds()), bucketAttempts)
}

func labels() string {
	return fmt.Sprintf("    ampqa: \"1\"\n    %s: \"%s\"\n", kubemeta.TestLabel, kubemeta.TestLabelValue)
}

func serverManifest(spec Spec) string {
	var b strings.Builder
	fmt.Fprintf(&b, "apiVersion: v1\nkind: Pod\nmetadata:\n  name: %s\n  labels:\n%s    app: %s\n", spec.Name, labels(), spec.Name)
	b.WriteString("spec:\n  restartPolicy: Never\n  containers:\n")
	fmt.Fprintf(&b, "    - name: minio\n      image: %s\n", spec.ServerImage)
	fmt.Fprintf(&b, "      args: [\"server\", \"/data\"]\n")
	fmt.Fprintf(&b, "      env:\n        - name: MINIO_ROOT_USER\n          value: %q\n        - name: MINIO_ROOT_PASSWORD\n          value: %q\n", AccessKey, SecretKey)
	fmt.Fprintf(&b, "      ports:\n        - containerPort: %d\n", port)
	fmt.Fprintf(&b, "      readinessProbe:\n        httpGet: {path: /minio/health/ready, port: %d}\n        initialDelaySeconds: 3\n        periodSeconds: 3\n", port)
	b.WriteString("---\n")
	fmt.Fprintf(&b, "apiVersion: v1\nkind: Service\nmetadata:\n  name: %s\n  labels:\n%s", spec.Name, labels())
	fmt.Fprintf(&b, "spec:\n  selector:\n    app: %s\n  ports:\n    - port: %d\n      targetPort: %d\n", spec.Name, port, port)
	return b.String()
}

func oneShotManifest(name, image, script string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "apiVersion: v1\nkind: Pod\nmetadata:\n  name: %s\n  labels:\n%s", name, labels())
	b.WriteString("spec:\n  restartPolicy: Never\n  containers:\n")
	fmt.Fprintf(&b, "    - name: mc\n      image: %s\n      command: [\"sh\", \"-c\", %q]\n", image, script)
	return b.String()
}
