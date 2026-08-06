// Reconcilers for the Meko data-plane stack: the resources an operator declares
// as desired state and this agent turns into running workloads.
//
// The protocol already carries whatever the control plane wants provisioned as a
// typed {kind, name, spec} triple, so a new managed component needs a reconciler
// here and nothing else — no wire change, no control-plane change. The kinds:
//
//	database     the data-plane database (managed StatefulSet, or an external one
//	             the operator points us at). Everything else resolves its DSN.
//	mcp-server   meko-mcp-server over that database — the tenant-facing endpoint.
//	rag-worker   the RAG indexing worker over the same database.
//
// There is a fourth, meko-stack, in mekostack.go: the whole data plane as a single
// resource, which is what registering an environment seeds. It is built on the pieces
// here rather than beside them — same database implementation, same env contracts.
//
// An ENVIRONMENT is the unit: one namespace, one stack. There is no datapack layer
// here — datapacks are Meko's own feature, created through its api-server once the
// stack is running, and YCP models the environment rather than what runs inside it.
//
// Two rules the reconcilers here follow, both learned the hard way elsewhere in
// this repo:
//
//   - Apply is server-side apply, not "POST and treat 409 as done". The latter is
//     create-once: a changed image in desired state would be accepted, reported
//     applied, and never actually roll out.
//   - Secret VALUES never travel in desired state, only Secret NAMES. Credentials
//     the operator supplies (OPENAI_API_KEY, AWS keys) reach a workload by
//     envFrom-secretRef, so the agent does not read, copy or log them.
package main

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log"
	"net/url"
	"regexp"
	"sort"
	"strings"

	"meko/ycp/internal/api"
)

const (
	// stackLabel marks every object a stack reconciler owns, and stackResourceLabel
	// records which desired resource it belongs to, so GC can tell a component that
	// is no longer declared from one that was never ours.
	stackLabel         = "meko.dev/stack"
	stackResourceLabel = "meko.dev/resource"

	// fieldManager identifies this agent's server-side-apply ownership.
	fieldManager = "meko-agent"

	// dbSecret holds the managed database's superuser password, and dbPasswordKey is
	// its key. Created once with a generated value when the operator supplies no
	// secret_ref: a local bring-up should not need an out-of-band Secret, and a
	// password the agent generates is strictly better than a well-known default.
	dbSecret      = "meko-database"
	dbPasswordKey = "password"

	// mekoDatabase is the database meko-mcp-server and the RAG worker share.
	mekoDatabase = "meko"
)

// ---- desired-state specs ----
//
// Every field is optional. An operator declaring `{"kind":"database","name":"meko"}`
// with no spec gets a working local default; production overrides what it needs.
// Unknown fields are ignored rather than rejected, so a newer control plane can
// send a richer spec to an older agent without breaking it.

// databaseSpec is kind "database": where this environment's Meko data lives.
type databaseSpec struct {
	// Mode is "managed" (the agent runs it) or "external" (the operator's own
	// database, e.g. the customer's YugabyteDB). Default managed.
	Mode string `json:"mode"`
	// Engine names what is running, and is the component name reported upstream.
	// "postgres" (default) or "yugabytedb".
	Engine string `json:"engine"`
	Image  string `json:"image"`
	// Command/Args override the container entrypoint — the escape hatch that lets an
	// engine the agent has no special knowledge of be declared without a code change.
	Command []string `json:"command"`
	Args    []string `json:"args"`

	// MasterFlags/TServerFlags are YugabyteDB gflags, passed to yugabyted as
	// --master_flags / --tserver_flags. This is how a server-side feature that is off
	// by default gets turned on — `ysql_yb_enable_mage=true` is the one the Meko schema
	// needs, and it has to be set at process start, not by a later SQL statement.
	MasterFlags  map[string]string `json:"master_flags"`
	TServerFlags map[string]string `json:"tserver_flags"`

	Host     string `json:"host"` // external mode only
	Port     int    `json:"port"`
	User     string `json:"user"`
	Database string `json:"database"` // admin database to connect to
	// SecretRef/PasswordKey name the Secret holding the password. Defaults to the
	// agent-managed one. Never a password value: desired state carries no secrets.
	SecretRef   string `json:"secret_ref"`
	PasswordKey string `json:"password_key"`

	StorageSize     string `json:"storage_size"`
	StorageClass    string `json:"storage_class"`
	ImagePullSecret string `json:"image_pull_secret"`
}

func (s databaseSpec) managed() bool { return s.Mode != "external" }

func (s databaseSpec) engine() string {
	if s.Engine != "" {
		return s.Engine
	}
	return "postgres"
}

// defaults fills in a spec that is coherent on a laptop kind cluster with no
// registry credentials and no pre-created Secrets.
//
// pgvector/pgvector, not plain postgres: meko-mcp-server stores embeddings through
// mem0, which needs the `vector` extension. A stock postgres image comes up fine
// and then fails on the first memory write, which looks like an MCP bug rather
// than a missing extension.
func (s databaseSpec) withDefaults() databaseSpec {
	if s.Image == "" && s.managed() {
		if s.engine() == "yugabytedb" {
			s.Image = "yugabytedb/yugabyte:2.25.2.0-b359"
		} else {
			s.Image = "pgvector/pgvector:pg16"
		}
	}
	if s.Port == 0 {
		if s.engine() == "yugabytedb" {
			s.Port = 5433
		} else {
			s.Port = 5432
		}
	}
	if s.User == "" {
		if s.engine() == "yugabytedb" {
			s.User = "yugabyte"
		} else {
			s.User = "postgres"
		}
	}
	if s.Database == "" {
		if s.engine() == "yugabytedb" {
			s.Database = "yugabyte"
		} else {
			s.Database = "postgres"
		}
	}
	if s.SecretRef == "" {
		s.SecretRef = dbSecret
	}
	if s.PasswordKey == "" {
		s.PasswordKey = dbPasswordKey
	}
	if s.StorageSize == "" {
		s.StorageSize = "8Gi"
	}
	return s
}

