package main

import (
	"encoding/json"
	"strings"
	"testing"

	"meko/ycp/internal/api"
)

// A database declared with no spec at all has to be deployable: the whole point of
// the defaults is that `{"kind":"database","name":"meko"}` bootstraps a working
// data plane on a laptop with no registry credentials and no pre-made Secrets.
func TestDatabaseSpecDefaults(t *testing.T) {
	var spec databaseSpec
	if err := decodeSpec(nil, &spec); err != nil {
		t.Fatalf("decode empty spec: %v", err)
	}
	got := spec.withDefaults()
	if !got.managed() {
		t.Error("an unspecified mode must mean managed, not external")
	}
	// pgvector, not postgres: mem0 needs the vector extension, and a stock postgres
	// starts fine and then fails on the first memory write.
	if !strings.HasPrefix(got.Image, "pgvector/") {
		t.Errorf("default image = %q, want a pgvector image", got.Image)
	}
	if got.Port != 5432 || got.User != "postgres" || got.Database != "postgres" {
		t.Errorf("postgres defaults wrong: port=%d user=%s db=%s", got.Port, got.User, got.Database)
	}
	if got.SecretRef != dbSecret || got.PasswordKey != dbPasswordKey {
		t.Errorf("password location defaults wrong: %s/%s", got.SecretRef, got.PasswordKey)
	}
}

// The yugabytedb engine has different defaults on every axis that matters. Getting
// one of these wrong (port 5432 against a YSQL endpoint on 5433) fails as a
// connection timeout with nothing pointing at the cause.
func TestDatabaseSpecYugabyteDefaults(t *testing.T) {
	spec := databaseSpec{Engine: "yugabytedb"}.withDefaults()
	if spec.Port != 5433 {
		t.Errorf("port = %d, want 5433 (YSQL)", spec.Port)
	}
	if spec.User != "yugabyte" || spec.Database != "yugabyte" {
		t.Errorf("user/db = %s/%s, want yugabyte/yugabyte", spec.User, spec.Database)
	}
	if !strings.Contains(spec.Image, "yugabyte") {
		t.Errorf("image = %q, want a yugabytedb image", spec.Image)
	}
}

// An explicit spec must survive withDefaults untouched — defaults may only fill
// gaps, never override what an operator declared.
func TestDatabaseSpecExplicitWins(t *testing.T) {
	in := databaseSpec{
		Mode: "external", Engine: "yugabytedb", Host: "yb.internal", Port: 9999,
		User: "app", Database: "meko", SecretRef: "byo", PasswordKey: "pw",
	}
	got := in.withDefaults()
	if got.managed() {
		t.Error("mode=external must not become managed")
	}
	if got.Port != 9999 || got.User != "app" || got.Database != "meko" {
		t.Errorf("explicit values overwritten: %+v", got)
	}
	if got.SecretRef != "byo" || got.PasswordKey != "pw" {
		t.Errorf("explicit secret location overwritten: %s/%s", got.SecretRef, got.PasswordKey)
	}
	// External mode must not invent an image: there is no workload to run.
	if got.Image != "" {
		t.Errorf("image = %q, want empty for an external database", got.Image)
	}
}

// The two DSN forms are not interchangeable, and the repo has already been bitten
// by the distinction (LOCAL_DEV_SETUP.md spells it out): meko-mcp-server's pgvector
// path wants libpq keywords, the RAG worker wants a URI.
func TestResolvedDBDSNForms(t *testing.T) {
	db := resolvedDB{host: "db.ns.svc", port: 5432, user: "postgres", password: "p@ss word/1", database: "postgres"}

	libpq := db.libpq("meko")
	for _, want := range []string{"host=db.ns.svc", "port=5432", "dbname=meko", "user=postgres", "password=p@ss word/1"} {
		if !strings.Contains(libpq, want) {
			t.Errorf("libpq DSN %q missing %q", libpq, want)
		}
	}
	if strings.Contains(libpq, "postgresql://") {
		t.Errorf("libpq form must not be a URL: %q", libpq)
	}

	uri := db.uri("meko")
	if !strings.HasPrefix(uri, "postgresql://") {
		t.Errorf("uri form = %q, want a postgresql:// URL", uri)
	}
	// A password with URL-special characters has to survive the round trip, or the
	// agent's own pool connects with a truncated credential.
	if !strings.Contains(uri, "p%40ss%20word%2F1") {
		t.Errorf("uri %q did not percent-encode the password", uri)
	}
	if !strings.HasSuffix(uri, "/meko?sslmode=disable") {
		t.Errorf("uri %q should end with the database and sslmode", uri)
	}
}

