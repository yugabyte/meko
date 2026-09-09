// The reconciler for kind "meko-stack": one desired resource that brings the whole
// Meko data plane up in the agent's namespace.
//
// # Why one resource and not six
//
// The pieces are not independently useful. A YugabyteDB with the MAGE and dist_rag
// extensions, an api-server whose migrations reference `dist_rag.vector_indexes`, an
// MCP server and a RAG worker over the same database, a gateway pointed at that MCP
// server and a UI pointed at that api-server — there is no combination of half of
// these that anybody wants running. Declaring them separately would mean six specs
// that all have to agree about a database, a registry, a pull secret and a credentials
// Secret, and six chances for them to disagree. The control plane resolves one release
// and sends one resource; withdrawing it takes the environment down as a unit.
//
// # The order things have to happen in
//
// One ordering constraint here is real rather than aesthetic: the extensions have to
// exist before the api-server's schema is built. Its data migrations reference
// `dist_rag.vector_indexes` and the `dist_rag` enum types, so migrating against a
// database without pg_dist_rag fails. So: database, then CREATE DATABASE, then
// CREATE EXTENSION, then everything else.
//
// And the schema has to be built at all. `api_server` does NOT migrate on boot — it
// migrates only when `-migrate-only` is passed, and otherwise serves against whatever
// happens to be there. So the api-server gets a `migrate` init container, exactly as the
// control plane has always had. Without it the whole stack comes up green, answers
// /api/v1/health with 200, and fails every real request with
// `relation "meko_system.users" does not exist`.
//
// Everything else is deployed regardless of whether that succeeded, for the reason the
// mcp-server reconciler already does it: a workload that is declared and retrying is
// visible, and an empty namespace while the agent waits for a database to accept
// connections is not. Kubernetes restarts what fails, and a component row carries the
// reason.
package main

import (
	"encoding/json"
	"fmt"
	"log"
	"regexp"
	"strings"

	"meko/ycp/internal/api"
)

// The components of the stack. The value is both the object-name suffix and the name
// the component reports itself as upstream, so a console row and a `kubectl get`
// line say the same word.
const (
	compDatabase = "yugabytedb"
	compAPI      = "api-server"
	compMCP      = "mcp-server"
	compRAG      = "rag-worker"
	compGateway  = "inference-gateway"
	compUI       = "ui"
)

// Ports each component listens on. Not configurable per component: these are what the
// images' own defaults are, and what everything else in the stack is wired to expect.
const (
	portAPI     = 25433
	portMCP     = 8000
	portGateway = 8088
	portUI      = 5000
)

// mageExtensions are the SQL extensions the Meko schema needs, and the reason this
// reconciler talks to the database at all.
//
//	mage         the MAGE graph extension. Also needs the ysql_yb_enable_mage gflag
//	             below — the extension will not install without it.
//	pg_dist_rag  distributed RAG: the dist_rag schema api_server's migrations and the
//	             RAG worker both write to. Pulls in `vector` via CASCADE, which mem0
//	             needs for embeddings.
var mageExtensions = []string{"mage", "pg_dist_rag"}

// mageGflags turns MAGE on in the database processes. It is a start-time gflag on both
// the master and the tserver, not something a later SQL statement can enable.
var mageGflags = map[string]string{"ysql_yb_enable_mage": "true"}

// validExtensionName matches what may be interpolated into a CREATE EXTENSION.
var validExtensionName = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)

