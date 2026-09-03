package deploy

import "testing"

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
