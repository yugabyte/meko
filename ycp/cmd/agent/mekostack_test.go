package main

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"meko/ycp/internal/api"
)

// errStub stands in for whatever a reconciler returns when an apply is rejected.
var errStub = errors.New("apply rejected")

// fullRelease is a complete image set, so a test can assert on what a component was
// given without matching real tags.
var fullRelease = stackImages{
	Database:         "registry/yugabyte-db/ybdb-meko-release:t",
	APIServer:        "registry/meko/meko-api-server:t",
	MCPServer:        "registry/meko/meko-mcp-server:t",
	RAGWorker:        "registry/meko/rag-agent:t",
	InferenceGateway: "registry/meko/inference-gateway:t",
	UI:               "registry/meko/meko-ui:t",
}

func mekoStackFor(t *testing.T, spec mekoStackSpec) *mekoStack {
	t.Helper()
	return &mekoStack{
		stack: &stack{ns: "ycp-agent", owned: map[string]bool{}, pool: &dbPool{}},
		base:  "meko",
		key:   resourceKey("meko-stack", "meko"),
		spec:  spec.withDefaults(),
	}
}

// The stack is a YugabyteDB stack, not a postgres one: MAGE and dist_rag are
// YugabyteDB extensions, and everything Meko stores depends on them. An engine default
// of postgres here would produce a database that comes up and then cannot install a
// single extension the schema needs.
func TestMekoStackDefaultsToYugabyteWithMage(t *testing.T) {
	spec := mekoStackSpec{Images: fullRelease}.withDefaults()

	if spec.Database.engine() != "yugabytedb" {
		t.Errorf("engine = %q, want yugabytedb", spec.Database.engine())
	}
	if spec.Database.Image != fullRelease.Database {
		t.Errorf("database image = %q, want images.database", spec.Database.Image)
	}
	if spec.Database.Port != 5433 || spec.Database.User != "yugabyte" {
		t.Errorf("YSQL defaults wrong: port=%d user=%s", spec.Database.Port, spec.Database.User)
	}
	// The gflag is not optional: without it the mage extension will not install, and
	// it can only be set when the processes start.
	for name, flags := range map[string]map[string]string{
		"master": spec.Database.MasterFlags, "tserver": spec.Database.TServerFlags,
	} {
		if flags["ysql_yb_enable_mage"] != "true" {
			t.Errorf("%s flags = %v, want ysql_yb_enable_mage=true", name, flags)
		}
	}
	if spec.DatabaseName != mekoDatabase {
		t.Errorf("database name = %q, want %q", spec.DatabaseName, mekoDatabase)
	}
	if got := *spec.Extensions; len(got) != 2 || got[0] != "mage" || got[1] != "pg_dist_rag" {
		t.Errorf("extensions = %v, want [mage pg_dist_rag]", got)
	}
}

// Declared gflags replace the default set rather than merging with it. Merging would
// mean an operator cannot turn MAGE off, and a flag you cannot unset is worse than one
// you have to remember to set.
func TestMekoStackDeclaredGflagsWin(t *testing.T) {
	spec := mekoStackSpec{
		Images:   fullRelease,
		Database: databaseSpec{TServerFlags: map[string]string{"ysql_log_statement": "all"}},
	}.withDefaults()

	if _, ok := spec.Database.TServerFlags["ysql_yb_enable_mage"]; ok {
		t.Error("the default gflag was merged into a declared tserver_flags set")
	}
	// The other list is untouched, so a partial override is still a partial override.
	if spec.Database.MasterFlags["ysql_yb_enable_mage"] != "true" {
		t.Error("declaring tserver_flags should not clear the master default")
	}
}

// An explicit empty extension list installs nothing — for an external database whose
// owner manages its own extensions, where running DDL would be overreach.
func TestMekoStackExtensionsCanBeEmptied(t *testing.T) {
	none := []string{}
	spec := mekoStackSpec{Images: fullRelease, Extensions: &none}.withDefaults()
	if got := *spec.Extensions; len(got) != 0 {
		t.Errorf("extensions = %v, want none", got)
	}
}