// workloadSpec is the shape kind "mcp-server" and kind "rag-worker" share.
type workloadSpec struct {
	Image    string `json:"image"`
	Replicas int    `json:"replicas"`
	// Database is the name of the declared database resource to run against.
	Database string `json:"database"`
	// DatabaseName is the database within it (default "meko"); the agent creates it.
	DatabaseName string `json:"database_name"`
	// SecretRef names an operator-supplied Secret mounted whole with
	// envFrom — where OPENAI_API_KEY and the AWS keys come from. Optional: absent,
	// the workload starts without them and fails its own way, which is a clearer
	// signal than the agent refusing to deploy.
	SecretRef string `json:"secret_ref"`

	// ImagePullSecret names a docker-registry Secret in the namespace. The Meko
	// images are private (ECR, no anonymous pull), so without this a real image can
	// only be side-loaded into the node — which is why ycp/STATUS.md lists
	// unpullable images in the namespaces the agent creates as a known gap.
	ImagePullSecret string `json:"image_pull_secret"`

	Port            int    `json:"port"`
	MekoBaseURL     string `json:"meko_base_url"`
	LangfuseBaseURL string `json:"langfuse_base_url"`
	AWSRegion       string `json:"aws_region"`
	S3Bucket        string `json:"s3_bucket"`
	LogLevel        string `json:"log_level"`

	// CreateTables lets mem0 run its own DDL (MEM0_CREATE_TABLES). The platform
	// normally provisions the pgvector schema out of band and the server is expected
	// not to; against a database this agent just created there is no such step, so a
	// managed database turns this on and an external one leaves it to the operator.
	CreateTables *bool `json:"create_tables"`
}

// ---- resolved database ----

// resolvedDB is a reconciled database another workload can be pointed at: the
// facts needed to build a DSN, plus where the password lives so it can be wired
// by reference instead of by value.
type resolvedDB struct {
	host        string
	port        int
	user        string
	database    string // admin database
	secretRef   string
	passwordKey string
	// password is read from the Secret only where a literal DSN is unavoidable
	// (YUGABYTEDB_URL is a single libpq string, and the agent's own pgx pool).
	password string
	engine   string
	// managed is true when this agent runs the database, which is what decides
	// whether anything else has provisioned a schema in it.
	managed bool
}

// libpq renders the keyword/value form. meko-mcp-server's pgvector path wants
// this, not a URL — LOCAL_DEV_SETUP.md calls the distinction out for the same
// reason it matters here.
func (d resolvedDB) libpq(dbName string) string {
	return fmt.Sprintf("host=%s port=%d dbname=%s user=%s password=%s",
		d.host, d.port, dbName, d.user, d.password)
}

// uri renders the postgresql:// form, which the RAG worker and pgx want.
func (d resolvedDB) uri(dbName string) string {
	u := url.URL{
		Scheme: "postgresql",
		User:   url.UserPassword(d.user, d.password),
		Host:   fmt.Sprintf("%s:%d", d.host, d.port),
		Path:   "/" + dbName,
	}
	q := u.Query()
	q.Set("sslmode", "disable")
	u.RawQuery = q.Encode()
	return u.String()
}

// ---- the stack ----

// stack reconciles an environment's declared resources, in the namespace the agent
// itself runs in.
type stack struct {
	kc *k8s
	ns string
	// pool is the shared data-plane connection cache: one connection per database for
	// the life of the process, rather than one per tick.
	pool *dbPool
	// db is the database resolved this tick, for other reconcilers to use. Nil
	// until a database resource is declared and applied.
	db *resolvedDB
	// components accumulates real health for the status report.
	components []component
	// owned records the resource keys applied this tick, so GC can delete the rest.
	owned map[string]bool
	// ownerKey, when set, is the resource key reconcilers label their objects with
	// instead of their own. The meko-stack reconciler sets it while it delegates to the
	// single-workload reconcilers, so every part of a stack carries one key and the GC
	// treats the stack as the unit it is declared as.
	ownerKey string
}

// component is one managed workload's health, as read back from the API server.
type component struct {
	name    string
	kind    string // Deployment | StatefulSet
	healthy int
	total   int
	version string
	err     string
}

func newStack(kc *k8s, ns string, pool *dbPool) *stack {
	return &stack{kc: kc, ns: ns, pool: pool, owned: map[string]bool{}}
}

// resourceKey is the value of stackResourceLabel: it has to survive as a label
// value, so it is kind and name joined by a dot rather than a slash.
func resourceKey(kind, name string) string { return kind + "." + baseName(name) }

// keyFor is the GC key a reconciler labels its objects with — its own resource's,
// unless it is running as part of a meko-stack, in which case it is the stack's.
func (s *stack) keyFor(kind, name string) string {
	if s.ownerKey != "" {
		return s.ownerKey
	}
	return resourceKey(kind, name)
}

// reconcile applies the given stack resources in dependency order — the database
// first, because the workloads that follow need its DSN.
//
// It does NOT garbage-collect: gc is a separate call, because it is only correct on a
// tick where the caller actually knows the full set of what is wanted. See the call
// site in main.
//
// Ordering is by kind, not by declaration order: an operator who lists mcp-server
// above database in the JSON should still get a working stack rather than an
// mcp-server pointed at nothing.
func (s *stack) reconcile(resources []api.DesiredResource) {
	// What is DECLARED, recorded before anything is applied. GC deletes what is not in
	// here, so it has to mean "not desired" — and only desired state can say that.
	//
	// It used to be filled in by each reconciler as it succeeded, which quietly made it
	// mean "applied cleanly this tick" instead. Any reconciler that returned early —
	// including mcp-server on the tick after its database blipped — left its key out,
	// and the GC then deleted a healthy, running, still-declared workload. Observed on
	// a real cluster: an mcp-server Deployment and Service vanished while desired state
	// still asked for them.
	for _, r := range resources {
		if stackKinds[r.Kind] {
			s.owned[resourceKey(r.Kind, r.Name)] = true
		}
	}

	for _, r := range inDependencyOrder(resources) {
		// The reconcilers record their own component row — with the real replica counts
		// and the error, if any — so the reported name stays stable (`postgres`,
		// `mcp-server`) whether the apply worked or not. Only a resource that failed
		// before recording anything needs a row here, and it gets exactly one: an
		// unconditional append would report the same resource twice, under two
		// different names, on every failing tick.
		before := len(s.components)
		var err error
		switch r.Kind {
		case "meko-stack":
			err = s.applyMekoStack(r)
		case "database":
			err = s.applyDatabase(r)
		case "mcp-server":
			err = s.applyMCPServer(r)
		case "rag-worker":
			err = s.applyRAGWorker(r)
		}
		if err != nil {
			log.Printf("reconcile %s/%s: %v", r.Kind, r.Name, err)
			if len(s.components) == before {
				s.fail(r.Kind, err)
			}
		}
	}
}

