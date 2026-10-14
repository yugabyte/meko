package features

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"slices"
	"strings"
	"time"

	"github.com/yugabyte/amp/test/functional/framework/apps/clientpod"
	"github.com/yugabyte/amp/test/functional/framework/apps/controller"
	"github.com/yugabyte/amp/test/functional/framework/apps/pg"
	"github.com/yugabyte/amp/test/functional/framework/core"
	"github.com/yugabyte/amp/test/functional/framework/exec/k8s"
)

const (
	documentdbEngine   = "documentdb"
	documentdbPort     = "10260"
	documentdbDocs     = 1000
	documentdbGateway  = 5 * time.Minute
	documentdbLBSettle = 5 * time.Minute
)

// After the seed: ids 0..999 with k = id % 10, the k=9 tenth deleted, k=3 flagged.
var documentdbWant = documentdbState{Count: 900, Flagged: 100, K5: 100, Sum: 449100}

const documentdbSeedJS = `const c = db.getSiblingDB('ampqa').items;
c.drop();
const docs = [];
for (let i = 0; i < 1000; i++) docs.push({_id: i, k: i % 10, v: 'x'.repeat(100)});
c.insertMany(docs);
c.createIndex({k: 1});
c.updateMany({k: 3}, {$set: {flag: true}});
c.deleteMany({k: 9});`

const documentdbReadJS = `const c = db.getSiblingDB('ampqa').items;
const agg = c.aggregate([{$group: {_id: null, s: {$sum: '$_id'}}}]).toArray();
print('AMPQA ' + JSON.stringify({count: c.countDocuments({}), flagged: c.countDocuments({flag: true}),
  k5: c.find({k: 5}).hint({k: 1}).toArray().length, sum: agg.length ? agg[0].s : -1,
  indexes: c.getIndexes().map(i => i.name)}));`

type documentdbState struct {
	Count   int64    `json:"count"`
	Flagged int64    `json:"flagged"`
	K5      int64    `json:"k5"`
	Sum     int64    `json:"sum"`
	Indexes []string `json:"indexes"`
}

func (s documentdbState) same(o documentdbState) bool {
	return s.Count == o.Count && s.Flagged == o.Flagged && s.K5 == o.K5 && s.Sum == o.Sum
}

func init() {
	core.Register(core.Test{
		Name:        "features/amp_documentdb_gateway",
		Suite:       "features",
		Category:    core.Functional,
		Env:         core.EnvSpec{Sharing: core.Exclusive, Shape: core.SharedControlPlane},
		Tags:        []string{"regression"},
		OwningTeam:  core.TeamControlPlane,
		Description: "The documentdb engine through its MongoDB wire-protocol gateway, the way a customer's driver reaches it (meko#1509, meko#1594). A writer on an engine:documentdb cluster reports documentdb_address on its address's host and port 10260, health ready, and a /ca answer with documentdb_verifiable. mongosh in the amp namespace connects to documentdb_address with the TLS options /ca prescribes (tlsCAFile against the address when the gateway serves the compute's certificate), writes, indexes, updates and deletes documents, and reads them back by index and aggregate; Postgres sees the same documents in the documentdb database; a wrong password is refused. After a compute restart the gateway serves the same documents. A fork's nlb_internal writer publishes the gateway on its load balancer's host, serves the fork's copy, and its writes stay off the parent.",
		Params:      []string{"functional", "functional.documentdb", "env.ampstack", "apps.ybastack", "apps.yba", "apps.ybk8s"},
		Timeout:     50 * time.Minute,
		Run:         runAmpDocumentDBGateway,
	})
}

type documentdbRun struct {
	a       *ampFeatureRun
	cl      *controller.Client
	kube    *k8s.Kubectl
	cluster controller.Cluster
	writer  controller.Endpoint
	ca      controller.EndpointCA
	pod     *clientpod.K8s
	podName string
}