// The MAGE-enabled build is a private image with no public equivalent, so there is no
// tag worth guessing: falling back to the public yugabytedb image would give a database
// that starts perfectly and then fails every CREATE EXTENSION.
func TestMekoStackRequiresADatabaseImage(t *testing.T) {
	m := mekoStackFor(t, mekoStackSpec{Images: stackImages{APIServer: "x:1"}})
	err := m.applyDatabase()
	if err == nil || !strings.Contains(err.Error(), "images.database") {
		t.Fatalf("err = %v, want one naming images.database", err)
	}
	// And it reports a component rather than silently omitting one: an environment with
	// an unusable spec must not read as an environment with nothing declared.
	if len(m.components) != 1 || m.components[0].name != compDatabase {
		t.Errorf("components = %+v, want one failed %s row", m.components, compDatabase)
	}
}

// Every component names the image it is missing, because "needs an image" without
// saying which field is a support round-trip.
func TestMekoStackComponentsNameTheirMissingImage(t *testing.T) {
	m := mekoStackFor(t, mekoStackSpec{Images: stackImages{Database: "db:1"}})
	m.db = &resolvedDB{host: "h", port: 5433, user: "yugabyte", database: "yugabyte"}

	for field, apply := range map[string]func() error{
		"images.api_server":        m.applyAPIServer,
		"images.mcp_server":        m.applyMCPServer,
		"images.rag_worker":        m.applyRAGWorker,
		"images.inference_gateway": m.applyGateway,
		"images.ui":                m.applyUI,
	} {
		err := apply()
		if err == nil || !strings.Contains(err.Error(), strings.TrimPrefix(field, "images.")) {
			t.Errorf("%s missing: err = %v, want one naming the field", field, err)
		}
	}
}

// The api-server waits for its database instead of crash-looping at it.
//
// api_server pings five times over fifteen seconds and then exits; a yugabyted takes
// about ninety seconds to come up. Without a gate the pod spends its restarts while the
// database is still starting and lands in a five-minute CrashLoopBackOff — measured on
// kind, it was still backing off eleven minutes in, long after the database was serving.
func TestAPIServerWaitsForItsDatabase(t *testing.T) {
	got := waitForPortScript("meko-yugabytedb.ycp-agent.svc.cluster.local.", 5433)
	for _, want := range []string{
		"until ", "nc -z meko-yugabytedb.ycp-agent.svc.cluster.local. 5433", "sleep 3",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("wait script %q is missing %q", got, want)
		}
	}

	// And deployment() has to actually carry an init container into the pod spec — a
	// silently dropped one puts the gate back to nothing.
	dep := deployment(deploymentOpts{
		name: "meko-api-server", ns: "ycp-agent", image: fullRelease.APIServer,
		initContainers: []any{map[string]any{"name": "wait-for-database"}},
	})
	tmpl := dep["spec"].(map[string]any)["template"].(map[string]any)
	if _, ok := tmpl["spec"].(map[string]any)["initContainers"]; !ok {
		t.Error("deployment() dropped the initContainers it was given")
	}
	// Absent by default, so no other workload grows an empty initContainers list.
	plain := deployment(deploymentOpts{name: "x", ns: "ycp-agent", image: "x:1"})
	plainSpec := plain["spec"].(map[string]any)["template"].(map[string]any)["spec"].(map[string]any)
	if _, ok := plainSpec["initContainers"]; ok {
		t.Error("deployment() added an initContainers key when none were given")
	}
}

// The whole stack carries ONE resource key, which is what makes it a unit: withdrawing
// the meko-stack resource garbage-collects every part, and a single component failing
// to apply cannot make the others look undesired.
func TestMekoStackComponentsShareOneResourceKey(t *testing.T) {
	m := mekoStackFor(t, mekoStackSpec{Images: fullRelease})
	want := resourceKey("meko-stack", "meko")
	for _, c := range []string{compDatabase, compAPI, compMCP, compRAG, compGateway, compUI} {
		if got := m.componentLabels(c)[stackResourceLabel]; got != want {
			t.Errorf("%s labelled %q, want the stack's key %q", c, got, want)
		}
	}
}

