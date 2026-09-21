package ampbackend

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"

	"meko/ycp/internal/ampclient"
	"meko/ycp/internal/api"
	"meko/ycp/reconcile"
)

// A pool declares its owner via owner_user_id, but users are not a YCP kind, so
// the agent must materialize that owner before creating the pool — otherwise the
// controller 404s on a missing owner on a fresh deploy.
func TestCreatePoolEnsuresOwnerFirst(t *testing.T) {
	const ownerID = "11111111-1111-1111-1111-111111111111"

	var hits []string
	var userBody ampclient.CreateUserRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits = append(hits, r.Method+" "+r.URL.Path)
		switch r.URL.Path {
		case "/users":
			if err := json.NewDecoder(r.Body).Decode(&userBody); err != nil {
				t.Errorf("decode user body: %v", err)
			}
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"id":"` + ownerID + `","is_admin":false}`))
		case "/storage-pools":
			w.WriteHeader(http.StatusAccepted)
			_, _ = w.Write([]byte(`{"id":"22222222-2222-2222-2222-222222222222"}`))
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	b, err := New(srv.URL, "", nil, noCtl())
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	err = b.Create(t.Context(), api.DesiredResource{
		Kind: KindPool,
		Name: "pool-a",
		Spec: json.RawMessage(`{"owner_user_id":"` + ownerID + `"}`),
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	want := []string{"POST /users", "POST /storage-pools"}
	if len(hits) != len(want) {
		t.Fatalf("request sequence = %v, want %v", hits, want)
	}
	for i := range want {
		if hits[i] != want[i] {
			t.Fatalf("request sequence = %v, want %v", hits, want)
		}
	}
	if userBody.Id == nil || userBody.Id.String() != ownerID {
		t.Fatalf("ensure-user id = %v, want %s", userBody.Id, ownerID)
	}
}

// With no owner in the spec the agent skips the user create and lets the
// controller reject the pool with its own required-field error.
func TestCreatePoolWithoutOwnerSkipsUserCreate(t *testing.T) {
	var hits []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits = append(hits, r.Method+" "+r.URL.Path)
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	b, err := New(srv.URL, "", nil, noCtl())
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	if err := b.Create(t.Context(), api.DesiredResource{
		Kind: KindPool,
		Name: "pool-a",
		Spec: json.RawMessage(`{}`),
	}); err != nil {
		t.Fatalf("Create: %v", err)
	}

	if len(hits) != 1 || hits[0] != "POST /storage-pools" {
		t.Fatalf("request sequence = %v, want [POST /storage-pools]", hits)
	}
}

// List reports each resource's kind-specific details in Attrs so YCP can render
// pg version, LSN, extensions, role and compute — not just topology and state.
func TestListReportsResourceAttrs(t *testing.T) {
	const (
		poolID     = "22222222-2222-2222-2222-222222222222"
		clusterID  = "33333333-3333-3333-3333-333333333333"
		branchID   = "44444444-4444-4444-4444-444444444444"
		endpointID = "55555555-5555-5555-5555-555555555555"
		parentID   = "66666666-6666-6666-6666-666666666666"
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(p, "/endpoints"):
			io.WriteString(w, `{"endpoints":[{"id":"`+endpointID+`","name":"rw","branch_id":"`+branchID+`","cluster_id":"`+clusterID+`","role":"read_write","address":"ep.host:5433","tailnet_address":"ep.tail:5433","resources":{"cpu":"2","memory":"4Gi"},"created_at":"2026-01-01T00:00:00Z"}]}`)
		case strings.HasSuffix(p, "/branches"):
			io.WriteString(w, `{"branches":[{"branch_id":"`+branchID+`","cluster_id":"`+clusterID+`","name":"main","is_default":false,"pg_version":18,"fork_lsn":"1/2","parent_branch_id":"`+parentID+`","extensions":["pgvector","pg_cron"],"created_at":"2026-01-01T00:00:00Z"}]}`)
		case strings.HasSuffix(p, "/clusters"):
			io.WriteString(w, `{"clusters":[{"id":"`+clusterID+`","name":"recproj","storage_pool_id":"`+poolID+`","extensions":["pgvector","pg_cron"],"created_at":"2026-01-01T00:00:00Z","default_branch":{"branch_id":"`+branchID+`","cluster_id":"`+clusterID+`","name":"main","is_default":true,"pg_version":18,"created_at":"2026-01-01T00:00:00Z"}}]}`)
		case p == "/version":
			// Every List reads the controller row; these tests are not about it.
			io.WriteString(w, `{"version":"0.0.0","git_commit":"abc1234","build_date":"2026-01-01T00:00:00Z"}`)
		case strings.HasSuffix(p, "/storage-pools"):
			io.WriteString(w, `{"storage_pools":[{"id":"`+poolID+`","name":"pool-a","owner_user_id":"11111111-1111-1111-1111-111111111111","created_at":"2026-01-01T00:00:00Z"}]}`)
		default:
			t.Errorf("unexpected request %s %s", r.Method, p)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	b, err := New(srv.URL, "", nil, noCtl())
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	actuals, err := b.List(t.Context())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	byKind := map[string]reconcile.Actual{}
	for _, a := range actuals {
		byKind[a.Key.Kind] = a
	}

	cl := byKind[KindCluster].Attrs
	if cl["id"] != clusterID || cl["pg_version"] != "18" || cl["extensions"] != "pgvector,pg_cron" {
		t.Errorf("cluster attrs = %v", cl)
	}

	br := byKind[KindBranch].Attrs
	if br["id"] != branchID || br["pg_version"] != "18" || br["fork_lsn"] != "1/2" ||
		br["parent_branch_id"] != parentID {
		t.Errorf("branch attrs = %v", br)
	}
	// The extension set is opted in per branch, so each branch reports its own
	// copy.
	if br["extensions"] != "pgvector,pg_cron" {
		t.Errorf("branch attrs extensions = %q, want pgvector,pg_cron", br["extensions"])
	}
	if _, ok := br["is_default"]; ok {
		t.Errorf("branch is_default should be absent when false, got %v", br)
	}

	ep := byKind[KindEndpoint].Attrs
	if ep["id"] != endpointID || ep["role"] != "read_write" || ep["address"] != "ep.host:5433" ||
		ep["tailnet_address"] != "ep.tail:5433" || ep["cpu"] != "2" || ep["memory"] != "4Gi" {
		t.Errorf("endpoint attrs = %v", ep)
	}
	// The row reports no connectivity — an endpoint from before it was
	// configurable — which resolves to the default rather than being omitted.
	if ep["connectivity_mode"] != "cluster_ip" {
		t.Errorf("endpoint connectivity_mode = %q, want cluster_ip", ep["connectivity_mode"])
	}
	if _, ok := ep["connectivity_allowed_cidrs"]; ok {
		t.Errorf("connectivity_allowed_cidrs should be absent under cluster_ip, got %v", ep)
	}

	if a := byKind[KindPool].Attrs; a["id"] != poolID {
		t.Errorf("pool attrs = %v, want id %s", a, poolID)
	}
}

// The console writes an endpoint's connectivity and has no other way to read back
// what the controller applied, and an nlb_* endpoint on the pod-IP fallback is
// otherwise indistinguishable from a cluster_ip one. Both halves are encoded so
// that the cidr key exists iff the mode has a load balancer, and an empty value
// there means unrestricted rather than unset.
func TestListReportsEndpointConnectivity(t *testing.T) {
	for name, tc := range map[string]struct {
		row       string // the endpoint row's `connectivity`, "" for no property
		mode      string
		cidrs     string
		wantCidrs bool
	}{
		"absent resolves to the default": {
			row: "", mode: "cluster_ip",
		},
		"an explicit default is the same thing": {
			row: `{"mode":"cluster_ip"}`, mode: "cluster_ip",
		},
		"source ranges are joined, sorted": {
			row:   `{"mode":"nlb_internal","allowed_cidrs":["10.2.0.0/16","10.0.0.0/16"]}`,
			mode:  "nlb_internal",
			cidrs: "10.0.0.0/16,10.2.0.0/16", wantCidrs: true,
		},
		// The case the whole encoding exists for: on nlb_external this is Postgres
		// on the public internet, so it must arrive as a present-but-empty key, not
		// as an absent one the console would read as "not configured".
		"unrestricted is reported, not dropped": {
			row:  `{"mode":"nlb_external"}`,
			mode: "nlb_external",
			// cidrs is the zero value: present and empty.
			wantCidrs: true,
		},
	} {
		t.Run(name, func(t *testing.T) {
			g := &genServer{endpointConnectivity: tc.row}
			b := g.start(t)

			actuals, err := b.List(t.Context())
			if err != nil {
				t.Fatalf("List: %v", err)
			}
			var attrs map[string]string
			for _, a := range actuals {
				if a.Key.Kind == KindEndpoint {
					attrs = a.Attrs
				}
			}
			if got := attrs["connectivity_mode"]; got != tc.mode {
				t.Errorf("connectivity_mode = %q, want %q", got, tc.mode)
			}
			// Comma-ok, not a bare comparison: an absent key also reads as "",
			// which would let the unrestricted case pass against attrPut's drop.
			got, ok := attrs["connectivity_allowed_cidrs"]
			if ok != tc.wantCidrs {
				t.Fatalf("connectivity_allowed_cidrs present = %v, want %v (attrs %v)", ok, tc.wantCidrs, attrs)
			}
			if ok && got != tc.cidrs {
				t.Errorf("connectivity_allowed_cidrs = %q, want %q", got, tc.cidrs)
			}
		})
	}
}

// The read-back half of the settings PATCHes (meko#1295). A declared surface owns
// the whole map, so an editor that cannot see what is stored deletes every key it
// never saw on its first write — the settings have to come back out of the
// inventory, not out of a second credentialed client against the controller.
func TestListReportsSettingsSurfaces(t *testing.T) {
	g := &genServer{
		branchSettings: `,"pg_settings":{"work_mem":"16MB","statement_timeout":"30s"}` +
			`,"write_token_settings":{"ttl_ms":90000,"renew_seconds":30}`,
		endpointSettings: `,"pg_settings":{"work_mem":"64MB"}` +
			`,"pageserver_settings":{"tenant_config.pitr_interval":"7 days","tenant_config.enable_gc":true}` +
			`,"safekeeper_settings":{}` +
			`,"extended_config":{"foo":"bar"}`,
	}
	b := g.start(t)

	actuals, err := b.List(t.Context())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	byKind := map[string]map[string]string{}
	for _, a := range actuals {
		byKind[a.Key.Kind] = a.Attrs
	}

	// One JSON object per surface, keys sorted by the encoder, so the value is
	// stable tick to tick and a consumer parses the surface whole — the unit the
	// PATCH replaces.
	for attr, want := range map[string]string{
		"pg_settings":          `{"statement_timeout":"30s","work_mem":"16MB"}`,
		"write_token_settings": `{"renew_seconds":30,"ttl_ms":90000}`,
	} {
		if got := byKind[KindBranch][attr]; got != want {
			t.Errorf("branch %s = %q, want %q", attr, got, want)
		}
	}
	for attr, want := range map[string]string{
		"pg_settings":         `{"work_mem":"64MB"}`,
		"pageserver_settings": `{"tenant_config.enable_gc":true,"tenant_config.pitr_interval":"7 days"}`,
		// Explicitly `{}` on the row, which must read the same as an absent one:
		// nothing is stored.
		"safekeeper_settings": `{}`,
		// Reported even though the settings PATCH cannot write it — a spec that
		// disagrees is permanent drift with nothing to show what it disagrees with.
		"extended_config": `{"foo":"bar"}`,
	} {
		if got := byKind[KindEndpoint][attr]; got != want {
			t.Errorf("endpoint %s = %q, want %q", attr, got, want)
		}
	}
}

// Skew, the rule meko#1252 set for connectivity: an absent key means the agent does
// not report this surface, never that the surface is empty. So every surface is
// written on every tick, and a row carrying none reports "{}" rather than dropping
// the key — a consumer reading absence as empty is the destructive save this whole
// read-back exists to prevent.
func TestListReportsUnsetSettingsAsEmptyObject(t *testing.T) {
	g := &genServer{}
	b := g.start(t)

	actuals, err := b.List(t.Context())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	byKind := map[string]map[string]string{}
	for _, a := range actuals {
		byKind[a.Key.Kind] = a.Attrs
	}

	for kind, attrs := range map[string][]string{
		KindBranch:   {"pg_settings", "write_token_settings"},
		KindEndpoint: {"pg_settings", "pageserver_settings", "safekeeper_settings", "extended_config"},
	} {
		for _, attr := range attrs {
			// Comma-ok, not a bare comparison: an absent key also reads as "" and
			// would pass a value check against the empty string.
			got, ok := byKind[kind][attr]
			if !ok {
				t.Errorf("%s %s absent, want %q — absence is reserved for an agent that does not report settings", kind, attr, "{}")
				continue
			}
			if got != "{}" {
				t.Errorf("%s %s = %q, want %q", kind, attr, got, "{}")
			}
		}
	}
}

// A 400 says the controller refuses the spec as declared — an endpoint memory
// quantity with no unit is the case that prompted this — so the same body would
// be rejected on every retry. It must come back as a permanent error the engine
// surfaces as drift, not a transient one it backs off on forever.
func TestBadRequestIsPermanent(t *testing.T) {
	const (
		poolID    = "22222222-2222-2222-2222-222222222222"
		clusterID = "33333333-3333-3333-3333-333333333333"
		branchID  = "44444444-4444-4444-4444-444444444444"
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/endpoints") && r.Method == http.MethodPost:
			w.WriteHeader(http.StatusBadRequest)
			io.WriteString(w, `{"error":{"code":"invalid_request","message":"resources.memory must be a Kubernetes memory quantity with an explicit unit"}}`)
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	b, err := New(srv.URL, "", nil, noCtl())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	idx := newIndex()
	idx.pool["pool-a"] = uuid.MustParse(poolID)
	idx.cluster["pool-a/cl"] = uuid.MustParse(clusterID)
	idx.branch["pool-a/cl/main"] = uuid.MustParse(branchID)
	b.idx = idx

	err = b.Create(t.Context(), api.DesiredResource{
		Kind: KindEndpoint,
		Name: "pool-a/cl/main/ep-rw",
		Spec: json.RawMessage(`{"role":"read_write","resources":{"memory":"4"}}`),
	})
	if err == nil {
		t.Fatal("Create with a rejected spec returned no error")
	}
	var perm *reconcile.PermanentError
	if !errors.As(err, &perm) {
		t.Errorf("Create error = %v (%T), want a *reconcile.PermanentError — a retried 400 never clears", err, err)
	}
	if !strings.Contains(err.Error(), "explicit unit") {
		t.Errorf("Create error = %v, want the controller's message carried through", err)
	}
}

// calls records what a test's apply verbs sent to the fake controller.
type calls struct {
	posts   []string                                // method+path of every mutating request
	added   []ampclient.AddBranchExtensionsRequest  // addBranchExtensions bodies, in order
	patched []ampclient.UpdateBranchSettingsRequest // updateBranchSettings bodies, in order
}

// listedTree stands up a controller serving one pool -> cluster -> branch whose
// branch has the given extension set, runs List (which is what populates the id
// index the apply verbs resolve against), and returns the backend plus what the
// test's apply verbs go on to send.
func listedTree(t *testing.T, branchExts []string) (*Backend, *calls) {
	t.Helper()
	const (
		poolID    = "22222222-2222-2222-2222-222222222222"
		clusterID = "33333333-3333-3333-3333-333333333333"
		branchID  = "44444444-4444-4444-4444-444444444444"
	)
	exts, err := json.Marshal(branchExts)
	if err != nil {
		t.Fatalf("marshal extensions: %v", err)
	}
	got := &calls{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		if r.Method != http.MethodGet {
			got.posts = append(got.posts, r.Method+" "+p)
		}
		switch {
		case strings.HasSuffix(p, "/branches") && r.Method == http.MethodPost:
			w.WriteHeader(http.StatusAccepted)
			io.WriteString(w, `{}`)
		case strings.HasSuffix(p, "/settings"):
			if want := "/storage-pools/" + poolID + "/clusters/" + clusterID + "/branches/" + branchID + "/settings"; p != want {
				t.Errorf("updateBranchSettings path = %s, want %s", p, want)
			}
			var body ampclient.UpdateBranchSettingsRequest
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Errorf("decode branch settings body: %v", err)
			}
			got.patched = append(got.patched, body)
			io.WriteString(w, `{"applied":true}`)
		case strings.HasSuffix(p, "/extensions"):
			if want := "/storage-pools/" + poolID + "/clusters/" + clusterID + "/branches/" + branchID + "/extensions"; p != want {
				t.Errorf("addBranchExtensions path = %s, want %s", p, want)
			}
			var body ampclient.AddBranchExtensionsRequest
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Errorf("decode add-extensions body: %v", err)
			}
			got.added = append(got.added, body)
			w.WriteHeader(http.StatusAccepted)
			io.WriteString(w, `{}`)
		case strings.HasSuffix(p, "/endpoints"):
			io.WriteString(w, `{"endpoints":[]}`)
		case strings.HasSuffix(p, "/branches"):
			io.WriteString(w, `{"branches":[{"branch_id":"`+branchID+`","cluster_id":"`+clusterID+`","name":"main","is_default":true,"pg_version":18,"extensions":`+string(exts)+`,"pg_settings":{"work_mem":"32MB"},"created_at":"2026-01-01T00:00:00Z"}]}`)
		case strings.HasSuffix(p, "/clusters"):
			io.WriteString(w, `{"clusters":[{"id":"`+clusterID+`","name":"recproj","storage_pool_id":"`+poolID+`","created_at":"2026-01-01T00:00:00Z","default_branch":{"branch_id":"`+branchID+`","cluster_id":"`+clusterID+`","name":"main","is_default":true,"pg_version":18,"created_at":"2026-01-01T00:00:00Z"}}]}`)
		case p == "/version":
			// Every List reads the controller row; these tests are not about it.
			io.WriteString(w, `{"version":"0.0.0","git_commit":"abc1234","build_date":"2026-01-01T00:00:00Z"}`)
		case strings.HasSuffix(p, "/storage-pools"):
			io.WriteString(w, `{"storage_pools":[{"id":"`+poolID+`","name":"pool-a","owner_user_id":"11111111-1111-1111-1111-111111111111","created_at":"2026-01-01T00:00:00Z"}]}`)
		default:
			t.Errorf("unexpected request %s %s", r.Method, p)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)

	b, err := New(srv.URL, "", nil, noCtl())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := b.List(t.Context()); err != nil {
		t.Fatalf("List: %v", err)
	}
	return b, got
}

func branchRes(spec string) api.DesiredResource {
	return api.DesiredResource{Kind: KindBranch, Name: "pool-a/recproj/main", Spec: json.RawMessage(spec)}
}

// branchResGen is branchRes at a generation, for the tests that care which call
// carried it.
func branchResGen(spec string, gen int64) api.DesiredResource {
	r := branchRes(spec)
	r.Generation = gen
	return r
}

// Updating a branch enables only the declared extensions the controller does not
// already report: the union is idempotent, but every add redeploys the branch's
// endpoints, so re-sending a name that is already on would bounce compute for
// nothing.
func TestUpdateBranchAddsOnlyMissingExtensions(t *testing.T) {
	b, got := listedTree(t, []string{"pgvector"})

	if err := b.Update(t.Context(), branchRes(`{"extensions":["pgvector","pg_cron","age"]}`)); err != nil {
		t.Fatalf("Update: %v", err)
	}
	if len(got.added) != 1 {
		t.Fatalf("addBranchExtensions calls = %d, want 1", len(got.added))
	}
	if got := got.added[0].Extensions; len(got) != 2 || got[0] != "age" || got[1] != "pg_cron" {
		t.Errorf("added extensions = %v, want [age pg_cron]", got)
	}
}

// A branch already carrying everything its spec declares is converged: no call,
// so declaring a name a fork already inherited does not redeploy its endpoints.
func TestUpdateBranchAlreadyConvergedIssuesNoCall(t *testing.T) {
	b, got := listedTree(t, []string{"pgvector", "pg_cron"})

	if err := b.Update(t.Context(), branchRes(`{"extensions":["pg_cron","pgvector"],"pg_settings":{"work_mem":"32MB"}}`)); err != nil {
		t.Fatalf("Update: %v", err)
	}
	if len(got.added) != 0 {
		t.Fatalf("addBranchExtensions calls = %d, want 0", len(got.added))
	}
}

// Converged extensions must not make the rest of the spec a silent success. A
// generation bump that edited pg_settings reaches Update with nothing to add, and
// reporting nil there without issuing anything would latch the generation as
// applied and lose the edit. pg_settings is now work rather than drift, so the
// edit goes out as a PATCH — and, with no extension add to follow it, that PATCH
// is what carries the generation.
func TestUpdateBranchAppliesPgSettingsWithNothingToAdd(t *testing.T) {
	b, got := listedTree(t, []string{"pgvector"})

	if err := b.Update(t.Context(), branchResGen(`{"extensions":["pgvector"],"pg_settings":{"work_mem":"64MB"}}`, 4)); err != nil {
		t.Fatalf("Update: %v", err)
	}
	if len(got.patched) != 1 {
		t.Fatalf("updateBranchSettings calls = %d, want 1", len(got.patched))
	}
	if got.patched[0].PgSettings == nil || (*got.patched[0].PgSettings)["work_mem"] != "64MB" {
		t.Errorf("patched pg_settings = %v, want work_mem=64MB", got.patched[0].PgSettings)
	}
	if gen := got.patched[0].YcpGeneration; gen == nil || *gen != 4 {
		t.Errorf("patched ycp_generation = %v, want 4 — the PATCH is the only call this tick", gen)
	}
	if len(got.added) != 0 {
		t.Errorf("addBranchExtensions calls = %d, want 0 — the extension set was already converged", len(got.added))
	}
}

// The work a spec does ask for still goes out when another field of the same spec
// is unfixable, so one bad field does not block the rest. parent_branch_id is the
// unfixable one here: it is fixed at the fork and no verb moves it.
func TestUpdateBranchAppliesWorkBeforeReportingDrift(t *testing.T) {
	b, got := listedTree(t, nil)

	err := b.Update(t.Context(), branchRes(
		`{"extensions":["pgvector"],"pg_settings":{"work_mem":"64MB"},`+
			`"parent_branch_id":"99999999-9999-9999-9999-999999999999"}`))
	var perm *reconcile.PermanentError
	if !errors.As(err, &perm) {
		t.Fatalf("Update err = %v, want a PermanentError", err)
	}
	if !strings.Contains(err.Error(), "parent_branch_id") {
		t.Errorf("Update err = %v, want it to name parent_branch_id", err)
	}
	if len(got.added) != 1 || got.added[0].Extensions[0] != "pgvector" {
		t.Errorf("added = %v, want pgvector issued despite the drift", got.added)
	}
	if len(got.patched) != 1 {
		t.Errorf("updateBranchSettings calls = %d, want 1 — the settings edit goes out too", len(got.patched))
	}
	if gen := got.patched[0].YcpGeneration; gen != nil {
		t.Errorf("patched ycp_generation = %d, want none — this spec never fully lands", *gen)
	}
}

// A branch's set only grows, so a name dropped from the spec is drift the agent
// reports and never retries — but the adds the same spec asks for still go out.
func TestUpdateBranchReportsUnremovableExtensionAsPermanent(t *testing.T) {
	b, got := listedTree(t, []string{"pgvector"})

	err := b.Update(t.Context(), branchRes(`{"extensions":["pg_cron"]}`))
	var perm *reconcile.PermanentError
	if !errors.As(err, &perm) {
		t.Fatalf("Update err = %v, want a PermanentError", err)
	}
	if !strings.Contains(err.Error(), "pgvector") {
		t.Errorf("Update err = %v, want it to name pgvector", err)
	}
	if len(got.added) != 1 || got.added[0].Extensions[0] != "pg_cron" {
		t.Errorf("added = %v, want the declared pg_cron issued anyway", got.added)
	}
}

// A branch spec declaring only settings converges on the PATCH alone, planning no
// task at all. It used to be permanent drift: extensions were the only branch
// field the agent could apply.
func TestUpdateBranchWithOnlySettingsConverges(t *testing.T) {
	b, got := listedTree(t, nil)

	if err := b.Update(t.Context(), branchResGen(`{"pg_settings":{"work_mem":"64MB"}}`, 4)); err != nil {
		t.Fatalf("Update: %v", err)
	}
	if len(got.patched) != 1 {
		t.Fatalf("updateBranchSettings calls = %d, want 1", len(got.patched))
	}
	if gen := got.patched[0].YcpGeneration; gen == nil || *gen != 4 {
		t.Errorf("patched ycp_generation = %v, want 4", gen)
	}
	if len(got.added) != 0 {
		t.Fatalf("addBranchExtensions calls = %d, want 0", len(got.added))
	}
}

// The surface is replaced, not merged: a key the row carries and the spec does not
// is dropped, because desired state says what the surface IS. The fake's branch
// reports work_mem=32MB, which this spec does not declare.
func TestUpdateBranchSettingsReplaceTheWholeSurface(t *testing.T) {
	b, got := listedTree(t, nil)

	if err := b.Update(t.Context(), branchRes(`{"pg_settings":{"shared_buffers":"1GB"}}`)); err != nil {
		t.Fatalf("Update: %v", err)
	}
	if len(got.patched) != 1 {
		t.Fatalf("updateBranchSettings calls = %d, want 1", len(got.patched))
	}
	sent := *got.patched[0].PgSettings
	if len(sent) != 1 || sent["shared_buffers"] != "1GB" {
		t.Errorf("patched pg_settings = %v, want exactly the declared map — work_mem is pruned", sent)
	}
}

// A surface the row already matches is left out of the body entirely: an omitted
// field means "do not touch", and re-sending an identical one would push a reload
// to every endpoint on the branch for a change that is not one. With nothing else
// to send and the generation already recorded, no call goes out at all.
func TestUpdateBranchSkipsSettingsAlreadyStored(t *testing.T) {
	b, got := listedTree(t, nil)

	if err := b.Update(t.Context(), branchRes(`{"pg_settings":{"work_mem":"32MB"}}`)); err != nil {
		t.Fatalf("Update: %v", err)
	}
	if len(got.patched) != 0 {
		t.Errorf("updateBranchSettings calls = %d, want 0 — the row already carries it", len(got.patched))
	}
}

// write_token_settings reaches a branch only through this PATCH: no create verb
// carries it, and nothing inherits it on a fork.
func TestUpdateBranchAppliesWriteTokenSettings(t *testing.T) {
	b, got := listedTree(t, nil)

	if err := b.Update(t.Context(), branchRes(`{"write_token_settings":{"ttl_ms":30000,"renew_seconds":10}}`)); err != nil {
		t.Fatalf("Update: %v", err)
	}
	if len(got.patched) != 1 || got.patched[0].WriteTokenSettings == nil {
		t.Fatalf("patched = %+v, want one call carrying write_token_settings", got.patched)
	}
	if got.patched[0].PgSettings != nil {
		t.Errorf("patched pg_settings = %v, want it absent — an undeclared surface is not touched", got.patched[0].PgSettings)
	}
}

// Extensions are not a create-time field, so a spec that declares them at branch
// create is refused outright instead of being dropped into CreateBranchRequest,
// which has no place for it. Permanent, so the engine reports the reason and
// stops rather than retrying a create that can never carry them.
func TestCreateBranchRefusesExtensions(t *testing.T) {
	b, got := listedTree(t, nil)

	err := b.Create(t.Context(), branchRes(`{"extensions":["pgvector"]}`))
	var perm *reconcile.PermanentError
	if !errors.As(err, &perm) {
		t.Fatalf("Create err = %v, want a PermanentError", err)
	}
	if !strings.Contains(err.Error(), "once the branch exists") {
		t.Errorf("Create err = %v, want it to say when extensions can be declared", err)
	}
	if len(got.posts) != 0 {
		t.Errorf("requests = %v, want none — the branch must not be created either", got.posts)
	}
}

// A branch spec with no extensions still creates normally — the fork inherits
// its parent's set, which is the only way a new branch gets one.
// write_token_settings is refused at create for the same reason and in the same
// shape: updateBranchSettings needs a branch id this call has not produced, and
// CreateBranchRequest has no field for it — so letting it through would drop a
// declared write lease with no error anywhere.
func TestCreateBranchRefusesWriteTokenSettings(t *testing.T) {
	b, got := listedTree(t, nil)

	err := b.Create(t.Context(), branchRes(`{"write_token_settings":{"ttl_ms":30000}}`))
	var perm *reconcile.PermanentError
	if !errors.As(err, &perm) {
		t.Fatalf("Create err = %v, want a PermanentError", err)
	}
	if !strings.Contains(err.Error(), "once the branch exists") {
		t.Errorf("Create err = %v, want it to say when it can be declared", err)
	}
	if len(got.posts) != 0 {
		t.Errorf("requests = %v, want none — the branch must not be created either", got.posts)
	}
}

func TestCreateBranchWithoutExtensionsProceeds(t *testing.T) {
	b, got := listedTree(t, nil)

	if err := b.Create(t.Context(), branchRes(`{"pg_settings":{"work_mem":"64MB"}}`)); err != nil {
		t.Fatalf("Create: %v", err)
	}
	want := "POST /storage-pools/22222222-2222-2222-2222-222222222222/clusters/33333333-3333-3333-3333-333333333333/branches"
	if len(got.posts) != 1 || got.posts[0] != want {
		t.Errorf("requests = %v, want [%s]", got.posts, want)
	}
}