// componentNameFor is what a kind reports itself as upstream — the thing running,
// not the name the operator gave the resource. Two databases named `primary` and
// `analytics` should both read as `postgres` in a component list.
func componentNameFor(kind string, spec databaseSpec) string {
	if kind == "database" {
		return spec.engine()
	}
	return kind
}

// fail records a component row for a resource that could not be applied at all, so
// an environment with a broken spec reports the failure rather than silently
// omitting the component and looking healthy.
func (s *stack) fail(name string, err error) {
	s.components = append(s.components, component{name: name, total: 1, err: err.Error()})
}

// reconcileOrder ranks kinds by what they depend on: everything needs the database's
// DSN, so it is applied first whatever order the control plane listed it in.
// meko-stack brings its own database and is self-contained, so it goes first of all.
var reconcileOrder = map[string]int{"meko-stack": -1, "database": 0, "mcp-server": 1, "rag-worker": 2}

// inDependencyOrder returns the resources sorted by reconcileOrder, stably, so
// resources of the same kind keep their declared order.
func inDependencyOrder(resources []api.DesiredResource) []api.DesiredResource {
	sorted := make([]api.DesiredResource, len(resources))
	copy(sorted, resources)
	sort.SliceStable(sorted, func(i, j int) bool {
		return reconcileOrder[sorted[i].Kind] < reconcileOrder[sorted[j].Kind]
	})
	return sorted
}

// stackKinds are the kinds reconciled here. main dispatches on this so an
// unhandled kind is logged once, in one place, rather than silently ignored.
var stackKinds = map[string]bool{
	"meko-stack": true, "database": true, "mcp-server": true, "rag-worker": true,
}

// nonDNS matches everything not allowed in a DNS-1123 object name.
var nonDNS = regexp.MustCompile(`[^a-z0-9-]+`)

func decodeSpec(raw json.RawMessage, out any) error {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	return json.Unmarshal(raw, out)
}

// applyDatabase brings up (or resolves) the data-plane database and records it on
// the stack for the workloads that follow.
func (s *stack) applyDatabase(r api.DesiredResource) error {
	var spec databaseSpec
	if err := decodeSpec(r.Spec, &spec); err != nil {
		return fmt.Errorf("bad database spec: %w", err)
	}
	spec = spec.withDefaults()
	name := baseNameOr(r.Name, "meko-db")

	db, err := s.resolveDatabase(spec, name)
	if err != nil {
		s.fail(componentNameFor(r.Kind, spec), err)
		return err
	}
	// Resolve the DSN BEFORE applying, and keep it whatever the apply does.
	//
	// It is derived from the spec and the generated password, not from the apply
	// result, and the workloads that follow only need somewhere to point. Returning
	// early on an apply error used to leave s.db nil, so a database that was up and
	// serving but whose spec had drifted on an immutable field (storage_size, below)
	// took the whole stack down with it: every dependent workload then failed with
	// "needs a database resource declared alongside it".
	s.db = db

	if !spec.managed() {
		// An external database is not this agent's workload, so it gets no component
		// row: reporting 1/1 healthy for something we neither run nor probe would be
		// an invention, and the console draws component rows as facts.
		log.Printf("database %q is external (%s:%d) — not managed here", r.Name, spec.Host, spec.Port)
		return nil
	}

	// Ownership is recorded in reconcile() from desired state, not here: a reconciler
	// that returns early must not make its resource look undesired to the GC.
	key := s.keyFor(r.Kind, r.Name)
	labels := s.labelsFor(name, key)

	applyErr := s.applyDatabaseWorkload(name, labels, spec)

	// Record the component either way, with the real replica counts read back. A
	// database whose spec cannot be applied but whose pod is up is both facts at once,
	// and reporting only the error would hide a running database from the fleet view.
	c := component{name: spec.engine(), kind: "StatefulSet", version: imageTag(spec.Image)}
	if applyErr != nil {
		c.err = applyErr.Error()
	}
	s.record(c, name)
	return applyErr
}

// resolveDatabase turns a spec into the facts every other workload derives its DSN
// from: for a managed database the password is generated once and kept, for an
// external one it is read from the Secret the operator put it in.
//
// Split out from applyDatabase because the meko-stack reconciler needs exactly this
// and nothing else — one database implementation, reached two ways.
func (s *stack) resolveDatabase(spec databaseSpec, svcName string) (*resolvedDB, error) {
	if !spec.managed() {
		if spec.Host == "" {
			return nil, fmt.Errorf("external database needs spec.host")
		}
		pw, err := s.kc.secretValue(s.ns, spec.SecretRef, spec.PasswordKey)
		if err != nil {
			return nil, fmt.Errorf("read %s/%s: %w", spec.SecretRef, spec.PasswordKey, err)
		}
		return &resolvedDB{
			host: spec.Host, port: spec.Port, user: spec.User, database: spec.Database,
			secretRef: spec.SecretRef, passwordKey: spec.PasswordKey, password: pw,
			engine: spec.engine(), managed: false,
		}, nil
	}
	// The password: reuse the Secret if it exists, generate it once if it does not.
	pw, err := s.kc.ensurePassword(s.ns, spec.SecretRef, spec.PasswordKey)
	if err != nil {
		return nil, fmt.Errorf("database password: %w", err)
	}
	return &resolvedDB{
		host:      svcFQDN(svcName, s.ns),
		port:      spec.Port,
		user:      spec.User,
		database:  spec.Database,
		secretRef: spec.SecretRef, passwordKey: spec.PasswordKey,
		password: pw, engine: spec.engine(), managed: true,
	}, nil
}