// Reconcile order is by kind, not declaration order: an operator who lists
// mcp-server before database still gets a working stack rather than an mcp-server
// that fails with "needs a database resource declared alongside it".
func TestReconcileOrdersDatabaseFirst(t *testing.T) {
	resources := []api.DesiredResource{
		{Kind: "rag-worker", Name: "rag"},
		{Kind: "mcp-server", Name: "mcp"},
		{Kind: "database", Name: "db"},
	}
	var got []string
	for _, r := range inDependencyOrder(resources) {
		got = append(got, r.Kind)
	}
	want := []string{"database", "mcp-server", "rag-worker"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("reconcile order = %v, want %v", got, want)
		}
	}
}

// Every kind the stack claims must have a reconciler, and datapack must NOT be one
// of them — it has its own path, and claiming it here would run both.
func TestStackKinds(t *testing.T) {
	for _, k := range []string{"database", "mcp-server", "rag-worker"} {
		if !stackKinds[k] {
			t.Errorf("kind %q is not registered as a stack kind", k)
		}
	}
	if stackKinds["datapack"] {
		t.Error("datapack must be reconciled by reconcileDatapacks, not the stack")
	}
}

// GC deletes what desired state no longer asks for, so ownership has to be recorded
// from desired state — not from what reconciled cleanly this tick.
//
// This is a data-loss bug observed on a real cluster: mcp-server returned early on the
// tick after its database blipped, so its key was missing from `owned`, and the GC
// deleted a healthy, running, still-declared Deployment and Service. Ownership is now
// taken from the declared resources before anything is applied, so a failed reconcile
// can leave a workload stale but can never make it look undesired.
func TestReconcileClaimsOwnershipFromDesiredStateNotFromSuccess(t *testing.T) {
	// A stack with no k8s client: every apply will fail as soon as it touches the API.
	// That is the point — ownership must be claimed anyway.
	s := newStack(nil, "meko", &dbPool{})
	resources := []api.DesiredResource{
		{Kind: "database", Name: "meko"},
		{Kind: "mcp-server", Name: "mcp"}, // fails: no spec.image
		{Kind: "rag-worker", Name: "rag"}, // fails: no spec.image
		{Kind: "datapack", Name: "acme"},  // retired: no longer a kind the agent knows
		{Kind: "langfuse", Name: "lf"},    // no reconciler at all
	}
	for _, r := range resources {
		if stackKinds[r.Kind] {
			s.owned[resourceKey(r.Kind, r.Name)] = true
		}
	}

	for _, want := range []string{"database.meko", "mcp-server.mcp", "rag-worker.rag"} {
		if !s.owned[want] {
			t.Errorf("resource %q is declared but unowned — GC would delete it", want)
		}
	}
	for _, unwanted := range []string{"datapack.acme", "langfuse.lf"} {
		if s.owned[unwanted] {
			t.Errorf("claimed ownership of %q, which the stack does not manage", unwanted)
		}
	}
}

// A workload without an image must be refused rather than deployed with a guessed
// one: the Meko images are private, so a wrong guess surfaces minutes later as
// ImagePullBackOff instead of immediately as a bad spec.
func TestWorkloadsRequireAnImage(t *testing.T) {
	s := &stack{ns: "meko", owned: map[string]bool{}, db: &resolvedDB{host: "h", port: 5432}}
	for _, kind := range []string{"mcp-server", "rag-worker"} {
		var err error
		r := api.DesiredResource{Kind: kind, Name: kind}
		if kind == "mcp-server" {
			err = s.applyMCPServer(r)
		} else {
			err = s.applyRAGWorker(r)
		}
		if err == nil || !strings.Contains(err.Error(), "spec.image") {
			t.Errorf("%s with no image: err = %v, want one naming spec.image", kind, err)
		}
	}
}

// ...and without a database, since its whole configuration is derived from one.
func TestWorkloadsRequireADatabase(t *testing.T) {
	s := &stack{ns: "meko", owned: map[string]bool{}}
	spec, _ := json.Marshal(workloadSpec{Image: "example.com/mcp:1"})
	err := s.applyMCPServer(api.DesiredResource{Kind: "mcp-server", Name: "mcp", Spec: spec})
	if err == nil || !strings.Contains(err.Error(), "database") {
		t.Errorf("err = %v, want one naming the missing database resource", err)
	}
}

// MEM0_CREATE_TABLES is the difference between a working fresh database and an MCP
// server that starts and then fails on a missing relation. It must default on for a
// database the agent created and off for one the platform provisioned.
func TestCreateTables(t *testing.T) {
	yes := true
	cases := []struct {
		name string
		spec workloadSpec
		db   *resolvedDB
		want string
	}{
		{"managed database creates tables", workloadSpec{}, &resolvedDB{managed: true}, "true"},
		{"external database does not", workloadSpec{}, &resolvedDB{managed: false}, "false"},
		{"explicit spec wins", workloadSpec{CreateTables: &yes}, &resolvedDB{managed: false}, "true"},
	}
	for _, c := range cases {
		if got := createTables(c.spec, c.db); got != c.want {
			t.Errorf("%s: createTables = %q, want %q", c.name, got, c.want)
		}
	}
}