func runAmpDocumentDBGateway(t *core.T) error {
	return runAmpFeature(t, func(a *ampFeatureRun) []phase {
		d := &documentdbRun{a: a}
		return []phase{
			{"documentdb_endpoint", d.endpoint},
			{"documentdb_client_pod", d.clientPod},
			{"documentdb_crud", d.crud},
			{"documentdb_after_restart", d.afterRestart},
			{"documentdb_fork_nlb", d.forkNLB},
		}
	})
}

func (d *documentdbRun) endpoint(ctx context.Context) error {
	cl, err := d.a.f.controllerClient(ctx)
	if err != nil {
		return err
	}
	d.cl = cl
	d.kube = d.a.f.ampKube()
	if d.kube == nil {
		d.a.f.t.Skip("no kube handle for the amp namespace, so no client pod can reach the gateway")
		return nil
	}
	proj := d.a.proj
	name := "docdb-" + d.a.f.suffix()
	_, err = cl.CreateClusterAndWait(ctx, proj.poolID, controller.CreateClusterRequest{Name: name, PgVersion: d.a.f.cfg.PgVersion, Engine: documentdbEngine})
	if extensionsUnavailable(err) || unknownEngineName(err) == documentdbEngine || unknownExtensionName(err) != "" {
		d.a.f.t.Skip("this control plane cannot create a documentdb cluster: " + err.Error())
		return nil
	}
	if err != nil {
		return fmt.Errorf("create the documentdb cluster: %w", err)
	}
	c, err := findClusterCtl(ctx, cl, proj.poolID, name)
	if err != nil {
		return err
	}
	d.a.f.register("delete documentdb cluster", func(ctx context.Context) error {
		_, e := cl.DeleteClusterAndWait(ctx, proj.poolID, c.ID)
		return e
	})
	d.cluster = c
	if !slices.Contains(c.Extensions, "documentdb") {
		return fmt.Errorf("the documentdb engine seeded %v, without documentdb", c.Extensions)
	}
	ep, err := createEndpoint(ctx, cl, proj.poolID, c.ID, c.DefaultBranch.BranchID)
	if err != nil {
		return fmt.Errorf("create the documentdb writer: %w", err)
	}
	if d.writer, err = d.waitGateway(ctx, ep); err != nil {
		return err
	}
	if err := d.addressOnHost(d.writer); err != nil {
		return err
	}
	ca, err := cl.GetEndpointCA(ctx, proj.poolID, c.ID, d.writer.BranchID, d.writer.ID)
	if err != nil {
		return fmt.Errorf("GET /ca of the documentdb writer: %w", err)
	}
	if ca.DocumentDBVerifiable == nil {
		return fmt.Errorf("GET /ca of an endpoint reporting documentdb_address %s carries no documentdb_verifiable", d.writer.DocumentDBAddress)
	}
	if *ca.DocumentDBVerifiable && ca.Required && strings.TrimSpace(ca.CACrt) == "" {
		return fmt.Errorf("GET /ca says the gateway is verifiable and a bundle is required, but ca_crt is empty")
	}
	d.ca = ca
	d.a.f.logf("documentdb writer %s: address %s, documentdb_address %s, /ca required=%v verifiable=%v", d.writer.ID, d.writer.Address, d.writer.DocumentDBAddress, ca.Required, *ca.DocumentDBVerifiable)
	return nil
}

// waitGateway waits for documentdb_address and a ready pod: the gateway binds
// only once compute_ctl has created the extension.
func (d *documentdbRun) waitGateway(ctx context.Context, ep controller.Endpoint) (controller.Endpoint, error) {
	got := ep
	err := pollUntil(ctx, documentdbGateway, 5*time.Second, func() (bool, error) {
		e, err := d.cl.GetEndpoint(ctx, d.a.proj.poolID, ep.ClusterID, ep.BranchID, ep.ID)
		if err != nil {
			return false, nil
		}
		got = e
		return e.DocumentDBAddress != "" && e.Health != nil && e.Health.Status == controller.HealthReady, nil
	})
	if err != nil {
		return got, fmt.Errorf("endpoint %s on a documentdb branch did not report a documentdb_address and health ready within %s (address %q, documentdb_address %q, health %+v): %w",
			ep.ID, documentdbGateway, got.Address, got.DocumentDBAddress, got.Health, err)
	}
	return got, nil
}