// The delegated reconcilers label their objects with the STACK's key, not with a key
// derived from the resource name they are called with. Without that the GC would see
// `mcp-server.meko-mcp-server`, find it undeclared, and delete a running MCP server on
// the very tick that created it.
func TestDelegatedReconcilersUseTheStackKey(t *testing.T) {
	m := mekoStackFor(t, mekoStackSpec{Images: fullRelease})
	stackKey := resourceKey("meko-stack", "meko")

	// keyFor is what the single-workload reconcilers call. Outside a stack it is the
	// resource's own key; lent the stack's ownerKey, it is the stack's.
	if got := m.stack.keyFor("mcp-server", "meko-mcp-server"); got != "mcp-server.mekomcpserver" {
		t.Errorf("keyFor outside a stack = %q, want the resource's own key", got)
	}
	m.stack.ownerKey = m.key
	if got := m.stack.keyFor("mcp-server", "meko-mcp-server"); got != stackKey {
		t.Errorf("keyFor inside a stack = %q, want %q", got, stackKey)
	}
}

// Delegation must restore what it borrowed. Leaving the stack's database or resource
// key behind would re-point — or mislabel — a separately declared resource reconciled
// later in the same tick.
func TestDelegationRestoresBorrowedState(t *testing.T) {
	m := mekoStackFor(t, mekoStackSpec{Images: fullRelease})
	outer := &resolvedDB{host: "someone-elses-db"}
	m.stack.db = outer
	m.db = &resolvedDB{host: "the-stacks-db"}

	// The reconciler fails, which is the point: the deferred restore has to happen on
	// the failure path too, and that is the path a real tick takes when the API server
	// rejects an apply.
	failing := func(*stack, api.DesiredResource) error { return errStub }
	if err := m.delegate(compMCP, fullRelease.MCPServer, portMCP, failing); err != errStub {
		t.Fatalf("delegate err = %v, want the reconciler's own error", err)
	}

	if m.stack.db != outer {
		t.Errorf("stack.db = %+v after delegation, want the database it had before", m.stack.db)
	}
	if m.stack.ownerKey != "" {
		t.Errorf("ownerKey = %q after delegation, want it cleared", m.stack.ownerKey)
	}
}

// Components can be left out without hand-writing a resource set: an environment that
// needs memory and no web UI is a reasonable thing to ask for.
func TestMekoStackDisable(t *testing.T) {
	m := mekoStackFor(t, mekoStackSpec{Images: fullRelease, Disable: []string{"ui", " Inference-Gateway "}})
	for _, c := range []string{compUI, compGateway} {
		if m.enabled(c) {
			t.Errorf("%s is disabled but reads as enabled", c)
		}
	}
	for _, c := range []string{compAPI, compMCP, compRAG} {
		if !m.enabled(c) {
			t.Errorf("%s was disabled by accident", c)
		}
	}
}

// Each component's in-cluster address has to be absolute — the trailing dot on a
// four-dot in-cluster name is what keeps a resolver from walking the ndots search list
// and stalling. See svcFQDN.
func TestMekoStackAddressesAreAbsolute(t *testing.T) {
	m := mekoStackFor(t, mekoStackSpec{Images: fullRelease})
	got := m.addr(compAPI, portAPI)
	if got != "http://meko-api-server.ycp-agent.svc.cluster.local.:25433" {
		t.Fatalf("addr = %q", got)
	}
}

// yugabyted has to be told to start; the image's entrypoint runs no database at all.
// And it has to be told to advertise a stable name, because it records the address it
// started with — a pod that came back on a new IP would not recognise its own data.
func TestYugabytedCommand(t *testing.T) {
	spec := databaseSpec{
		Engine:       "yugabytedb",
		MasterFlags:  map[string]string{"ysql_yb_enable_mage": "true"},
		TServerFlags: map[string]string{"b": "2", "a": "1"},
	}.withDefaults()
	command, args := yugabytedCommand(spec, "meko-yugabytedb", "ycp-agent")

	if len(command) == 0 || command[0] != "/bin/bash" || len(args) != 1 {
		t.Fatalf("command/args = %v %v, want a single bash script", command, args)
	}
	script := args[0]
	for _, want := range []string{
		"bin/yugabyted start",
		"--base_dir=" + ybBaseDir,
		`--advertise_address="$ADDR"`,
		"${HOSTNAME}.meko-yugabytedb.ycp-agent.svc.cluster.local",
		"--master_flags=ysql_yb_enable_mage=true",
		// Sorted, so the rendered pod template is stable across ticks.
		"--tserver_flags=a=1,b=2",
		// The name must resolve before yugabyted binds it, and it exits rather than
		// retries if it does not.
		"until getent hosts",
	} {
		if !strings.Contains(script, want) {
			t.Errorf("script missing %q:\n%s", want, script)
		}
	}
	// ${HOSTNAME} and not $(HOSTNAME): Kubernetes expands the $(VAR) form itself in
	// command and args, and would replace it with an empty string.
	if strings.Contains(script, "$(HOSTNAME)") {
		t.Error("used $(HOSTNAME), which Kubernetes expands before the shell sees it")
	}
}

