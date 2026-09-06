package deploy

import (
	"log/slog"
	"testing"
)

// The provider-ID schemes each cloud-controller-manager writes. Getting this
// mapping wrong is not a cosmetic bug: the chart picks the annotation set from
// it, and the wrong answer on an internal load balancer means a public address.
func TestParseCloudProvider(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
		want string
	}{
		{
			name: "EKS",
			body: `{"items":[{"spec":{"providerID":"aws:///us-east-2a/i-020ce166bb393b119"}}]}`,
			want: "aws",
		},
		{
			// GKE nodes say "gce://" — the cloud is still GCP.
			name: "GKE",
			body: `{"items":[{"spec":{"providerID":"gce://yugabyte/us-west1-b/gke-itest-dev-pool-1-abc"}}]}`,
			want: "gcp",
		},
		{
			name: "AKS",
			body: `{"items":[{"spec":{"providerID":"azure:///subscriptions/0000/resourceGroups/rg/providers/Microsoft.Compute/virtualMachines/vm-0"}}]}`,
			want: "azure",
		},
		{
			// kind, minikube, bare metal: no cloud LB to annotate for.
			name: "no provider ID",
			body: `{"items":[{"spec":{}}]}`,
			want: "",
		},
		{
			name: "unknown provider",
			body: `{"items":[{"spec":{"providerID":"openstack:///abc"}}]}`,
			want: "",
		},
		{
			// A node without a provider ID must not mask a later one that has it.
			name: "skips nodes with no provider ID",
			body: `{"items":[{"spec":{}},{"spec":{"providerID":"aws:///us-east-2a/i-1"}}]}`,
			want: "aws",
		},
		{name: "empty list", body: `{"items":[]}`, want: ""},
		{name: "malformed json", body: `{`, want: ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := parseCloudProvider([]byte(tc.body)); got != tc.want {
				t.Errorf("parseCloudProvider(%s) = %q, want %q", tc.name, got, tc.want)
			}
		})
	}
}

// A failed detection must not be memoized. k8sAPIGet is best-effort and returns
// nothing on a transient apiserver error or a throttled node LIST; caching that
// would refuse every later nlb_internal create for the life of the controller
// process, recoverable only by a restart. Out of cluster k8sAPIGet always fails,
// which is exactly the failing case.
func TestDetectCloudDoesNotCacheFailure(t *testing.T) {
	k := NewKubernetesDeployer(KubernetesConfig{}, slog.New(slog.DiscardHandler))
	for i := range 2 {
		if got := k.detectCloud(t.Context()); got != "" {
			t.Fatalf("call %d: detectCloud = %q, want empty out of cluster", i, got)
		}
	}
	if k.cloud != "" {
		t.Errorf("cloud cache = %q after two failures, want empty so the next create retries", k.cloud)
	}
}

// An explicit cloud short-circuits the node read entirely — the way out for a
// controller without `nodes get, list`, or a cluster whose provider IDs say
// something unexpected.
func TestDetectCloudExplicitWins(t *testing.T) {
	k := NewKubernetesDeployer(KubernetesConfig{Cloud: "azure"}, slog.New(slog.DiscardHandler))
	if got := k.detectCloud(t.Context()); got != "azure" {
		t.Errorf("detectCloud = %q, want azure", got)
	}
}