// applyDatabaseWorkload applies the StatefulSet and Service for a managed database.
func (s *stack) applyDatabaseWorkload(name string, labels map[string]string, spec databaseSpec) error {
	container := map[string]any{
		"name":  "database",
		"image": spec.Image,
		"ports": []any{map[string]any{"name": "sql", "containerPort": spec.Port}},
		"env":   databaseEnv(spec),
		"volumeMounts": []any{map[string]any{
			"name": "data", "mountPath": dataMountPath(spec.engine()),
		}},
		"readinessProbe": map[string]any{
			"exec":                map[string]any{"command": readinessCommand(spec)},
			"initialDelaySeconds": 10,
			"periodSeconds":       5,
			"failureThreshold":    30,
		},
		"resources": map[string]any{
			"requests": databaseRequests(spec),
		},
	}
	command, args := spec.Command, spec.Args
	if len(command) == 0 && len(args) == 0 && spec.engine() == "yugabytedb" {
		command, args = yugabytedCommand(spec, name, s.ns)
	}
	if len(command) > 0 {
		container["command"] = command
	}
	if len(args) > 0 {
		container["args"] = args
	}

	// Built as its own variable rather than reaching back into the nested map with a
	// type assertion: an assertion that is wrong panics, and a panic here takes the
	// whole reconcile loop down with it.
	claimSpec := map[string]any{
		"accessModes": []any{"ReadWriteOnce"},
		"resources":   map[string]any{"requests": map[string]string{"storage": spec.StorageSize}},
	}
	if spec.StorageClass != "" {
		claimSpec["storageClassName"] = spec.StorageClass
	}
	claim := map[string]any{
		"metadata": map[string]any{"name": "data"},
		"spec":     claimSpec,
	}

	podSpec := map[string]any{"containers": []any{container}}
	if spec.ImagePullSecret != "" {
		podSpec["imagePullSecrets"] = []any{map[string]any{"name": spec.ImagePullSecret}}
	}
	sts := map[string]any{
		"apiVersion": "apps/v1", "kind": "StatefulSet",
		"metadata": map[string]any{"name": name, "namespace": s.ns, "labels": labels},
		"spec": map[string]any{
			"serviceName": name,
			"replicas":    1,
			"selector":    map[string]any{"matchLabels": map[string]string{"app": name}},
			"template": map[string]any{
				"metadata": map[string]any{"labels": labels},
				"spec":     podSpec,
			},
			"volumeClaimTemplates": []any{claim},
		},
	}
	applyErr := s.kc.applySSA(stsPath(s.ns, name), sts)
	if applyErr != nil {
		applyErr = describeImmutable(applyErr, name)
	}
	if err := s.applyDatabaseService(name, labels, spec); err != nil && applyErr == nil {
		applyErr = err
	}
	return applyErr
}

// applyDatabaseService publishes the database.
//
// yugabytedb gets a HEADLESS Service, and that is not a stylistic choice: yugabyted
// binds to the address it is told to advertise, so that address has to be the pod's
// own — a ClusterIP would resolve to a virtual address the process cannot bind. A
// headless Service resolves the Service name straight to the pod, which is what both
// the process and its clients need, and it is what gives the pod its stable
// `<pod>.<svc>` name. `publishNotReadyAddresses` because yugabyted resolves that name
// during startup, long before the readiness probe passes.
func (s *stack) applyDatabaseService(name string, labels map[string]string, spec databaseSpec) error {
	if spec.engine() != "yugabytedb" {
		return s.applyService(name, labels, spec.Port, "sql")
	}
	return s.kc.applySSA(svcPath(s.ns, name), map[string]any{
		"apiVersion": "v1", "kind": "Service",
		"metadata": map[string]any{"name": name, "namespace": s.ns, "labels": labels},
		"spec": map[string]any{
			"clusterIP":                "None",
			"publishNotReadyAddresses": true,
			"selector":                 map[string]string{"app": name},
			"ports":                    []any{map[string]any{"name": "sql", "port": spec.Port, "targetPort": spec.Port}},
		},
	})
}

// databaseEnv is the engine's own configuration contract. Both engines read their
// bootstrap user, database and password from the environment; the names differ.
func databaseEnv(spec databaseSpec) []any {
	if spec.engine() == "yugabytedb" {
		return []any{
			map[string]any{"name": "YSQL_USER", "value": spec.User},
			map[string]any{"name": "YSQL_DB", "value": spec.Database},
			secretEnv("YSQL_PASSWORD", spec.SecretRef, spec.PasswordKey),
			// PGPASSWORD as well, for the readiness probe's ysqlsh.
			//
			// Setting YSQL_PASSWORD makes yugabyted turn `ysql_enable_auth` on, so the
			// probe's own connection then needs a password too. Without this the probe
			// fails with "no password supplied" on every single attempt and the database
			// never reports ready however healthy it is — verified against
			// ybdb-meko-release:2026.1.0.0-b53, where the probe hits a password prompt.
			secretEnv("PGPASSWORD", spec.SecretRef, spec.PasswordKey),
		}
	}
	return []any{
		map[string]any{"name": "POSTGRES_USER", "value": spec.User},
		map[string]any{"name": "POSTGRES_DB", "value": spec.Database},
		secretEnv("POSTGRES_PASSWORD", spec.SecretRef, spec.PasswordKey),
		// Keep initdb out of the mounted data directory: the volume has a lost+found,
		// and postgres refuses to initialise into a non-empty directory.
		map[string]any{"name": "PGDATA", "value": "/var/lib/postgresql/data/pgdata"},
	}
}

// ybBaseDir is yugabyted's --base_dir: a SUBDIRECTORY of the mounted volume, for the
// same reason PGDATA is above — the volume root has a lost+found in it.
const ybBaseDir = "/home/yugabyte/var/data"

// yugabytedCommand renders the entrypoint for a single-node yugabyted.
//
// Not an override of a working default: the image's entrypoint starts no database at
// all, so without this a `yugabytedb` engine produces a pod that runs and serves
// nothing. That is what the kind used to do.
//
// --advertise_address is the pod's stable DNS name rather than its IP, because
// yugabyted records the address it was started with in its own metadata: a pod that
// came back with a new IP would no longer recognise the data directory it is looking
// at. ${HOSTNAME} is the pod name, which for a StatefulSet is stable — and it is
// written with braces so that Kubernetes, which expands only the $(VAR) form in
// command and args, leaves it for the shell.
//
// The wait loop covers DNS propagation. The Service publishes not-ready addresses so
// the name exists as soon as the pod has an IP, but "as soon as" is not "already", and
// yugabyted exits rather than retries if the name it must bind does not resolve yet.
func yugabytedCommand(spec databaseSpec, svc, ns string) (command, args []string) {
	script := strings.Join([]string{
		"set -eu",
		fmt.Sprintf(`ADDR="${HOSTNAME}.%s.%s.svc.cluster.local"`, svc, ns),
		`until getent hosts "$ADDR" >/dev/null 2>&1; do echo "waiting for $ADDR to resolve"; sleep 2; done`,
		"mkdir -p " + ybBaseDir,
		strings.Join(append([]string{
			"exec bin/yugabyted start --background=false",
			"--base_dir=" + ybBaseDir,
			`--advertise_address="$ADDR"`,
		}, gflagArgs(spec)...), " "),
	}, "\n")
	return []string{"/bin/bash", "-c"}, []string{script}
}