// The gflag render has to be deterministic. Go randomises map iteration, so an
// unsorted render would rewrite the StatefulSet — and restart the database — on
// roughly every tick.
func TestGflagArgsAreStable(t *testing.T) {
	spec := databaseSpec{MasterFlags: map[string]string{"c": "3", "a": "1", "b": "2"}}
	first := gflagArgs(spec)
	for i := 0; i < 20; i++ {
		if got := gflagArgs(spec); got[0] != first[0] {
			t.Fatalf("gflagArgs is not stable: %v then %v", first, got)
		}
	}
	if first[0] != "--master_flags=a=1,b=2,c=3" {
		t.Errorf("gflagArgs = %q", first[0])
	}
	// No flags means no argument at all, not an empty one that yugabyted would reject.
	if got := gflagArgs(databaseSpec{}); len(got) != 0 {
		t.Errorf("gflagArgs with no flags = %v, want none", got)
	}
}

// Setting YSQL_PASSWORD makes yugabyted enable password auth, so the readiness probe's
// own ysqlsh needs the password too. Without PGPASSWORD the probe fails with "no
// password supplied" on every attempt and a perfectly healthy database never reports
// ready — verified against ybdb-meko-release:2026.1.0.0-b53.
func TestDatabaseEnvGivesTheProbeAPassword(t *testing.T) {
	spec := databaseSpec{Engine: "yugabytedb"}.withDefaults()
	var names []string
	for _, e := range databaseEnv(spec) {
		names = append(names, e.(map[string]any)["name"].(string))
	}
	for _, want := range []string{"YSQL_PASSWORD", "PGPASSWORD"} {
		if !contains(names, want) {
			t.Errorf("yugabytedb env = %v, missing %s", names, want)
		}
	}

	pg := databaseSpec{}.withDefaults()
	names = nil
	for _, e := range databaseEnv(pg) {
		names = append(names, e.(map[string]any)["name"].(string))
	}
	if !contains(names, "POSTGRES_PASSWORD") || !contains(names, "PGDATA") {
		t.Errorf("postgres env = %v", names)
	}
}

// An extension name is interpolated into a CREATE EXTENSION, since that statement takes
// an identifier and not a bind parameter. So anything that is not a plain identifier has
// to be refused before it gets there.
func TestExtensionNamesAreValidated(t *testing.T) {
	for _, ok := range []string{"mage", "pg_dist_rag", "vector", "a1_b2"} {
		if !validExtensionName.MatchString(ok) {
			t.Errorf("%q should be a valid extension name", ok)
		}
	}
	for _, bad := range []string{
		"", "1vector", "pg-dist-rag", "mage; DROP DATABASE meko", `mage"`, "MAGE", "pg_dist_rag--",
	} {
		if validExtensionName.MatchString(bad) {
			t.Errorf("%q must be refused as an extension name", bad)
		}
	}
}

// A meko-stack spec that says nothing beyond its images must decode and default
// cleanly: the control plane sends exactly that, and an unknown field from a newer
// control plane must be ignored rather than fatal.
func TestMekoStackSpecDecodes(t *testing.T) {
	raw := []byte(`{"images":{"database":"db:1","ui":"ui:1"},"secret_ref":"creds","future_field":42}`)
	var spec mekoStackSpec
	if err := decodeSpec(raw, &spec); err != nil {
		t.Fatalf("decode: %v", err)
	}
	spec = spec.withDefaults()
	if spec.Images.Database != "db:1" || spec.Images.UI != "ui:1" {
		t.Errorf("images = %+v", spec.Images)
	}
	if spec.SecretRef != "creds" {
		t.Errorf("secret_ref = %q", spec.SecretRef)
	}
	// The pull secret flows down to the database, which is in the same private registry
	// as everything else.
	spec2 := mekoStackSpec{Images: fullRelease, ImagePullSecret: "meko-ecr"}.withDefaults()
	if spec2.Database.ImagePullSecret != "meko-ecr" {
		t.Errorf("database pull secret = %q, want the stack's", spec2.Database.ImagePullSecret)
	}
}

