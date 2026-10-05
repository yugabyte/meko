// Package llmmock deploys the LLM mock into a stack's namespace and gives tests a
// client for it. The mock is `mekoqa mock llm`, so the pod runs the framework's
// own binary on a public image with a shell, copied in after the pod starts.
package llmmock

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/yugabyte/meko/meko/test/functional/framework/apps/mockpod"
	"github.com/yugabyte/meko/meko/test/functional/framework/exec/k8s"
	"github.com/yugabyte/meko/meko/test/functional/framework/kubemeta"
)

const (
	DefaultName = "llmmock"
	Port        = 8080
)

type Spec struct {
	Name         string
	Pod          mockpod.Config
	ReadyTimeout time.Duration
}

func (s Spec) withDefaults() Spec {
	if s.Name == "" {
		s.Name = DefaultName
	}
	s.Pod = s.Pod.WithDefaults()
	if s.ReadyTimeout <= 0 {
		s.ReadyTimeout = 5 * time.Minute
	}
	return s
}

type Server struct {
	kube *k8s.Kubectl
	name string
}

func (s *Server) Name() string { return s.name }

func (s *Server) Endpoint() string { return EndpointFor(s.name, s.kube.Namespace()) }

func EndpointFor(name, namespace string) string {
	return fmt.Sprintf("http://%s.%s.svc.cluster.local:%d", name, namespace, Port)
}

// Deploy is safe to repeat: the pod restarts only when the binary or image changed.
func Deploy(ctx context.Context, kube *k8s.Kubectl, spec Spec) (*Server, error) {
	spec = spec.withDefaults()
	bin, err := mockpod.Resolve(ctx, spec.Pod)
	if err != nil {
		return nil, fmt.Errorf("llmmock: %w", err)
	}
	manifest, err := Manifest(spec, bin)
	if err != nil {
		return nil, err
	}
	if err := kube.Apply(ctx, manifest); err != nil {
		return nil, fmt.Errorf("llmmock: apply: %w", err)
	}
	if err := mockpod.Install(ctx, kube, spec.Name, bin, "", spec.ReadyTimeout); err != nil {
		return nil, fmt.Errorf("llmmock: %w", err)
	}
	if err := kube.RolloutStatus(ctx, "deployment", spec.Name, spec.ReadyTimeout); err != nil {
		logs, _ := kube.Logs(ctx, "deployment/"+spec.Name, "", 20)
		return nil, fmt.Errorf("llmmock: rollout: %w%s", err, k8s.LogTail("logs", logs))
	}
	return &Server{kube: kube, name: spec.Name}, nil
}

func labels(name string) map[string]string {
	return map[string]string{"mekoqa": "1", kubemeta.TestLabel: kubemeta.TestLabelValue, "app": name}
}

func Manifest(spec Spec, bin mockpod.Binary) (string, error) {
	spec = spec.withDefaults()
	meta := func(name string) map[string]any {
		return map[string]any{"name": name, "labels": labels(spec.Name)}
	}
	podLabels := labels(spec.Name)
	podLabels[mockpod.BinLabel] = bin.Label()
	dep := map[string]any{
		"apiVersion": "apps/v1", "kind": "Deployment",
		"metadata": meta(spec.Name),
		"spec": map[string]any{
			"replicas": 1,
			"selector": map[string]any{"matchLabels": map[string]string{"app": spec.Name}},
			"template": map[string]any{
				"metadata": map[string]any{"labels": podLabels},
				"spec": map[string]any{
					"containers": []any{map[string]any{
						"name":    "llmmock",
						"image":   spec.Pod.Image,
						"command": mockpod.Command("mock", "llm", "--port", fmt.Sprint(Port)),
						"ports":   []any{map[string]any{"containerPort": Port}},
						"readinessProbe": map[string]any{
							"httpGet":       map[string]any{"path": "/_mock/health", "port": Port},
							"periodSeconds": 2,
						},
						"resources":    map[string]any{"requests": map[string]string{"cpu": "100m", "memory": "128Mi"}, "limits": map[string]string{"memory": "512Mi"}},
						"volumeMounts": []any{mockpod.VolumeMount()},
					}},
					"volumes": []any{mockpod.Volume()},
				},
			},
		},
	}
	svc := map[string]any{
		"apiVersion": "v1", "kind": "Service",
		"metadata": meta(spec.Name),
		"spec": map[string]any{
			"selector": map[string]string{"app": spec.Name},
			"ports":    []any{map[string]any{"port": Port, "targetPort": Port}},
		},
	}
	b, err := json.Marshal(map[string]any{"apiVersion": "v1", "kind": "List", "items": []any{dep, svc}})
	if err != nil {
		return "", err
	}
	return string(b), nil
}

func OpenAIBaseURL(name, namespace string) string {
	return strings.TrimSuffix(EndpointFor(name, namespace), "/") + "/v1"
}