// gflagArgs renders the --master_flags / --tserver_flags pairs, sorted.
//
// Sorted because the rendered string goes into a pod template that is server-side
// applied every tick: Go map iteration order is randomised, so an unsorted render
// would rewrite the StatefulSet — and restart the database — on roughly every tick.
func gflagArgs(spec databaseSpec) []string {
	var out []string
	for _, f := range []struct {
		arg   string
		flags map[string]string
	}{
		{"--master_flags", spec.MasterFlags},
		{"--tserver_flags", spec.TServerFlags},
	} {
		if len(f.flags) == 0 {
			continue
		}
		pairs := make([]string, 0, len(f.flags))
		for k, v := range f.flags {
			pairs = append(pairs, k+"="+v)
		}
		sort.Strings(pairs)
		out = append(out, f.arg+"="+strings.Join(pairs, ","))
	}
	return out
}

// databaseRequests sizes the pod. YugabyteDB runs a master and a tserver in one
// process tree and will not come up in the 256Mi a postgres is happy with.
func databaseRequests(spec databaseSpec) map[string]string {
	if spec.engine() == "yugabytedb" {
		return map[string]string{"cpu": "500m", "memory": "2Gi"}
	}
	return map[string]string{"cpu": "100m", "memory": "256Mi"}
}

// describeImmutable turns the API server's rejection of an immutable-field update
// into something an operator can act on.
//
// A StatefulSet's volumeClaimTemplates cannot be changed in place, so raising
// storage_size on a declared database is rejected forever, once per tick, as a
// 422 with a long field list in it. Resizing means recreating the StatefulSet, and
// the agent will not do that to a running database on its own — deleting the
// workload that owns the data is a decision an operator has to make deliberately.
func describeImmutable(err error, name string) error {
	msg := err.Error()
	if !strings.Contains(msg, "Forbidden: updates to statefulset spec") {
		return err
	}
	return fmt.Errorf("this database's StatefulSet (%s) is already running with different "+
		"storage or a different volume layout, and those fields cannot be changed in place. "+
		"Either declare the values it was created with, or delete the StatefulSet deliberately "+
		"to have it recreated (which discards its PersistentVolumeClaim). "+
		"The database itself is untouched and still serving. Underlying error: %w", name, err)
}

// applyMCPServer deploys meko-mcp-server against the resolved database.
//
// The env contract is meko-mcp-server's, mirrored from what the provisioner sets
// for a tenant (meko-provisioner/internal/controller/resources.go): the same
// generic HOST/PORT/DBNAME/USER/PASSWORD names mem0 reads, plus YUGABYTEDB_URL in
// libpq keyword form for the pgvector path.
func (s *stack) applyMCPServer(r api.DesiredResource) error {
	var spec workloadSpec
	if err := decodeSpec(r.Spec, &spec); err != nil {
		return fmt.Errorf("bad mcp-server spec: %w", err)
	}
	if spec.Image == "" {
		// No default on purpose: meko-mcp-server is a private image with no public
		// tag to guess, and a wrong guess fails as ImagePullBackOff minutes later.
		return fmt.Errorf("mcp-server needs spec.image (e.g. %s/meko/meko-mcp-server:<tag>)", ecrRegistry)
	}
	if s.db == nil {
		return fmt.Errorf("mcp-server needs a database resource declared alongside it")
	}
	dbName := orDefault(spec.DatabaseName, mekoDatabase)
	port := spec.Port
	if port == 0 {
		port = 8000
	}
	name := baseNameOr(r.Name, "mcp-server")
	// Ownership is recorded in reconcile() from desired state, not here: a reconciler
	// that returns early must not make its resource look undesired to the GC.
	key := s.keyFor(r.Kind, r.Name)
	labels := s.labelsFor(name, key)

	// The database itself, created through the same pool the datapack reconciler uses.
	// mem0 will not create it, and CREATE DATABASE cannot run from a connection to the
	// database being created.
	//
	// Best-effort, and NOT a gate on applying the workload below. Returning here left
	// the mcp-server Deployment uncreated for as long as the agent could not reach the
	// database — so a database that was slow to accept connections read as a control
	// plane being ignored, with nothing in the namespace to look at. The workload does
	// its own connecting and its own retrying; the agent's job is to declare it.
	dbErr := s.ensureDatabase(s.db, dbName)
	if dbErr != nil {
		s.pool.logOnce("mcp-server."+name, fmt.Sprintf(
			"mcp-server %s: %v — deploying anyway, it retries its own connection", name, dbErr))
	}

	cfg := map[string]string{
		"YB_MCP_TRANSPORT":      "http",
		"YB_MCP_STATELESS_HTTP": "true",
		"YB_ALLOWED_HOSTS":      "all",
		"YB_LOG_LEVEL":          orDefault(spec.LogLevel, "INFO"),
		"AWS_REGION":            orDefault(spec.AWSRegion, "us-east-1"),
		// A database this agent just created has no schema in it, and the server
		// deliberately runs no DDL by default. Left off, it starts and then fails on
		// the first memory write with a missing relation — which reads as an MCP bug.
		"MEM0_CREATE_TABLES": createTables(spec, s.db),
	}
	for k, v := range map[string]string{
		"MEKO_BASE_URL":     spec.MekoBaseURL,
		"LANGFUSE_BASE_URL": spec.LangfuseBaseURL,
		"LANGFUSE_HOST":     spec.LangfuseBaseURL,
	} {
		if v != "" {
			cfg[k] = v
		}
	}
	if err := s.kc.applySSA(cmPath(s.ns, name), map[string]any{
		"apiVersion": "v1", "kind": "ConfigMap",
		"metadata": map[string]any{"name": name, "namespace": s.ns, "labels": labels},
		"data":     cfg,
	}); err != nil {
		return err
	}

	// The one Secret the agent writes: a composed connection string that cannot be
	// assembled from secretKeyRefs, because it is a single string with the password
	// inside it. Same namespace and same trust boundary as the password it derives
	// from, and for a managed database it is a password this agent generated.
	if err := s.kc.applySSA(secretPath(s.ns, name+"-db"), map[string]any{
		"apiVersion": "v1", "kind": "Secret",
		"metadata": map[string]any{"name": name + "-db", "namespace": s.ns, "labels": labels},
		"stringData": map[string]string{
			"YUGABYTEDB_URL": s.db.libpq(dbName),
		},
	}); err != nil {
		return err
	}

	env := []any{
		map[string]any{"name": "HOST", "value": s.db.host},
		map[string]any{"name": "PORT", "value": fmt.Sprint(s.db.port)},
		map[string]any{"name": "DBNAME", "value": dbName},
		map[string]any{"name": "USER", "value": s.db.user},
		map[string]any{"name": "USERNAME", "value": s.db.user},
		map[string]any{"name": "YUGABYTEDB_USER", "value": s.db.user},
		secretEnv("PASSWORD", s.db.secretRef, s.db.passwordKey),
		secretEnv("YUGABYTEDB_PASSWORD", s.db.secretRef, s.db.passwordKey),
		secretEnv("YUGABYTEDB_URL", name+"-db", "YUGABYTEDB_URL"),
	}
	envFrom := []any{
		map[string]any{"configMapRef": map[string]any{"name": name}},
	}
	if spec.SecretRef != "" {
		// optional: the workload starts and reports its own missing-credential error
		// rather than being held hostage to a Secret the operator has not created yet.
		envFrom = append(envFrom, map[string]any{
			"secretRef": map[string]any{"name": spec.SecretRef, "optional": true},
		})
	}

	dep := deployment(deploymentOpts{
		name: name, ns: s.ns, labels: labels, image: spec.Image, replicas: spec.Replicas,
		port: port, env: env, envFrom: envFrom,
		readiness: map[string]any{
			"httpGet":             map[string]any{"path": "/ping", "port": port},
			"initialDelaySeconds": 10,
			"periodSeconds":       10,
			// The image is large and its first start builds mem0's schema, so give it
			// five minutes before kubelet calls it failed rather than slow.
			"failureThreshold": 30,
		},
		requests:   map[string]string{"cpu": "250m", "memory": "512Mi"},
		pullSecret: spec.ImagePullSecret,
	})
	if err := s.kc.applySSA(depPath(s.ns, name), dep); err != nil {
		return err
	}
	if err := s.applyService(name, labels, port, "http"); err != nil {
		return err
	}
	// The component row carries the database error if there was one: the workload is
	// declared and will be 0/1 until its database exists, and that is the reason.
	c := component{name: "mcp-server", kind: "Deployment", version: imageTag(spec.Image)}
	if dbErr != nil {
		c.err = dbErr.Error()
	}
	s.record(c, name)
	return nil
}