// meko-stack is reconciled before the individual kinds, and it is a stack kind — an
// unregistered kind is skipped with a log line, which for the seeded resource would
// mean an agent that enrolls and then runs nothing.
func TestMekoStackIsAKindAndGoesFirst(t *testing.T) {
	if !stackKinds["meko-stack"] {
		t.Fatal("meko-stack is not registered as a stack kind")
	}
	ordered := inDependencyOrder([]api.DesiredResource{
		{Kind: "rag-worker", Name: "rag"},
		{Kind: "database", Name: "db"},
		{Kind: "meko-stack", Name: "meko"},
	})
	if ordered[0].Kind != "meko-stack" {
		t.Errorf("order = %s first, want meko-stack", ordered[0].Kind)
	}
}

// The delegated workloads get the stack's own api-server as MEKO_BASE_URL, not the
// SaaS default: a BYOC data plane that called back to the SaaS for its own metadata
// would be the opposite of the point.
func TestDelegatedWorkloadsPointAtTheStack(t *testing.T) {
	m := mekoStackFor(t, mekoStackSpec{Images: fullRelease, SecretRef: "creds", ImagePullSecret: "ecr"})
	m.db = &resolvedDB{host: "db", port: 5433}

	var got workloadSpec
	captured := false
	apply := func(s *stack, r api.DesiredResource) error {
		captured = true
		return json.Unmarshal(r.Spec, &got)
	}
	if err := m.delegate(compMCP, fullRelease.MCPServer, portMCP, apply); err != nil {
		t.Fatalf("delegate: %v", err)
	}
	if !captured {
		t.Fatal("delegate did not call the reconciler")
	}
	if got.MekoBaseURL != "http://meko-api-server.ycp-agent.svc.cluster.local.:25433/api/v1" {
		t.Errorf("meko_base_url = %q", got.MekoBaseURL)
	}
	if got.Image != fullRelease.MCPServer || got.Port != portMCP {
		t.Errorf("image/port = %q/%d", got.Image, got.Port)
	}
	if got.SecretRef != "creds" || got.ImagePullSecret != "ecr" {
		t.Errorf("Secret names not passed down: %+v", got)
	}
	if got.DatabaseName != mekoDatabase {
		t.Errorf("database_name = %q, want %q", got.DatabaseName, mekoDatabase)
	}
}

// The stack the agent enrolled with is what it deploys, every tick, with no
// control-plane call in the path. That is the property that makes bring-up independent
// of a desired-state command — and of a reachable control plane.
func TestEnrolledResources(t *testing.T) {
	stack := json.RawMessage(`{"images":{"database":"db:1"}}`)
	got := enrolledResources(stack)
	if len(got) != 1 {
		t.Fatalf("enrolledResources = %+v, want one resource", got)
	}
	if got[0].Kind != "meko-stack" || got[0].Name != enrolledStackName {
		t.Errorf("resource = %s/%s, want meko-stack/%s", got[0].Kind, got[0].Name, enrolledStackName)
	}
	if string(got[0].Spec) != string(stack) {
		t.Errorf("spec = %s, want the enrolled stack verbatim", got[0].Spec)
	}
	// The name has to be fixed, because it is the GC key and the object-name prefix: a
	// name that varied between ticks would have the agent delete and recreate the whole
	// data plane on every one.
	if enrolledResources(stack)[0].Name != got[0].Name {
		t.Error("the enrolled stack's name is not stable")
	}
	// No stack means no resources — a control plane running --no-bootstrap-defaults, or
	// one too old to send one. The environment then runs only what it is told to.
	if got := enrolledResources(nil); got != nil {
		t.Errorf("enrolledResources(nil) = %+v, want none", got)
	}
}