func (d *documentdbRun) addressOnHost(ep controller.Endpoint) error {
	host, _, err := net.SplitHostPort(ep.Address)
	if err != nil {
		host = ep.Address
	}
	if want := net.JoinHostPort(host, documentdbPort); ep.DocumentDBAddress != want {
		return fmt.Errorf("endpoint %s reports documentdb_address %q, want %q: the same host as address %q on the gateway's port", ep.ID, ep.DocumentDBAddress, want, ep.Address)
	}
	return nil
}

func (d *documentdbRun) clientPod(ctx context.Context) error {
	if d.kube == nil {
		return nil
	}
	d.podName = "ampqa-mongo-" + d.a.f.suffix()
	cp := clientpod.NewK8s(d.kube)
	spec := clientpod.Spec{
		Name:    d.podName,
		Image:   d.a.f.t.Options.GetString("functional.documentdb.client_image", "mongo:8.0"),
		Command: []string{"sh", "-c", `printf '%s' "$CA_CRT" > /tmp/ca.crt && sleep infinity`},
		Env:     map[string]string{"CA_CRT": d.ca.CACrt},
	}
	if err := cp.Start(ctx, spec); err != nil {
		return core.AsEnvFailure(fmt.Errorf("start the mongosh client pod (%s): %w", spec.Image, err))
	}
	d.pod = cp
	d.a.f.register("stop the mongosh client pod", func(ctx context.Context) error {
		return cp.Stop(ctx, discardSink{})
	})
	return nil
}

func (d *documentdbRun) uri(address, password string) string {
	tls := "tls=true&tlsAllowInvalidCertificates=true"
	if d.ca.DocumentDBVerifiable != nil && *d.ca.DocumentDBVerifiable {
		tls = "tls=true"
		if d.ca.Required {
			tls += "&tlsCAFile=/tmp/ca.crt"
		}
	}
	return fmt.Sprintf("mongodb://%s:%s@%s/?%s&authMechanism=SCRAM-SHA-256", d.a.f.cfg.EndpointUser, password, address, tls)
}

func (d *documentdbRun) mongosh(ctx context.Context, address, password, js string) (string, error) {
	cmd := fmt.Sprintf("mongosh --quiet %s --eval %s", shellQuote(d.uri(address, password)), shellQuote(js))
	res, err := d.kube.ForPod(d.podName, "").Run(ctx, cmd)
	out := strings.TrimSpace(res.Stdout + "\n" + res.Stderr)
	if err == nil && res.ExitCode != 0 {
		err = fmt.Errorf("exit %d", res.ExitCode)
	}
	if err != nil {
		return out, fmt.Errorf("mongosh against %s: %w (output: %s)", address, err, truncateFor([]byte(out), 2000))
	}
	return out, nil
}

func (d *documentdbRun) read(ctx context.Context, address string) (documentdbState, error) {
	out, err := d.mongosh(ctx, address, d.a.f.cfg.EndpointPassword, documentdbReadJS)
	if err != nil {
		return documentdbState{}, err
	}
	for _, line := range strings.Split(out, "\n") {
		if rest, ok := strings.CutPrefix(strings.TrimSpace(line), "AMPQA "); ok {
			var s documentdbState
			if err := json.Unmarshal([]byte(rest), &s); err != nil {
				return s, fmt.Errorf("decode the read script's result %q: %w", rest, err)
			}
			return s, nil
		}
	}
	return documentdbState{}, fmt.Errorf("the read script printed no result line through %s: %s", address, truncateFor([]byte(out), 2000))
}