// applyRAGWorker deploys the RAG indexing worker. It has no service and no
// readiness probe of its own upstream — it is a queue consumer, so health is
// replica readiness.
func (s *stack) applyRAGWorker(r api.DesiredResource) error {
	var spec workloadSpec
	if err := decodeSpec(r.Spec, &spec); err != nil {
		return fmt.Errorf("bad rag-worker spec: %w", err)
	}
	if spec.Image == "" {
		return fmt.Errorf("rag-worker needs spec.image (e.g. %s/meko/rag-agent:<tag>)", ecrRegistry)
	}
	if s.db == nil {
		return fmt.Errorf("rag-worker needs a database resource declared alongside it")
	}
	dbName := orDefault(spec.DatabaseName, mekoDatabase)
	name := baseNameOr(r.Name, "rag-worker")
	// Ownership is recorded in reconcile() from desired state, not here: a reconciler
	// that returns early must not make its resource look undesired to the GC.
	key := s.keyFor(r.Kind, r.Name)
	labels := s.labelsFor(name, key)

	// Best-effort, as for mcp-server: the worker is declared either way and retries
	// its own connection. See applyMCPServer.
	dbErr := s.ensureDatabase(s.db, dbName)
	if dbErr != nil {
		s.pool.logOnce("rag-worker."+name, fmt.Sprintf(
			"rag-worker %s: %v — deploying anyway, it retries its own connection", name, dbErr))
	}

	cfg := map[string]string{
		"AWS_REGION":          orDefault(spec.AWSRegion, "us-east-1"),
		"TASK_LEASE_DURATION": "600",
	}
	if spec.S3Bucket != "" {
		cfg["AWS_S3_BUCKET_NAME"] = spec.S3Bucket
	}
	if err := s.kc.applySSA(cmPath(s.ns, name), map[string]any{
		"apiVersion": "v1", "kind": "ConfigMap",
		"metadata": map[string]any{"name": name, "namespace": s.ns, "labels": labels},
		"data":     cfg,
	}); err != nil {
		return err
	}
	// The worker takes a URI, not libpq keywords (see BuildRAGSecret in the
	// provisioner) — the two forms are not interchangeable.
	if err := s.kc.applySSA(secretPath(s.ns, name+"-db"), map[string]any{
		"apiVersion": "v1", "kind": "Secret",
		"metadata": map[string]any{"name": name + "-db", "namespace": s.ns, "labels": labels},
		"stringData": map[string]string{
			"YUGABYTEDB_CONNECTION_STRING": s.db.uri(dbName),
		},
	}); err != nil {
		return err
	}

	envFrom := []any{map[string]any{"configMapRef": map[string]any{"name": name}}}
	if spec.SecretRef != "" {
		envFrom = append(envFrom, map[string]any{
			"secretRef": map[string]any{"name": spec.SecretRef, "optional": true},
		})
	}
	dep := deployment(deploymentOpts{
		name: name, ns: s.ns, labels: labels, image: spec.Image, replicas: spec.Replicas,
		env: []any{
			secretEnv("YUGABYTEDB_CONNECTION_STRING", name+"-db", "YUGABYTEDB_CONNECTION_STRING"),
		},
		envFrom:    envFrom,
		requests:   map[string]string{"cpu": "250m", "memory": "512Mi"},
		pullSecret: spec.ImagePullSecret,
	})
	if err := s.kc.applySSA(depPath(s.ns, name), dep); err != nil {
		return err
	}
	c := component{name: "rag-worker", kind: "Deployment", version: imageTag(spec.Image)}
	if dbErr != nil {
		c.err = dbErr.Error()
	}
	s.record(c, name)
	return nil
}