// The enrolled stack is a fallback, and an EMPTY desired state that was actually read
// has to beat it.
//
// `POST /desired` with `[]` is how an environment is told to run nothing. If the
// enrolled stack outranked a successfully-read desired state, that call would silently
// do nothing and the stack could never be withdrawn — while the GC, which claims
// ownership from the same list, would keep every object alive forever.
//
// This mirrors the precedence in main's loop, which is where the two are combined:
// desired state when it could be read, the enrolled stack when it could not.
func TestDesiredStateOutranksTheEnrolledStack(t *testing.T) {
	enrolled := enrolledResources(json.RawMessage(`{"images":{"ui":"pinned:1"}}`))
	if len(enrolled) != 1 {
		t.Fatalf("enrolled = %+v", enrolled)
	}

	stack := json.RawMessage(`{"images":{"ui":"pinned:1"}}`)

	// Read, and empty: nothing runs. The fallback must not resurrect the stack.
	resources := resourcesToApply(true, []api.DesiredResource{}, nil, stack)
	if len(resources) != 0 {
		t.Errorf("an explicitly empty desired state gave %+v, want nothing", resources)
	}

	// Read, with an operator's own stack: theirs, exactly — not blended with the pinned
	// one, because a resource in desired state replaces rather than merges.
	own := []api.DesiredResource{
		{Kind: "meko-stack", Name: enrolledStackName, Spec: json.RawMessage(`{"images":{"ui":"mine:2"}}`)},
	}
	resources = resourcesToApply(true, own, nil, stack)
	if len(resources) != 1 || string(resources[0].Spec) != `{"images":{"ui":"mine:2"}}` {
		t.Errorf("resources = %+v, want the operator's replacement", resources)
	}

	// NOT read, and nothing read before: the enrolled stack stands, which is what brings
	// a fresh data plane up without the control plane. Note the desired state passed in
	// is the zero value from a failed decode, which must not be read as "run nothing".
	resources = resourcesToApply(false, nil, nil, stack)
	if len(resources) != 1 || string(resources[0].Spec) != `{"images":{"ui":"pinned:1"}}` {
		t.Errorf("resources = %+v, want the enrolled stack", resources)
	}
	if resources[0].Kind != "meko-stack" || resources[0].Name != enrolledStackName {
		t.Errorf("fallback resource = %s/%s", resources[0].Kind, resources[0].Name)
	}
}

// An outage must not rewind the data plane to what the environment was registered with.
//
// The case that matters: an operator withdraws the stack (or edits it), the agent reads
// that, and then the control plane goes away. Falling back to the ENROLLED stack there
// would silently redeploy everything they just removed — the enrolled copy is a
// bootstrap value, not a standing instruction, and it stops being the newest thing the
// agent knows the moment it reads anything.
func TestOutageHoldsTheLastReadStateNotTheEnrolledOne(t *testing.T) {
	stack := json.RawMessage(`{"images":{"ui":"pinned:1"}}`)

	// The operator withdrew everything, and the agent read that.
	withdrawn := []api.DesiredResource{}
	if got := resourcesToApply(false, nil, withdrawn, stack); len(got) != 0 {
		t.Errorf("after an outage the agent applied %+v, want nothing — it was told to run nothing", got)
	}

	// Or they replaced it, and the agent read that.
	edited := []api.DesiredResource{
		{Kind: "meko-stack", Name: enrolledStackName, Spec: json.RawMessage(`{"images":{"ui":"mine:2"}}`)},
	}
	got := resourcesToApply(false, nil, edited, stack)
	if len(got) != 1 || string(got[0].Spec) != `{"images":{"ui":"mine:2"}}` {
		t.Errorf("after an outage the agent applied %+v, want the last state it read", got)
	}
}

func contains(haystack []string, needle string) bool {
	for _, v := range haystack {
		if v == needle {
			return true
		}
	}
	return false
}