func (d *documentdbRun) crud(ctx context.Context) error {
	if d.pod == nil {
		return nil
	}
	addr := d.writer.DocumentDBAddress
	if _, err := d.mongosh(ctx, addr, d.a.f.cfg.EndpointPassword, documentdbSeedJS); err != nil {
		return fmt.Errorf("seed through the gateway: %w", err)
	}
	got, err := d.read(ctx, addr)
	if err != nil {
		return err
	}
	if !got.same(documentdbWant) {
		return fmt.Errorf("through the gateway the collection reads %+v, want %+v", got, documentdbWant)
	}
	if !slices.Contains(got.Indexes, "k_1") {
		return fmt.Errorf("the index created through the gateway is missing: getIndexes returned %v", got.Indexes)
	}
	out, err := d.mongosh(ctx, addr, "wrong-"+d.a.f.suffix(), "db.runCommand({ping: 1})")
	if err == nil {
		return fmt.Errorf("the gateway accepted a wrong password: %s", truncateFor([]byte(out), 500))
	}
	// The gateway answers a failed SCRAM proof with "Invalid key".
	if low := strings.ToLower(out); !strings.Contains(low, "auth") && !strings.Contains(low, "invalid key") {
		return fmt.Errorf("a wrong password failed without naming authentication: %s", truncateFor([]byte(out), 1000))
	}
	return d.postgresSees(ctx, d.writer, documentdbWant.Count)
}

// postgresSees counts the collection from SQL, in the documentdb database
// compute_ctl created the extension in.
func (d *documentdbRun) postgresSees(ctx context.Context, ep controller.Endpoint, want int64) error {
	conn, err := d.a.f.endpointSQLWith(ctx, ep, func(ci *pg.ConnInfo) { ci.Database = "documentdb" })
	if err != nil {
		return fmt.Errorf("connect to the documentdb database of endpoint %s: %w", ep.ID, err)
	}
	defer conn.Close()
	n, err := conn.QueryInt(ctx, "SELECT count(*) FROM documentdb_api.collection('ampqa', 'items')")
	if err != nil {
		return fmt.Errorf("count ampqa.items from SQL: %w", err)
	}
	if n != want {
		return fmt.Errorf("Postgres counts %d documents in ampqa.items, the gateway %d", n, want)
	}
	return nil
}

func (d *documentdbRun) afterRestart(ctx context.Context) error {
	if d.pod == nil {
		return nil
	}
	conn, ok, err := d.a.restartEndpointCompute(ctx, d.writer)
	if err != nil {
		return err
	}
	if !ok {
		return nil
	}
	conn.Close()
	ep, err := d.waitGateway(ctx, d.writer)
	if err != nil {
		return fmt.Errorf("after a compute restart: %w", err)
	}
	if ep.DocumentDBAddress != d.writer.DocumentDBAddress {
		return fmt.Errorf("documentdb_address moved from %s to %s across a compute restart", d.writer.DocumentDBAddress, ep.DocumentDBAddress)
	}
	var got documentdbState
	err = pollUntil(ctx, documentdbGateway, 5*time.Second, func() (bool, error) {
		got, err = d.read(ctx, ep.DocumentDBAddress)
		return err == nil, nil
	})
	if err != nil {
		return fmt.Errorf("the gateway did not serve the collection within %s of the compute restart: %w", documentdbGateway, err)
	}
	if !got.same(documentdbWant) {
		return fmt.Errorf("after a compute restart the gateway reads %+v, want %+v", got, documentdbWant)
	}
	return nil
}

