package api_test

import (
	"context"
	"net/http"
	"testing"

	"github.com/yugabyte/amp/controller/internal/api"
	"github.com/yugabyte/amp/controller/internal/core"
	"github.com/yugabyte/amp/controller/internal/deploy"
	"github.com/yugabyte/amp/controller/internal/extensions"
)

// caDeployer stands in for the Kubernetes deployer's certificate answers.
type caDeployer struct {
	*fakeDeployer
	cert deploy.ServerCert
}

func (d caDeployer) EndpointServerCert(context.Context, string) (deploy.ServerCert, error) {
	return d.cert, nil
}

// `required` is about the CA, for Postgres and the gateway alike.
// `documentdb_verifiable` is about the gateway alone, so it appears exactly where
// documentdb_address does, and says whether the gateway serves the compute's
// certificate or self-signs.
func TestEndpointCaBundle(t *testing.T) {
	const pem = "-----BEGIN CERTIFICATE-----\nMIIB\n-----END CERTIFICATE-----\n"
	cases := []struct {
		name       string
		dep        deploy.Deployer
		required   bool
		gatewayVer bool
	}{
		{"deployer with no certificate notion", &fakeDeployer{addr: "ep.test:5432"}, false, false},
		{"compute TLS off", caDeployer{fakeDeployer: &fakeDeployer{addr: "ep.test:5432"}}, false, false},
		{"publicly trusted", caDeployer{fakeDeployer: &fakeDeployer{addr: "ep.test:5432"},
			cert: deploy.ServerCert{GatewayServes: true}}, false, true},
		{"private CA", caDeployer{fakeDeployer: &fakeDeployer{addr: "ep.test:5432"},
			cert: deploy.ServerCert{CA: pem, GatewayServes: true}}, true, true},
		{"private CA, gateway rendered before it served it", caDeployer{fakeDeployer: &fakeDeployer{addr: "ep.test:5432"},
			cert: deploy.ServerCert{CA: pem}}, true, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			prov := api.Provisioning{Deployer: tc.dep, Extensions: extensions.NewService(extensions.NewCatalog("quay.io/yugabyte"), nil)}
			client, backend, store := newTestServerWith(t, core.NewMemBackend(), prov)
			ctx := context.Background()
			user := mustCreateUser(t, client, false)
			pool := mustCreatePool(t, client, user.Id, "ca-bundle-pool")

			get := func(engine string) *api.EndpointCaBundle {
				t.Helper()
				withWriter := true
				req := api.CreateClusterRequest{Name: engine, WithWriter: &withWriter, Password: strptr(testPassword)}
				if engine != "postgres" {
					e := api.CreateClusterRequestEngine(engine)
					req.Engine = &e
				}
				cr, err := client.CreateClusterWithResponse(ctx, pool.Id, req)
				if err != nil || cr.StatusCode() != http.StatusAccepted {
					t.Fatalf("create %s cluster: %v (%d)", engine, err, cr.StatusCode())
				}
				drainTasks(t, store, backend, prov)
				clusterID := *cr.JSON202.ClusterId
				cl, err := client.GetClusterWithResponse(ctx, pool.Id, clusterID)
				if err != nil || cl.JSON200 == nil {
					t.Fatalf("get cluster: %v", err)
				}
				branchID := cl.JSON200.DefaultBranch.BranchId
				eps, err := client.ListEndpointsWithResponse(ctx, pool.Id, clusterID, branchID, nil)
				if err != nil || eps.JSON200 == nil || len(eps.JSON200.Endpoints) == 0 {
					t.Fatalf("list endpoints: %v", err)
				}
				res, err := client.GetEndpointCaBundleWithResponse(ctx, pool.Id, clusterID, branchID, eps.JSON200.Endpoints[0].Id)
				if err != nil || res.JSON200 == nil {
					t.Fatalf("get ca bundle: %v (%d)", err, res.StatusCode())
				}
				return res.JSON200
			}

			for _, engine := range []string{"postgres", "documentdb"} {
				got := get(engine)
				if got.Required != tc.required {
					t.Errorf("%s: required=%v, want %v", engine, got.Required, tc.required)
				}
				if tc.required && (got.CaCrt == nil || *got.CaCrt != pem) {
					t.Errorf("%s: ca_crt = %v, want the deployer's PEM", engine, got.CaCrt)
				}
				if !tc.required && got.CaCrt != nil && *got.CaCrt != "" {
					t.Errorf("%s: ca_crt = %q with required=false", engine, *got.CaCrt)
				}
				if got.VerifyName == nil || *got.VerifyName != "ep.test" {
					t.Errorf("%s: verify_name = %v, want the address's host", engine, got.VerifyName)
				}
				switch {
				case engine == "postgres" && got.DocumentdbVerifiable != nil:
					t.Errorf("postgres: documentdb_verifiable = %v, want absent", *got.DocumentdbVerifiable)
				case engine == "documentdb" && got.DocumentdbVerifiable == nil:
					t.Error("documentdb: documentdb_verifiable absent")
				case engine == "documentdb" && *got.DocumentdbVerifiable != tc.gatewayVer:
					t.Errorf("documentdb: documentdb_verifiable = %v, want %v", *got.DocumentdbVerifiable, tc.gatewayVer)
				}
			}
		})
	}
}