// The environment state is now observed rather than narrated. The distinction that
// matters to an operator: a stack that has not come up yet is PROVISIONING, one that
// came up and broke is DEGRADED.
func TestStateFor(t *testing.T) {
	agent := api.Component{Name: "meko-agent", Healthy: 1, Total: 1}
	unready := api.Component{Name: "mcp-server", Healthy: 0, Total: 1}

	wasReady := false
	if got := stateFor([]api.Component{agent, unready}, &wasReady); got != api.StateProvisioning {
		t.Errorf("coming up: state = %s, want PROVISIONING", got)
	}
	if wasReady {
		t.Error("wasReady must stay false until everything is healthy")
	}
	if got := stateFor([]api.Component{agent}, &wasReady); got != api.StateReady {
		t.Errorf("all healthy: state = %s, want READY", got)
	}
	if !wasReady {
		t.Error("wasReady must latch once READY")
	}
	if got := stateFor([]api.Component{agent, unready}, &wasReady); got != api.StateDegraded {
		t.Errorf("regressed after READY: state = %s, want DEGRADED", got)
	}
}

// A component reporting 0/0 is not healthy. Treating it as healthy is how an
// environment goes green while its workload does not exist — the readback failed and
// left zero values.
func TestStateForZeroReplicasIsNotHealthy(t *testing.T) {
	wasReady := false
	comps := []api.Component{{Name: "mcp-server", Healthy: 0, Total: 0}}
	if got := stateFor(comps, &wasReady); got != api.StateProvisioning {
		t.Errorf("state = %s, want PROVISIONING for a 0/0 component", got)
	}
}

// The trailing dot on an in-cluster Service name is load-bearing, and its absence
// fails in a way that looks like anything but DNS.
//
// Kubernetes sets ndots:5 in every pod's resolv.conf, and `meko.ns.svc.cluster.local`
// has four dots — so a resolver treats it as relative and tries the search list first
// (`meko.ns.svc.cluster.local.ns.svc.cluster.local`, …) before the name as written.
// Measured against a live cluster: connecting to the bare form timed out every time,
// while the same address with a trailing dot — and the ClusterIP, and the short name —
// connected instantly. The symptom was a database that was plainly up and a fleet view
// reporting components that could not reach it.
func TestSvcFQDNIsAbsolute(t *testing.T) {
	got := svcFQDN("meko", "ycp-agent")
	if got != "meko.ycp-agent.svc.cluster.local." {
		t.Fatalf("svcFQDN = %q, want the absolute form with a trailing dot", got)
	}
	if !strings.HasSuffix(got, ".") {
		t.Error("no trailing dot: resolvers will walk the ndots search list and stall")
	}
}

// Every DSN handed to a workload has to carry that absolute name — this is the
// property that actually matters, since the DSNs are what connect.
func TestResolvedDBDSNsUseTheAbsoluteName(t *testing.T) {
	db := resolvedDB{host: svcFQDN("meko", "ycp-agent"), port: 5432, user: "postgres", password: "p", database: "postgres"}
	for name, dsn := range map[string]string{"libpq": db.libpq("meko"), "uri": db.uri("meko")} {
		if !strings.Contains(dsn, "meko.ycp-agent.svc.cluster.local.") {
			t.Errorf("%s DSN lost the trailing dot: %s", name, dsn)
		}
	}
}

func TestImageTag(t *testing.T) {
	cases := map[string]string{
		"161908593886.dkr.ecr.us-east-2.amazonaws.com/meko/meko-mcp-server:2.1.0-20260730": "2.1.0-20260730",
		"pgvector/pgvector:pg16":       "pg16",
		"registry:5000/meko/mcp":       "latest",
		"meko/mcp@sha256:abc123":       "latest",
		"registry:5000/meko/mcp:1.2.3": "1.2.3",
	}
	for image, want := range cases {
		if got := imageTag(image); got != want {
			t.Errorf("imageTag(%q) = %q, want %q", image, got, want)
		}
	}
}

// Object names come from operator-supplied resource names, so they have to be
// coerced into something the API server will accept.
func TestBaseNameOr(t *testing.T) {
	cases := map[string]string{
		"Acme Prod": "acme-prod",
		"meko_db":   "meko-db",
		"---":       "fallback",
		"":          "fallback",
		"ok-name":   "ok-name",
	}
	for in, want := range cases {
		if got := baseNameOr(in, "fallback"); got != want {
			t.Errorf("baseNameOr(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestSummarize(t *testing.T) {
	got := summarize([]api.Component{
		{Name: "meko-agent", Healthy: 1, Total: 1},
		{Name: "mcp-server", Healthy: 0, Total: 2},
	})
	if got != "meko-agent 1/1, mcp-server 0/2" {
		t.Errorf("summarize = %q", got)
	}
}