// An image reference the kubelet cannot parse must be refused by the agent, not applied.
//
// Applied, it becomes a pod in InvalidImageName and a component stuck at 0/1 with no
// reason on it — the operator has to go read pod status to discover the spec was junk.
// This is the same class of failure ycp/STATUS.md records against the old console's
// stack template, and it happened again for real: a documented example containing a `…`
// placeholder, pasted verbatim into POST /desired.
func TestCheckImage(t *testing.T) {
	valid := []string{
		"161908593886.dkr.ecr.us-east-2.amazonaws.com/meko/meko-ui:1.0.0-20260729-bf56db7",
		"pgvector/pgvector:pg16",
		"registry:5000/meko/mcp:1.2.3",
		"meko/mcp@sha256:abc123",
		"postgres",
	}
	for _, image := range valid {
		if err := checkImage("images.ui", image); err != nil {
			t.Errorf("checkImage(%q) = %v, want nil", image, err)
		}
	}

	// The one that actually happened, plus the other placeholder shapes that show up in
	// copied examples.
	invalid := []string{
		"…/meko-ui:1.0.0",
		"<registry>/meko/meko-ui:1.0.0",
		"161908593886.dkr.ecr.us-east-2.amazonaws.com/meko/meko-ui:<tag>",
		"meko/meko ui:1.0",
		"meko/ui:1.0\n",
	}
	for _, image := range invalid {
		err := checkImage("images.ui", image)
		if err == nil {
			t.Errorf("checkImage(%q) = nil, want it refused before it reaches a pod spec", image)
			continue
		}
		// The message has to name the field, or an operator with six images in a spec
		// cannot tell which one is wrong.
		if !strings.Contains(err.Error(), "images.ui") {
			t.Errorf("checkImage(%q) error does not name the field: %v", image, err)
		}
	}
}

// And it has to be wired into every component, not just defined. A validator nothing
// calls is the same as no validator.
func TestEveryComponentValidatesItsImage(t *testing.T) {
	bad := "…/broken:1"
	m := mekoStackFor(t, mekoStackSpec{Images: stackImages{
		Database: bad, APIServer: bad, MCPServer: bad, RAGWorker: bad,
		InferenceGateway: bad, UI: bad,
	}})
	m.db = &resolvedDB{host: "h", port: 5433, user: "yugabyte", database: "yugabyte"}

	for name, apply := range map[string]func() error{
		compDatabase: m.applyDatabase,
		compAPI:      m.applyAPIServer,
		compMCP:      m.applyMCPServer,
		compRAG:      m.applyRAGWorker,
		compGateway:  m.applyGateway,
		compUI:       m.applyUI,
	} {
		err := apply()
		if err == nil || !strings.Contains(err.Error(), "not a valid image reference") {
			t.Errorf("%s accepted %q: err = %v", name, bad, err)
		}
	}
}

// The fallback has to survive a restart, or it only covers outages shorter than the
// pod's lifetime.
//
// This is why: an agent ran for two and a half days after its control plane's namespace
// was deleted, holding six components up on an in-memory copy of the last desired state
// it read. One eviction in that window and it would have fallen back to the ENROLLED
// stack instead — reverting the environment to what it was registered with, discarding
// every edit since.
func TestResumeDesired(t *testing.T) {
	// A real persisted value round-trips.
	raw := json.RawMessage(`[{"kind":"meko-stack","name":"meko","spec":{"images":{"ui":"u:1"}}}]`)
	lastGood, persisted := resumeDesired(raw)
	if len(lastGood) != 1 || lastGood[0].Kind != "meko-stack" {
		t.Fatalf("lastGood = %+v, want the persisted resource", lastGood)
	}
	if string(persisted) != string(raw) {
		t.Errorf("persisted = %s, want the bytes back for change comparison", persisted)
	}

	// "Told to run nothing" has to survive as an EMPTY list, not as nil — nil falls
	// through to the enrolled stack, which would resurrect a withdrawn stack on restart.
	lastGood, persisted = resumeDesired(json.RawMessage(`[]`))
	if lastGood == nil {
		t.Error("an empty persisted list came back as nil, which means 'never read anything'")
	}
	if len(lastGood) != 0 {
		t.Errorf("lastGood = %+v, want empty", lastGood)
	}
	if len(persisted) == 0 {
		t.Error("persisted bytes lost for the empty case")
	}
	// `null` decodes cleanly into a nil slice, so it needs the same normalisation.
	if got, _ := resumeDesired(json.RawMessage(`null`)); got == nil {
		t.Error("a persisted null came back as nil; it represents an empty list")
	}

	// Nothing persisted, and garbage, both mean "never read anything" — fall through to
	// the enrolled stack rather than trust a partial decode.
	for _, in := range []json.RawMessage{nil, {}, json.RawMessage(`{oh no`)} {
		if got, p := resumeDesired(in); got != nil || p != nil {
			t.Errorf("resumeDesired(%q) = (%+v, %s), want (nil, nil)", in, got, p)
		}
	}
}