// ensureDatabase creates a database inside the resolved data-plane database, through
// the shared connection cache.
//
// Through the cache and not a connection of its own: this runs on every tick, and a
// fresh pool per tick means a DNS lookup and an auth handshake per tick, each one
// another chance for a transient failure to stop a workload being reconciled at all.
func (s *stack) ensureDatabase(db *resolvedDB, name string) error {
	if db == nil {
		return fmt.Errorf("no database resolved")
	}
	d := s.pool.use(db.uri(db.database))
	if d == nil {
		// Not fatal to the workload: the database may simply not be accepting
		// connections yet on the tick that created its StatefulSet. The next tick tries
		// again, and the workload's own readiness gate holds until then.
		return fmt.Errorf("database %s is not accepting connections yet", db.host)
	}
	// Cheap to repeat: one SELECT on a pooled connection. The expensive part was
	// building a fresh pool per call, which the shared cache removed.
	return d.ensureDatabase(baseName(name))
}

func (s *stack) applyService(name string, labels map[string]string, port int, portName string) error {
	return s.applyExposedService(name, labels, port, portName, "")
}

// applyExposedService publishes a workload, optionally through a LoadBalancer.
//
// exposure is "" (ClusterIP, in-cluster only), "internal" (a LoadBalancer confined to the
// VPC) or "external" (a public one). The internal case needs cloud-specific annotations,
// and both clouds' are set: each ignores the other's, and the alternative is the agent
// having to know which cloud it is on, which it deliberately does not.
//
// Getting internal wrong is not a cosmetic mistake. A Service that was meant to be
// VPC-only and comes up public exposes whatever is behind it to the internet, and on GKE
// the difference is exactly one annotation.
func (s *stack) applyExposedService(name string, labels map[string]string, port int, portName, exposure string) error {
	meta := map[string]any{"name": name, "namespace": s.ns, "labels": labels}
	spec := map[string]any{
		"selector": map[string]string{"app": name},
		"ports":    []any{map[string]any{"name": portName, "port": port, "targetPort": port}},
	}
	switch exposure {
	case "", "cluster", "clusterip":
	case "internal":
		spec["type"] = "LoadBalancer"
		meta["annotations"] = map[string]string{
			"networking.gke.io/load-balancing":                    "internal",
			"cloud.google.com/load-balancer-type":                 "Internal",
			"service.beta.kubernetes.io/aws-load-balancer-scheme": "internal",
		}
	case "external":
		spec["type"] = "LoadBalancer"
	default:
		return fmt.Errorf("expose[%s] = %q is not a kind of exposure (want internal or external)", name, exposure)
	}
	return s.kc.applySSA(svcPath(s.ns, name), map[string]any{
		"apiVersion": "v1", "kind": "Service", "metadata": meta, "spec": spec,
	})
}

func (s *stack) labelsFor(name, key string) map[string]string {
	return map[string]string{
		"app":                          name,
		"app.kubernetes.io/managed-by": fieldManager,
		stackLabel:                     "true",
		stackResourceLabel:             key,
	}
}

// record reads the applied workload's real replica counts back and appends a
// component row. Reporting the desired count as healthy is how a fleet view ends
// up green while nothing is running.
func (s *stack) record(c component, workload string) {
	ready, total, err := s.kc.workloadReadiness(s.ns, c.kind, workload)
	if err != nil {
		c.err = err.Error()
	}
	c.healthy, c.total = ready, total
	s.components = append(s.components, c)
}

// report converts the components observed this tick into wire types.
func (s *stack) report() []api.Component {
	out := make([]api.Component, 0, len(s.components))
	for _, c := range s.components {
		out = append(out, api.Component{
			Name: c.name, Healthy: c.healthy, Total: c.total, Version: c.version, Error: c.err,
		})
	}
	return out
}

// stateFor derives the environment state from what is actually running.
//
// The transition an operator watches for is a stack coming up: every declared
// component reporting all replicas ready. Until then the environment is still
// PROVISIONING; if it was READY once and something has since failed, that is
// DEGRADED, which is a different problem and reads differently in the console.
func stateFor(comps []api.Component, wasReady *bool) api.EnvState {
	healthy := true
	for _, c := range comps {
		if c.Total == 0 || c.Healthy < c.Total {
			healthy = false
			break
		}
	}
	if healthy {
		*wasReady = true
		return api.StateReady
	}
	if *wasReady {
		return api.StateDegraded
	}
	return api.StateProvisioning
}

// summarize renders components as "meko-agent 1/1, postgres 1/1" for the log line
// that an operator watching a bring-up actually reads.
func summarize(comps []api.Component) string {
	parts := make([]string, 0, len(comps))
	for _, c := range comps {
		parts = append(parts, fmt.Sprintf("%s %d/%d", c.Name, c.Healthy, c.Total))
	}
	return strings.Join(parts, ", ")
}

// gc deletes stack objects whose desired resource is gone. Scoped by label to
// what this agent applied, so it can never touch an operator's own workloads in
// the same namespace.
func (s *stack) gc() {
	type target struct{ kind, path string }
	for _, t := range []target{
		{"Deployment", "/apis/apps/v1/namespaces/" + s.ns + "/deployments"},
		{"StatefulSet", "/apis/apps/v1/namespaces/" + s.ns + "/statefulsets"},
		{"Service", "/api/v1/namespaces/" + s.ns + "/services"},
		{"ConfigMap", "/api/v1/namespaces/" + s.ns + "/configmaps"},
	} {
		items, err := s.kc.listLabeled(t.path, stackLabel+"=true")
		if err != nil {
			log.Printf("gc list %s: %v", t.kind, err)
			continue
		}
		for _, it := range items {
			key := it.Metadata.Labels[stackResourceLabel]
			if key == "" || s.owned[key] {
				continue
			}
			if err := s.kc.deletePath(t.path + "/" + it.Metadata.Name); err != nil {
				log.Printf("gc %s %s: %v", t.kind, it.Metadata.Name, err)
				continue
			}
			log.Printf("GC: deleted %s %s/%s (resource %s no longer declared)", t.kind, s.ns, it.Metadata.Name, key)
		}
	}
}