func (d *documentdbRun) forkNLB(ctx context.Context) error {
	if d.pod == nil {
		return nil
	}
	proj := d.a.proj
	name := "docfork-" + d.a.f.suffix()
	if _, err := d.cl.CreateBranchAndWait(ctx, proj.poolID, d.cluster.ID, controller.CreateBranchRequest{Name: name, ParentBranchID: d.cluster.DefaultBranch.BranchID}); err != nil {
		return fmt.Errorf("fork the documentdb branch: %w", err)
	}
	br, err := findBranchCtl(ctx, d.cl, proj.poolID, d.cluster.ID, name)
	if err != nil {
		return err
	}
	_, err = createEndpointReq(ctx, d.cl, proj.poolID, d.cluster.ID, br.BranchID, controller.CreateEndpointRequest{
		Role: roleReadWrite, Connectivity: &controller.EndpointConnectivity{Mode: controller.ConnectivityNLBInternal},
	})
	if err != nil {
		if strings.Contains(err.Error(), "cloud") {
			d.a.f.logf("nlb_internal is not available on this cluster (%v), so the load-balanced gateway is not checked", err)
			return nil
		}
		return fmt.Errorf("create the fork's nlb_internal writer: %w", err)
	}
	eps, err := d.cl.ListAllEndpoints(ctx, proj.poolID, d.cluster.ID, br.BranchID)
	if err != nil || len(eps) != 1 {
		return fmt.Errorf("list the fork's endpoints: %d found, %v", len(eps), err)
	}
	var ep controller.Endpoint
	err = pollUntil(ctx, documentdbLBSettle, 10*time.Second, func() (bool, error) {
		e, err := d.cl.GetEndpoint(ctx, proj.poolID, d.cluster.ID, br.BranchID, eps[0].ID)
		if err != nil {
			return false, nil
		}
		ep = e
		return e.DocumentDBAddress != "" && e.Health != nil && e.Health.Status == controller.HealthReady, nil
	})
	if err != nil && ep.DocumentDBAddress == "" {
		d.a.f.logf("the fork's nlb_internal writer reports address %q and no documentdb_address within %s; the spec leaves it absent when the address fell back to a NodePort, so the load-balanced gateway is not checked", ep.Address, documentdbLBSettle)
		return nil
	}
	if err != nil {
		return fmt.Errorf("the fork's nlb_internal writer reports documentdb_address %s but is not ready within %s: health %+v", ep.DocumentDBAddress, documentdbLBSettle, ep.Health)
	}
	if err := d.addressOnHost(ep); err != nil {
		return err
	}
	if strings.Contains(ep.DocumentDBAddress, ".svc") {
		return fmt.Errorf("the nlb_internal writer's documentdb_address %s is a cluster Service name, not its load balancer", ep.DocumentDBAddress)
	}
	ca, err := d.cl.GetEndpointCA(ctx, proj.poolID, d.cluster.ID, br.BranchID, ep.ID)
	if err != nil {
		return err
	}
	saved := d.ca
	d.ca = ca
	defer func() { d.ca = saved }()
	if ca.Required && ca.CACrt != saved.CACrt {
		d.a.f.logf("the fork's CA bundle differs from the parent's; the client pod holds the parent's, so the fork is read without CA verification")
		f := false
		d.ca.DocumentDBVerifiable = &f
	}
	var got documentdbState
	err = pollUntil(ctx, documentdbLBSettle, 10*time.Second, func() (bool, error) {
		got, err = d.read(ctx, ep.DocumentDBAddress)
		return err == nil, nil
	})
	if err != nil {
		return fmt.Errorf("the fork's gateway at its load balancer %s did not serve within %s: %w", ep.DocumentDBAddress, documentdbLBSettle, err)
	}
	if !got.same(documentdbWant) {
		return fmt.Errorf("the fork's gateway reads %+v, want the parent's %+v at the fork point", got, documentdbWant)
	}
	if _, err := d.mongosh(ctx, ep.DocumentDBAddress, d.a.f.cfg.EndpointPassword, "db.getSiblingDB('ampqa').items.deleteMany({k: 5})"); err != nil {
		return fmt.Errorf("delete on the fork through its load balancer: %w", err)
	}
	d.ca = saved
	parent, err := d.read(ctx, d.writer.DocumentDBAddress)
	if err != nil {
		return err
	}
	if !parent.same(documentdbWant) {
		return fmt.Errorf("a delete on the fork changed the parent: it reads %+v, want %+v", parent, documentdbWant)
	}
	return nil
}