// The api-server needs a migrate step, and it has to run after the wait.
//
// api_server does NOT migrate on boot — only when -migrate-only is passed. Without this
// the whole stack comes up green, answers /api/v1/health with 200, and fails every real
// request with `relation "meko_system.users" does not exist`: a data plane that looks
// perfect and cannot serve a call. Found by opening the UI, which loaded and then 401d
// on everything.
func TestAPIServerMigratesBeforeItServes(t *testing.T) {
	m := mekoStackFor(t, mekoStackSpec{Images: fullRelease})
	m.db = &resolvedDB{
		host: "meko-yugabytedb.ycp-agent.svc.cluster.local.", port: 5433,
		user: "yugabyte", secretRef: dbSecret, passwordKey: dbPasswordKey,
	}
	dep := deployment(deploymentOpts{
		name: "meko-api-server", ns: m.ns, image: fullRelease.APIServer,
		initContainers: m.apiServerInitContainers(
			[]string{"-db-name=meko", "-db-host=" + m.db.host, "-db-password=$(DB_PASSWORD)"},
			[]any{map[string]any{"name": "DB_PASSWORD"}}, nil),
		args: []string{"-port=25433"},
	})
	spec := dep["spec"].(map[string]any)["template"].(map[string]any)["spec"].(map[string]any)
	inits, ok := spec["initContainers"].([]any)
	if !ok || len(inits) != 2 {
		t.Fatalf("initContainers = %v, want the wait and the migrate step", spec["initContainers"])
	}
	// Order matters: migrating before the database accepts connections just fails.
	first := inits[0].(map[string]any)["name"].(string)
	second := inits[1].(map[string]any)["name"].(string)
	if first != "wait-for-database" || second != "migrate" {
		t.Errorf("init order = %s then %s, want wait-for-database then migrate", first, second)
	}

	migrate := inits[1].(map[string]any)
	args, _ := migrate["args"].([]string)
	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "-migrate-only") {
		t.Errorf("migrate args = %q, want -migrate-only — without it api_server serves and never migrates", joined)
	}
	// It has to reach the same database as the server, or it builds a schema nobody uses.
	for _, want := range []string{"-db-name=meko", "-db-host=" + m.db.host, "-db-password=$(DB_PASSWORD)"} {
		if !strings.Contains(joined, want) {
			t.Errorf("migrate args %q missing %q", joined, want)
		}
	}
	// And the password by reference, never a literal.
	if migrate["env"] == nil {
		t.Error("the migrate step has no env, so $(DB_PASSWORD) expands to nothing")
	}
}

// Exposure is opt-in, and internal must not silently come up public.
//
// On GKE the difference between a VPC-only address and one the whole internet can reach
// is a single annotation, and the thing behind this Service is an api-server that does
// not authenticate its callers when Cognito is unset. So: no exposure by default, and
// "internal" has to actually carry the annotations that confine it.
func TestServiceExposure(t *testing.T) {
	m := mekoStackFor(t, mekoStackSpec{
		Images: fullRelease,
		Expose: map[string]string{compUI: "Internal", compGateway: "external"},
	})

	// Case-insensitive and trimmed, because these come from hand-written JSON.
	if got := m.exposure(compUI); got != "internal" {
		t.Errorf("exposure(ui) = %q, want internal", got)
	}
	if got := m.exposure(compGateway); got != "external" {
		t.Errorf("exposure(gateway) = %q, want external", got)
	}
	// Not mentioned means ClusterIP. A component that quietly gained a LoadBalancer
	// because something defaulted would be the whole problem.
	if got := m.exposure(compAPI); got != "" {
		t.Errorf("exposure(api-server) = %q, want empty — exposure is opt-in", got)
	}

	// An unknown value is refused rather than treated as one of the two. Guessing here
	// is how "internl" becomes a public address.
	s := &stack{ns: "ycp-agent", owned: map[string]bool{}}
	err := s.applyExposedService("x", nil, 80, "http", "publik")
	if err == nil || !strings.Contains(err.Error(), "not a kind of exposure") {
		t.Errorf("err = %v, want a refusal naming the valid values", err)
	}
}