// imageRefChars is what a Docker image reference may be built from: the registry host,
// path, tag and digest alphabet, and nothing else. Deliberately a character class
// rather than a full grammar — the point is to catch a reference that is obviously not
// one, not to re-implement the distribution spec.
var imageRefChars = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:/@-]*$`)

// checkImage rejects an image reference Kubernetes could not parse.
//
// Without this the agent happily applies the workload and the kubelet reports
// InvalidImageName, which surfaces as a component stuck at 0/1 with no reason attached —
// the operator has to go and read pod status to find out that the spec was junk. Since
// the whole reason the private images have no defaults is "a bad spec should fail as a
// bad spec, immediately, rather than as a pull failure minutes later", a reference that
// cannot even be parsed should fail soonest of all.
//
// The case this was written for: a documented example containing a `…` placeholder,
// pasted verbatim. ycp/STATUS.md records the same thing sinking the old console's stack
// template — placeholders that reach a pod spec are a recurring failure here, not a
// hypothetical one.
func checkImage(field, image string) error {
	if strings.ContainsAny(image, " \t\n") {
		return fmt.Errorf("%s = %q contains whitespace, which is not a valid image reference", field, image)
	}
	if !imageRefChars.MatchString(image) {
		return fmt.Errorf("%s = %q is not a valid image reference — it has characters an image name cannot "+
			"contain. A copied example with a placeholder left in it (…, <registry>, <tag>) is the usual cause; "+
			"the reference has to be the full one, e.g. %s/meko/meko-ui:1.0.0", field, image, ecrRegistry)
	}
	return nil
}

// mekoStackSpec is kind "meko-stack". Every field is optional except images.database,
// and the control plane fills all of them in — a caller who registers an environment
// supplies a name and nothing else.
type mekoStackSpec struct {
	// Images are resolved by the control plane, which is what decides the release a
	// data plane runs. The agent does not guess: the Meko images are private, so a
	// guessed tag fails minutes later as ImagePullBackOff rather than immediately as a
	// bad spec.
	Images stackImages `json:"images"`

	// ImagePullSecret and SecretRef are Secret NAMES in the agent's namespace, never
	// values — the pull credentials for the private registry, and the operator's own
	// Secret holding OPENAI_API_KEY, the AWS keys and the Cognito client secret. Both
	// are mounted by reference, so the control plane never learns either.
	ImagePullSecret string `json:"image_pull_secret"`
	SecretRef       string `json:"secret_ref"`

	// Database configures the stack's YugabyteDB, or points at one the operator
	// already runs (`{"mode":"external","host":…}`). The engine defaults to
	// yugabytedb here rather than postgres: MAGE and dist_rag are YugabyteDB
	// extensions, and the whole stack is built on them.
	Database databaseSpec `json:"database"`
	// DatabaseName is the database inside it that Meko lives in. The agent creates it.
	DatabaseName string `json:"database_name"`

	// Extensions overrides what is installed into that database. An explicit empty
	// array installs nothing — for an external database whose owner manages its own
	// extensions, where the agent's role is to connect rather than to run DDL.
	Extensions *[]string `json:"extensions"`

	// Disable names components to leave out, by the constants above. The rest of the
	// stack still comes up: an environment that only needs memory and no web UI is a
	// reasonable thing to ask for, and it should not need a hand-written resource set.
	Disable []string `json:"disable"`

	// Expose puts a LoadBalancer in front of a component instead of the default
	// ClusterIP — `{"ui": "internal"}` or `{"ui": "external"}`, keyed by the component
	// names above.
	//
	// The two are not interchangeable and the difference is the whole point:
	//
	//	internal   an address only reachable inside the VPC. What the YCP control plane
	//	           itself uses. Needs a VPN or a bastion to reach from a laptop, which
	//	           is why port-forwarding is still the usual way in.
	//	external   a public address. Anything that can route to the internet can reach
	//	           this component, so it is only ever right for a component that
	//	           authenticates its own callers — and Meko's api-server does not when
	//	           Cognito is unset, which is the default here.
	//
	// Absent, a component keeps its ClusterIP and is reachable only in-cluster. That is
	// the right default: an operator who wants an address off-cluster should have to say
	// which kind.
	Expose map[string]string `json:"expose"`

	// PublicBaseURL is the address a browser reaches this data plane on, when it has
	// one. It is what the gateway must allow as a CORS origin — without it a UI served
	// from the data plane calls a gateway that refuses the request, which surfaces as
	// a chat that silently never answers.
	PublicBaseURL string `json:"public_base_url"`

	LangfuseBaseURL string `json:"langfuse_base_url"`
	AWSRegion       string `json:"aws_region"`
	S3Bucket        string `json:"s3_bucket"`
	LogLevel        string `json:"log_level"`
}

// stackImages is one image reference per component, as resolved upstream.
type stackImages struct {
	Database         string `json:"database"`
	APIServer        string `json:"api_server"`
	MCPServer        string `json:"mcp_server"`
	RAGWorker        string `json:"rag_worker"`
	InferenceGateway string `json:"inference_gateway"`
	UI               string `json:"ui"`
}

func (s mekoStackSpec) withDefaults() mekoStackSpec {
	if s.Database.Engine == "" {
		s.Database.Engine = "yugabytedb"
	}
	if s.Database.Image == "" {
		s.Database.Image = s.Images.Database
	}
	if s.Database.ImagePullSecret == "" {
		s.Database.ImagePullSecret = s.ImagePullSecret
	}
	// The gflag that MAGE needs, unless the caller declared its own flags — in which
	// case it owns the whole set, since silently adding to a declared list is how an
	// operator ends up unable to turn something off.
	if s.Database.MasterFlags == nil {
		s.Database.MasterFlags = mageGflags
	}
	if s.Database.TServerFlags == nil {
		s.Database.TServerFlags = mageGflags
	}
	s.Database = s.Database.withDefaults()
	if s.DatabaseName == "" {
		s.DatabaseName = mekoDatabase
	}
	if s.Extensions == nil {
		exts := mageExtensions
		s.Extensions = &exts
	}
	if s.AWSRegion == "" {
		s.AWSRegion = "us-east-1"
	}
	if s.LogLevel == "" {
		s.LogLevel = "INFO"
	}
	return s
}

// enrolledStackName is what the stack that came with enrollment is called. Fixed
// rather than derived from the environment name: it is the resource key the GC matches
// on and the prefix on every object name, so it has to be the same string on every
// tick and after every restart.
const enrolledStackName = "meko"

// enrolledResources is what the agent deploys from what it enrolled with: the stack,
// as a resource the ordinary reconcile path can take.
//
// This is what removes the control plane from the bring-up path. The stack arrives with
// enrollment and is persisted, so a fresh agent's first tick and a restarted agent's
// first tick both deploy the whole data plane without a single successful request.
//
// It is a FALLBACK, not an override: a desired state the agent managed to read is
// authoritative, because that is where the same stack was written at registration and
// where an operator's changes go. Preferring the enrolled copy would mean an operator
// could never withdraw the stack — `POST /desired` with `[]` is how an environment is
// told to run nothing, and a fallback that outranked it would quietly ignore that.
//
// An agent with no enrolled stack starts from nothing, which is correct: that is a
// control plane running --no-bootstrap-defaults, one registered with an explicit
// `desired`, or one older than this feature.
func enrolledResources(stack json.RawMessage) []api.DesiredResource {
	if len(stack) == 0 {
		return nil
	}
	return []api.DesiredResource{{Kind: "meko-stack", Name: enrolledStackName, Spec: stack}}
}

// resumeDesired turns a persisted desired state into the fallback a fresh process starts
// with, and the bytes to compare future writes against.
//
// Unreadable persisted state yields (nil, nil) rather than an empty list, and that
// distinction is the whole point: nil means "never read anything", which falls through to
// the enrolled stack, while an empty list means "told to run nothing". Trusting a
// half-decoded value would be worse than either.
func resumeDesired(raw json.RawMessage) (lastGood []api.DesiredResource, persisted json.RawMessage) {
	if len(raw) == 0 {
		return nil, nil
	}
	if err := json.Unmarshal(raw, &lastGood); err != nil {
		log.Printf("persisted desired state is unreadable (%v) — falling back to the enrolled stack", err)
		return nil, nil
	}
	// An explicit `null` decodes without error and leaves the slice nil, which would
	// silently mean "never read anything". Normalise it to the empty list it represents.
	if lastGood == nil {
		lastGood = []api.DesiredResource{}
	}
	return lastGood, raw
}

// resourcesToApply is what one tick reconciles, in order of how much the agent trusts
// what it knows:
//
//  1. the desired state it just read, if it could read it;
//  2. otherwise the last desired state it DID read;
//  3. otherwise the stack it enrolled with.
//
// desiredRead is the whole distinction at (1), and it is not the same as
// len(desired) > 0. A desired state that was read and is empty means "run nothing", and
// has to beat both fallbacks; one that could not be read means nothing at all, and must
// not be mistaken for the same instruction.
//
// (2) is what stops an outage rewinding the data plane. Without it, an operator who
// withdraws the stack — or edits it — and then loses the control plane would have the
// agent quietly redeploy whatever the environment was REGISTERED with, which by then can
// be arbitrarily stale. The enrolled stack is a bootstrap value, not a standing
// instruction, and it stops being the best thing the agent knows the moment it reads
// anything newer.
//
// lastGood is in memory, so an agent that restarts during an outage still falls back to
// the enrolled stack. That is the right answer for a fresh install and the wrong one for
// a long-withdrawn stack; closing it means persisting lastGood beside the token.
func resourcesToApply(desiredRead bool, desired, lastGood []api.DesiredResource, stack json.RawMessage) []api.DesiredResource {
	if desiredRead {
		return desired
	}
	if lastGood != nil {
		return lastGood
	}
	return enrolledResources(stack)
}

// mekoStack is one meko-stack resource mid-reconcile. It carries its own resolved
// database rather than writing to stack.db: a stack is self-contained, and clobbering
// the field a separately declared `database` resource resolved would silently
// re-point that resource's workloads.
type mekoStack struct {
	*stack
	base string // object-name prefix, from the resource name
	key  string // stackResourceLabel value, so GC treats the stack as one unit
	spec mekoStackSpec
	db   *resolvedDB
	// schemaErr is why the database or its extensions could not be created this tick.
	// Reported on the api-server's component row, because that is the component it
	// actually blocks: api_server's migrations reference the dist_rag schema, so an
	// api-server that is 0/1 next to a healthy database is almost always this.
	schemaErr error
}

// applyMekoStack reconciles the whole data plane from one declared resource.
func (s *stack) applyMekoStack(r api.DesiredResource) error {
	var spec mekoStackSpec
	if err := decodeSpec(r.Spec, &spec); err != nil {
		return fmt.Errorf("bad meko-stack spec: %w", err)
	}
	m := &mekoStack{
		stack: s,
		base:  baseNameOr(r.Name, "meko"),
		key:   resourceKey(r.Kind, r.Name),
		spec:  spec.withDefaults(),
	}
	return m.apply()
}

func (m *mekoStack) apply() error {
	// The database first, and its failure is reported but not fatal to the rest: a
	// workload that cannot reach a database yet is a pod retrying with a reason
	// attached, which is a far better state to debug than an empty namespace.
	dbErr := m.applyDatabase()
	if dbErr != nil {
		log.Printf("meko-stack %s: database: %v", m.base, dbErr)
	}

	// CREATE DATABASE, then the extensions. This IS a gate on deploying the
	// api-server, not only on its migrations.
	//
	// Deploying it anyway and letting it retry — which is right for every other
	// component — is wrong here, because its migration does not retry. The
	// wait-for-database init container gates on the TCP port, which yugabyted opens
	// before this function has installed pg_dist_rag, so the data migration can reach
	// a database without the dist_rag types it references. A migration that fails
	// part-way leaves golang-migrate's version DIRTY, and nothing recovers from that:
	// every later restart reads the flag and refuses, so the one component that
	// "retries" is the one that never comes back. Waiting a tick costs nothing by
	// comparison.
	//
	// The dirty latch has other ways in — anything that kills the migrator mid-run,
	// which on an autoscaling cluster includes the database being rescheduled out
	// from under it (meko#497). This gate closes the ordering hazard, not the latch.
	m.schemaErr = m.applySchema()
	if m.schemaErr != nil {
		m.pool.logOnce("meko-stack.schema."+m.base, fmt.Sprintf(
			"meko-stack %s: %v — the rest of the stack is deployed anyway and each "+
				"component retries; the api-server waits for the extensions",
			m.base, m.schemaErr))
	}

	// Every component records its own row, so a failure to apply one does not stop the
	// others: this is a stack, and five components running plus one broken is both more
	// useful and more diagnosable than nothing.
	var firstErr error
	for _, c := range []struct {
		name  string
		apply func() error
	}{
		{compAPI, m.applyAPIServer},
		{compMCP, m.applyMCPServer},
		{compRAG, m.applyRAGWorker},
		{compGateway, m.applyGateway},
		{compUI, m.applyUI},
	} {
		if !m.enabled(c.name) {
			continue
		}
		// The api-server is the one component whose failure does not heal, so it is
		// the one that waits. Recorded as a failed row rather than skipped silently,
		// so the console shows a component that is 0/1 for a stated reason instead of
		// one that has quietly vanished from the report.
		if c.name == compAPI && m.schemaErr != nil {
			m.fail(compAPI, fmt.Errorf("waiting for the database schema before migrating: %w", m.schemaErr))
			continue
		}
		if err := c.apply(); err != nil {
			log.Printf("meko-stack %s: %s: %v", m.base, c.name, err)
			if firstErr == nil {
				firstErr = err
			}
		}
	}
	if firstErr == nil {
		firstErr = dbErr
	}
	return firstErr
}

// enabled reports whether a component is part of this stack.
func (m *mekoStack) enabled(component string) bool {
	for _, d := range m.spec.Disable {
		if strings.EqualFold(strings.TrimSpace(d), component) {
			return false
		}
	}
	return true
}

// exposure is how a component should be published, from spec.expose. Empty means the
// default ClusterIP.
func (m *mekoStack) exposure(component string) string {
	return strings.ToLower(strings.TrimSpace(m.spec.Expose[component]))
}

// name is a component's object name: the resource's name and the component, so one
// namespace could hold two stacks without their Deployments colliding.
func (m *mekoStack) name(component string) string { return m.base + "-" + component }

// addr is the in-cluster URL of a component of this stack, absolute (see svcFQDN for
// why the trailing dot is load-bearing).
func (m *mekoStack) addr(component string, port int) string {
	return fmt.Sprintf("http://%s:%d", svcFQDN(m.name(component), m.ns), port)
}

// labels for a component, all under the one resource key so that withdrawing the
// meko-stack resource garbage-collects the entire stack.
func (m *mekoStack) componentLabels(component string) map[string]string {
	return m.labelsFor(m.name(component), m.key)
}

// applyDatabase brings up the stack's YugabyteDB, or resolves an external one.
func (m *mekoStack) applyDatabase() error {
	spec := m.spec.Database
	name := m.name(compDatabase)

	if spec.managed() && m.spec.Images.Database == "" {
		// No default, for the same reason the workload kinds have none: the MAGE and
		// dist_rag extensions are in Yugabyte's Meko build, not in the public
		// yugabytedb image, so falling back to a public tag would produce a database
		// that comes up perfectly and then cannot install a single extension the
		// stack needs.
		err := fmt.Errorf("meko-stack needs images.database — the MAGE-enabled YugabyteDB build "+
			"(e.g. %s/yugabyte-db/ybdb-meko-release:<tag>); the public yugabytedb image has "+
			"neither mage nor pg_dist_rag", ecrRegistry)
		m.fail(compDatabase, err)
		return err
	}

	if spec.managed() {
		if err := checkImage("images.database", spec.Image); err != nil {
			m.fail(compDatabase, err)
			return err
		}
	}

	db, err := m.resolveDatabase(spec, name)
	if err != nil {
		m.fail(compDatabase, err)
		return err
	}
	// Kept whatever the apply below does, so a database that is serving but whose spec
	// has drifted on an immutable field does not take the rest of the stack with it.
	m.db = db

	if !spec.managed() {
		log.Printf("meko-stack %s: database is external (%s:%d) — not managed here", m.base, spec.Host, spec.Port)
		return nil
	}

	applyErr := m.applyDatabaseWorkload(name, m.componentLabels(compDatabase), spec)
	c := component{name: compDatabase, kind: "StatefulSet", version: imageTag(spec.Image)}
	if applyErr != nil {
		c.err = applyErr.Error()
	}
	m.record(c, name)
	return applyErr
}

// applySchema creates the Meko database and the extensions inside it.
func (m *mekoStack) applySchema() error {
	if m.db == nil {
		return fmt.Errorf("no database to create the Meko schema in")
	}
	if err := m.ensureDatabase(m.db, m.spec.DatabaseName); err != nil {
		return err
	}
	exts := *m.spec.Extensions
	if len(exts) == 0 {
		return nil
	}
	// A connection to the Meko database itself, not the admin one: CREATE EXTENSION
	// installs into the database it is run in.
	d := m.pool.use(m.db.uri(baseName(m.spec.DatabaseName)))
	if d == nil {
		return fmt.Errorf("database %s is not accepting connections yet", m.db.host)
	}
	for _, ext := range exts {
		if !validExtensionName.MatchString(ext) {
			return fmt.Errorf("extension %q is not a plain identifier", ext)
		}
		if err := d.createExtension(ext); err != nil {
			return fmt.Errorf("create extension %s in %s: %w", ext, m.spec.DatabaseName, err)
		}
	}
	return nil
}

// applyAPIServer deploys Meko's own REST API over the stack's database.
//
// Its database configuration goes in as ARGS rather than env, because that is the only
// contract api_server has for it — `-db-host` and friends have no environment
// fallback. The password still travels by reference: Kubernetes expands $(VAR) in args
// from the container's env, so the value reaches the process without appearing in the
// pod spec, and everything else api_server reads (OPENAI_API_KEY, the S3 and Cognito
// settings) it takes from the environment, which is what the operator's Secret is for.
func (m *mekoStack) applyAPIServer() error {
	if m.spec.Images.APIServer == "" {
		return fmt.Errorf("api-server needs images.api_server (e.g. %s/meko/meko-api-server:<tag>)", ecrRegistry)
	}
	if err := checkImage("images.api_server", m.spec.Images.APIServer); err != nil {
		return err
	}
	if m.db == nil {
		return fmt.Errorf("api-server needs the stack's database resolved")
	}
	name := m.name(compAPI)
	labels := m.componentLabels(compAPI)
	dbName := baseName(m.spec.DatabaseName)

	env := []any{
		secretEnv("DB_PASSWORD", m.db.secretRef, m.db.passwordKey),
		map[string]any{"name": "LOG_LEVEL", "value": strings.ToLower(m.spec.LogLevel)},
		map[string]any{"name": "S3_REGION", "value": m.spec.AWSRegion},
		map[string]any{"name": "AWS_REGION", "value": m.spec.AWSRegion},
	}
	if m.spec.S3Bucket != "" {
		env = append(env, map[string]any{"name": "S3_BUCKET", "value": m.spec.S3Bucket})
	}
	if m.spec.LangfuseBaseURL != "" {
		env = append(env, map[string]any{"name": "LANGFUSE_HOST", "value": m.spec.LangfuseBaseURL})
	}

	// The database flags, shared by the server and the migration step below so the two
	// cannot drift onto different databases.
	dbArgs := []string{
		"-db-host=" + m.db.host,
		"-db-port=" + fmt.Sprint(m.db.port),
		"-db-user-name=" + m.db.user,
		"-db-password=$(DB_PASSWORD)",
		"-db-name=" + dbName,
		"-db-sslmode=disable",
	}

	dep := deployment(deploymentOpts{
		name: name, ns: m.ns, labels: labels, image: m.spec.Images.APIServer,
		port: portAPI, env: env, envFrom: m.credentialEnvFrom(),
		initContainers: m.apiServerInitContainers(dbArgs, env, m.credentialEnvFrom()),
		command:        []string{"/usr/local/bin/meko-api-server"},
		args:           append([]string{"-port=" + fmt.Sprint(portAPI)}, dbArgs...),
		readiness: map[string]any{
			"httpGet":             map[string]any{"path": "/api/v1/health", "port": portAPI},
			"initialDelaySeconds": 10,
			"periodSeconds":       10,
			"failureThreshold":    30,
		},
		requests:   map[string]string{"cpu": "250m", "memory": "512Mi"},
		pullSecret: m.spec.ImagePullSecret,
	})
	if err := m.kc.applySSA(depPath(m.ns, name), dep); err != nil {
		return err
	}
	if err := m.applyExposedService(name, labels, portAPI, "http", m.exposure(compAPI)); err != nil {
		return err
	}
	c := component{name: compAPI, kind: "Deployment", version: imageTag(m.spec.Images.APIServer)}
	if m.schemaErr != nil {
		c.err = m.schemaErr.Error()
	}
	m.record(c, name)
	return nil
}

// apiServerInitContainers gates the api-server on its database and then builds its
// schema. Extracted so a test can assert both steps and their order: the order is
// load-bearing, and a missing migrate step is invisible until a real request runs.
func (m *mekoStack) apiServerInitContainers(dbArgs []string, env, envFrom []any) []any {
	return []any{
		// 1. Wait for the database rather than crash-loop at it. api_server pings five
		// times over fifteen seconds and then exits, and a yugabyted takes about ninety
		// seconds to come up — so without this gate the pod burns through its restarts
		// while the database is still starting and lands in a five-minute
		// CrashLoopBackOff, from which it stays down long after the database is
		// serving. Measured on kind: ready at 90s with the gate, still backing off at
		// 11 minutes without it. An Init:0/1 pod also says "waiting for its database"
		// at a glance, which CrashLoopBackOff does not.
		//
		// The api-server's own image, because it is Alpine and busybox brings nc;
		// adding a second image to the stack for a three-line wait is not worth the
		// pull.
		map[string]any{
			"name":      "wait-for-database",
			"image":     m.spec.Images.APIServer,
			"command":   []string{"/bin/sh", "-c"},
			"args":      []string{waitForPortScript(m.db.host, m.db.port)},
			"resources": map[string]any{"requests": map[string]string{"cpu": "10m", "memory": "16Mi"}},
		},
		// 2. Build the schema. api_server does NOT migrate on boot — it migrates only
		// when -migrate-only (or a scoping flag) is passed, and otherwise starts the
		// server against whatever is there. Without this step the api-server comes up
		// perfectly healthy, answers /api/v1/health with 200, and then fails every
		// real request with `relation "meko_system.users" does not exist` — a data
		// plane that looks entirely fine and cannot serve a single call. The control
		// plane has always had its own migrate init container for this reason; the
		// data plane needs one too.
		//
		// A bare -migrate-only outside control-plane mode runs both sets, system and
		// data, which is what a data plane wants: meko_system for accounts and
		// registries, datapacks/dist_rag for the tenant schema. The system set needs
		// YugabyteDB-only DDL, which is one more reason the stack's database is
		// YugabyteDB and not postgres.
		map[string]any{
			"name":      "migrate",
			"image":     m.spec.Images.APIServer,
			"command":   []string{"/usr/local/bin/meko-api-server"},
			"args":      append([]string{"-migrate-only"}, dbArgs...),
			"env":       env,
			"envFrom":   envFrom,
			"resources": map[string]any{"requests": map[string]string{"cpu": "100m", "memory": "256Mi"}},
		},
	}
}

// applyMCPServer deploys the tenant-facing MCP endpoint. It delegates to the
// mcp-server reconciler so there is one env contract for meko-mcp-server rather than
// two that drift.
func (m *mekoStack) applyMCPServer() error {
	return m.delegate(compMCP, m.spec.Images.MCPServer, portMCP, (*stack).applyMCPServer)
}

// applyRAGWorker deploys the RAG indexing worker, likewise through the existing
// reconciler.
func (m *mekoStack) applyRAGWorker() error {
	return m.delegate(compRAG, m.spec.Images.RAGWorker, 0, (*stack).applyRAGWorker)
}

// delegate runs one of the single-workload reconcilers as part of this stack: same
// database, same Secrets, same resource key so GC treats it as part of the stack.
//
// The alternative was a second copy of each env contract. meko-mcp-server's is
// fourteen variables across a ConfigMap, a composed-DSN Secret and an envFrom, with a
// comment on each explaining which of mem0's names it answers to; the RAG worker's
// takes a URI where the MCP server takes libpq keywords. Two copies of that diverge,
// and the divergence shows up as a workload that starts and then fails on its first
// write.
func (m *mekoStack) delegate(component, image string, port int, apply func(*stack, api.DesiredResource) error) error {
	if image == "" {
		return fmt.Errorf("%s needs images.%s", component, strings.ReplaceAll(component, "-", "_"))
	}
	if err := checkImage("images."+strings.ReplaceAll(component, "-", "_"), image); err != nil {
		return err
	}
	spec, err := json.Marshal(workloadSpec{
		Image:           image,
		Port:            port,
		DatabaseName:    m.spec.DatabaseName,
		SecretRef:       m.spec.SecretRef,
		ImagePullSecret: m.spec.ImagePullSecret,
		MekoBaseURL:     m.addr(compAPI, portAPI) + "/api/v1",
		LangfuseBaseURL: m.spec.LangfuseBaseURL,
		AWSRegion:       m.spec.AWSRegion,
		S3Bucket:        m.spec.S3Bucket,
		LogLevel:        m.spec.LogLevel,
	})
	if err != nil {
		return err
	}
	// The reconcilers read the database and the GC key off the stack, so both are lent
	// to them for the call: this stack's database, and this stack's resource key — so
	// the objects they apply are labelled as part of the stack rather than as
	// independently declared resources the GC would delete on sight.
	savedDB, savedKey := m.stack.db, m.stack.ownerKey
	m.stack.db, m.stack.ownerKey = m.db, m.key
	defer func() { m.stack.db, m.stack.ownerKey = savedDB, savedKey }()
	return apply(m.stack, api.DesiredResource{
		Kind: component, Name: m.name(component), Spec: spec,
	})
}

// applyGateway deploys the Playground chat backend against this stack's MCP server.
func (m *mekoStack) applyGateway() error {
	if m.spec.Images.InferenceGateway == "" {
		return fmt.Errorf("inference-gateway needs images.inference_gateway "+
			"(e.g. %s/meko/inference-gateway:<tag>)", ecrRegistry)
	}
	if err := checkImage("images.inference_gateway", m.spec.Images.InferenceGateway); err != nil {
		return err
	}
	name := m.name(compGateway)
	labels := m.componentLabels(compGateway)

	cfg := map[string]string{
		"INFERENCE_PORT": fmt.Sprint(portGateway),
		// The MCP server in this namespace, not the SaaS default the image ships with
		// (mcp.mekodata.ai) — a BYOC data plane that reached back out to the SaaS for
		// memory would be the opposite of the point.
		"INFERENCE_MCP_ENDPOINT": m.addr(compMCP, portMCP) + "/mcp",
		"INFERENCE_LOG_LEVEL":    strings.ToUpper(m.spec.LogLevel),
		"AWS_REGION":             m.spec.AWSRegion,
	}
	if m.spec.PublicBaseURL != "" {
		// The gateway's default CORS list is the SaaS origins. A browser on this data
		// plane's own address is not one of them, and the failure is a chat that never
		// answers with nothing in the gateway's log.
		cfg["INFERENCE_CORS_ORIGINS"] = m.spec.PublicBaseURL
	}
	if err := m.applyConfigMap(name, labels, cfg); err != nil {
		return err
	}

	dep := deployment(deploymentOpts{
		name: name, ns: m.ns, labels: labels, image: m.spec.Images.InferenceGateway,
		port: portGateway, envFrom: append(m.configEnvFrom(name), m.credentialEnvFrom()...),
		readiness: map[string]any{
			"httpGet":             map[string]any{"path": "/healthz", "port": portGateway},
			"initialDelaySeconds": 10,
			"periodSeconds":       10,
			"failureThreshold":    30,
		},
		requests:   map[string]string{"cpu": "250m", "memory": "512Mi"},
		pullSecret: m.spec.ImagePullSecret,
	})
	if err := m.kc.applySSA(depPath(m.ns, name), dep); err != nil {
		return err
	}
	if err := m.applyExposedService(name, labels, portGateway, "http", m.exposure(compGateway)); err != nil {
		return err
	}
	m.record(component{name: compGateway, kind: "Deployment", version: imageTag(m.spec.Images.InferenceGateway)}, name)
	return nil
}

// applyUI deploys the web app, proxying to this stack's api-server.
//
// Note what this cannot fix: the UI's Cognito settings are baked into its bundle at
// build time (VITE_COGNITO_* in meko_ui/Dockerfile), so a data plane runs whatever
// identity provider its image was built against. Only the runtime allow-list in
// meko_ui/server/index.ts — feature flags, the admin view, RudderStack — is settable
// from here.
func (m *mekoStack) applyUI() error {
	if m.spec.Images.UI == "" {
		return fmt.Errorf("ui needs images.ui (e.g. %s/meko/meko-ui:<tag>)", ecrRegistry)
	}
	if err := checkImage("images.ui", m.spec.Images.UI); err != nil {
		return err
	}
	name := m.name(compUI)
	labels := m.componentLabels(compUI)

	cfg := map[string]string{
		"PORT": fmt.Sprint(portUI),
		// The api-server in this namespace. Empty would disable the proxy and leave the
		// UI serving its own mock data, which looks like a working Meko with none of the
		// data plane behind it.
		"MEKO_API_UPSTREAM": m.addr(compAPI, portAPI),
		"NODE_ENV":          "production",
	}
	if err := m.applyConfigMap(name, labels, cfg); err != nil {
		return err
	}

	dep := deployment(deploymentOpts{
		name: name, ns: m.ns, labels: labels, image: m.spec.Images.UI,
		port: portUI, envFrom: append(m.configEnvFrom(name), m.credentialEnvFrom()...),
		readiness: map[string]any{
			// /runtime-config.js rather than /: it is a route the server itself answers,
			// so a 200 means the Express server is up rather than that a static file
			// happened to be readable.
			"httpGet":             map[string]any{"path": "/runtime-config.js", "port": portUI},
			"initialDelaySeconds": 10,
			"periodSeconds":       10,
			"failureThreshold":    18,
		},
		requests:   map[string]string{"cpu": "100m", "memory": "256Mi"},
		pullSecret: m.spec.ImagePullSecret,
	})
	if err := m.kc.applySSA(depPath(m.ns, name), dep); err != nil {
		return err
	}
	if err := m.applyExposedService(name, labels, portUI, "http", m.exposure(compUI)); err != nil {
		return err
	}
	m.record(component{name: compUI, kind: "Deployment", version: imageTag(m.spec.Images.UI)}, name)
	return nil
}

// waitForPortScript blocks until something is listening on host:port. busybox `nc`,
// because the images this runs in are Alpine and it is already there.
func waitForPortScript(host string, port int) string {
	return fmt.Sprintf("until nc -z %s %d; do echo 'waiting for %s:%d'; sleep 3; done", host, port, host, port)
}

// applyConfigMap writes a component's non-secret configuration.
func (m *mekoStack) applyConfigMap(name string, labels map[string]string, data map[string]string) error {
	return m.kc.applySSA(cmPath(m.ns, name), map[string]any{
		"apiVersion": "v1", "kind": "ConfigMap",
		"metadata": map[string]any{"name": name, "namespace": m.ns, "labels": labels},
		"data":     data,
	})
}

func (m *mekoStack) configEnvFrom(name string) []any {
	return []any{map[string]any{"configMapRef": map[string]any{"name": name}}}
}

// credentialEnvFrom mounts the operator's Secret whole, if there is one.
//
// optional:true so a Secret the operator has not created yet does not block the
// rollout: the workload starts and reports its own missing-credential error, which
// says more than a pod stuck in CreateContainerConfigError.
func (m *mekoStack) credentialEnvFrom() []any {
	if m.spec.SecretRef == "" {
		return nil
	}
	return []any{map[string]any{
		"secretRef": map[string]any{"name": m.spec.SecretRef, "optional": true},
	}}
}