// ---- small builders ----

type deploymentOpts struct {
	name, ns string
	labels   map[string]string
	image    string
	replicas int
	port     int
	// command/args override the image's entrypoint. api_server takes its database
	// configuration only as flags, so this is the only way to configure it.
	command []string
	args    []string
	// initContainers gate the workload on something outside it — a database that is
	// still starting, where the alternative is a crash-loop into a long backoff.
	initContainers []any
	env            []any
	envFrom        []any
	readiness      map[string]any
	requests       map[string]string
	pullSecret     string
}

func deployment(o deploymentOpts) map[string]any {
	if o.replicas == 0 {
		o.replicas = 1
	}
	container := map[string]any{
		"name":      o.name,
		"image":     o.image,
		"env":       o.env,
		"envFrom":   o.envFrom,
		"resources": map[string]any{"requests": o.requests},
	}
	if len(o.command) > 0 {
		container["command"] = o.command
	}
	if len(o.args) > 0 {
		container["args"] = o.args
	}
	if o.port != 0 {
		container["ports"] = []any{map[string]any{"name": "http", "containerPort": o.port}}
	}
	if o.readiness != nil {
		container["readinessProbe"] = o.readiness
	}
	podSpec := map[string]any{"containers": []any{container}}
	if len(o.initContainers) > 0 {
		podSpec["initContainers"] = o.initContainers
	}
	if o.pullSecret != "" {
		podSpec["imagePullSecrets"] = []any{map[string]any{"name": o.pullSecret}}
	}
	return map[string]any{
		"apiVersion": "apps/v1", "kind": "Deployment",
		"metadata": map[string]any{"name": o.name, "namespace": o.ns, "labels": o.labels},
		"spec": map[string]any{
			"replicas": o.replicas,
			"selector": map[string]any{"matchLabels": map[string]string{"app": o.name}},
			"template": map[string]any{
				"metadata": map[string]any{"labels": o.labels},
				"spec":     podSpec,
			},
		},
	}
}

func secretEnv(name, secret, key string) map[string]any {
	return map[string]any{
		"name": name,
		"valueFrom": map[string]any{
			"secretKeyRef": map[string]any{"name": secret, "key": key},
		},
	}
}

func readinessCommand(spec databaseSpec) []any {
	if spec.engine() == "yugabytedb" {
		return []any{"bash", "-c", fmt.Sprintf("bin/ysqlsh -h $(hostname) -p %d -U %s -c 'select 1'", spec.Port, spec.User)}
	}
	return []any{"pg_isready", "-U", spec.User, "-d", spec.Database}
}

func dataMountPath(engine string) string {
	if engine == "yugabytedb" {
		return "/home/yugabyte/var"
	}
	return "/var/lib/postgresql/data"
}

// svcFQDN is the in-cluster address of a Service, **with the trailing dot**.
//
// The dot is not cosmetic. Kubernetes injects `ndots:5` into every pod's
// resolv.conf, and `meko.ycp-agent.svc.cluster.local` has four dots — so a resolver
// treats it as a relative name and tries the whole search list first
// (`…svc.cluster.local.ycp-agent.svc.cluster.local`, and so on) before ever asking
// for the name as written. Go's resolver does exactly that, and those bogus lookups
// were enough to blow a 10-second connect budget: the database was up, reachable by
// IP, and answered instantly for the same name written with a trailing dot, while
// every connect the agent made timed out. The trailing dot marks the name absolute
// and skips the search list.
//
// It matters here more than most places, because this address is handed to everything
// the agent provisions — YUGABYTEDB_URL, the RAG worker's URI, the agent's own pool.
func svcFQDN(svc, ns string) string {
	return fmt.Sprintf("%s.%s.svc.cluster.local.", svc, ns)
}

func stsPath(ns, n string) string    { return "/apis/apps/v1/namespaces/" + ns + "/statefulsets/" + n }
func depPath(ns, n string) string    { return "/apis/apps/v1/namespaces/" + ns + "/deployments/" + n }
func svcPath(ns, n string) string    { return "/api/v1/namespaces/" + ns + "/services/" + n }
func cmPath(ns, n string) string     { return "/api/v1/namespaces/" + ns + "/configmaps/" + n }
func secretPath(ns, n string) string { return "/api/v1/namespaces/" + ns + "/secrets/" + n }

// ecrRegistry is where the private Meko images live. Named only in error
// messages, to point an operator at the right place.
const ecrRegistry = "161908593886.dkr.ecr.us-east-2.amazonaws.com"

// createTables resolves MEM0_CREATE_TABLES: whatever the spec says, else true for a
// database the agent manages (nothing else will have created the schema) and false
// for an external one (the platform's provisioning step owns it).
func createTables(spec workloadSpec, db *resolvedDB) string {
	if spec.CreateTables != nil {
		return fmt.Sprint(*spec.CreateTables)
	}
	return fmt.Sprint(db != nil && db.managed)
}

func orDefault(v, def string) string {
	if v == "" {
		return def
	}
	return v
}

// baseNameOr sanitizes a resource name into a DNS-1123 object name, falling back
// when the name sanitizes to nothing.
func baseNameOr(name, def string) string {
	b := strings.Trim(nonDNS.ReplaceAllString(strings.ToLower(name), "-"), "-")
	if b == "" {
		return def
	}
	return b
}

// imageTag is the version to report for a component: the tag, not the whole
// reference, so the console shows "2.1.0-20260730" and not a registry path.
func imageTag(image string) string {
	// Strip a digest first: an @sha256:… has no useful version in it.
	if i := strings.Index(image, "@"); i >= 0 {
		image = image[:i]
	}
	i := strings.LastIndex(image, ":")
	// A colon before the last slash is a registry port, not a tag.
	if i < 0 || strings.Contains(image[i:], "/") {
		return "latest"
	}
	return image[i+1:]
}

// generatePassword returns a URL-safe random password. Used once per environment
// for a managed database, then persisted in its Secret.
func generatePassword() (string, error) {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}
