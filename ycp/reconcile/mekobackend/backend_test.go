package mekobackend

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"meko/ycp/internal/api"
	"meko/ycp/reconcile"
)

const dpID = "11111111-1111-1111-1111-111111111111"

// api_server responses. The reconciler is the api-server's only caller, so
// everything it returns is the agent's to converge — there is no "someone else's
// row" to distinguish.
const (
	agentYCP   = `{"agent_id":"ycp","user_id":"u1"}`
	agentOther = `{"agent_id":"meko_agent","user_id":"u1"}`
	packSmoke  = `{"datapack_id":"` + dpID + `","datapack_name":"smoke","created_by_agent_id":"ycp"}`
	packOther  = `{"datapack_id":"22222222-2222-2222-2222-222222222222","datapack_name":"legacy","created_by_agent_id":"meko_agent"}`
	packUnattr = `{"datapack_id":"33333333-3333-3333-3333-333333333333","datapack_name":"orphan"}`
)

// serve runs h as the api-server and returns a Backend pointed at it.
func serve(t *testing.T, h http.HandlerFunc) *Backend {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return New(srv.URL, "pat-token", srv.Client())
}

// listing answers /agents and /datapacks with the given rows and lets the caller
// handle everything else.
func listing(t *testing.T, agents, packs string, extra http.HandlerFunc) *Backend {
	t.Helper()
	return serve(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/agents":
			_, _ = w.Write([]byte(`{"data":[` + agents + `]}`))
		case r.Method == http.MethodGet && r.URL.Path == "/datapacks":
			_, _ = w.Write([]byte(`{"data":[` + packs + `]}`))
		default:
			extra(w, r)
		}
	})
}

func isPermanent(err error) bool {
	var p *reconcile.PermanentError
	return errors.As(err, &p)
}

func reject(t *testing.T) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("no request should be issued, got %s %s", r.Method, r.URL.Path)
	}
}

// The agent is authoritative over the inventory, as it is over an amp controller's:
// the api-server has no other caller, so everything it holds is reported and
// anything desired state stops declaring is collected. The one exception is the
// account's signup agent and the rows attributed to it — see
// TestTheSignupAgentIsNeverInTheInventory.
func TestListReportsTheInventory(t *testing.T) {
	b := listing(t, agentYCP+","+agentOther, packSmoke+","+packOther+","+packUnattr, reject(t))
	got, err := b.List(t.Context())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	var keys []string
	for _, a := range got {
		keys = append(keys, a.Key.String())
	}
	want := []string{"agent/ycp", "datapack/ycp/smoke"}
	if strings.Join(keys, ",") != strings.Join(want, ",") {
		t.Fatalf("List = %v, want %v", keys, want)
	}
	// The datapack hangs off its agent, which is what makes the engine create the
	// agent first and gate the datapack on it.
	for _, a := range got {
		if a.Key.Kind == KindDatapack && a.Parent.Kind != KindAgent {
			t.Errorf("%s has no agent parent", a.Key)
		}
		// Synchronous verbs mean there is nothing to echo; a nonzero value would
		// claim an echo this backend has not got.
		if a.ObservedGen != 0 || !a.Ready {
			t.Errorf("%s: ObservedGen=%d Ready=%v", a.Key, a.ObservedGen, a.Ready)
		}
	}
}

// A failed List must abort the tick rather than read as an empty world.
func TestListErrorIsReturned(t *testing.T) {
	b := serve(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte(`<html>gateway</html>`))
	})
	if _, err := b.List(t.Context()); err == nil {
		t.Fatal("List must return the failure, not an empty inventory")
	}
}

// The agent id is the resource's name, so a create is exactly that and nothing
// else — there is no server-assigned identity to reconcile against.
func TestCreateAgentUsesTheResourceName(t *testing.T) {
	var body map[string]any
	b := serve(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/agents" || r.Method != http.MethodPost {
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"data":{"agent_id":"ycp"}}`))
	})
	if err := b.Create(t.Context(), api.DesiredResource{Kind: KindAgent, Name: "ycp"}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if body["agent_id"] != "ycp" {
		t.Errorf("agent_id = %v", body["agent_id"])
	}
}

// The datapack takes its agent from the parent path, never from the spec: one set
// from desired state could hand a datapack to an agent the engine does not manage.
func TestCreateDatapackUsesTheParentAgent(t *testing.T) {
	var body map[string]any
	b := serve(t, func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer pat-token" {
			t.Errorf("Authorization = %q", got)
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"data":{"datapack_id":"` + dpID + `"}}`))
	})
	err := b.Create(t.Context(), api.DesiredResource{
		Kind: KindDatapack, Name: "ycp/smoke",
		Spec: json.RawMessage(`{"description":"d"}`),
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if body["name"] != "smoke" || body["agent_id"] != "ycp" {
		t.Errorf("create body = %v, want name=smoke agent_id=ycp", body)
	}
	if _, ok := body["user_id"]; ok {
		t.Error("create body must not carry user_id")
	}
}

// A name of the wrong shape would make Create and List disagree about what the
// resource is called — a create and a delete every tick, each create permanently
// spending free-tier quota.
func TestNamesMustMatchTheirKindsShape(t *testing.T) {
	b := serve(t, reject(t))
	for _, tc := range []struct{ kind, name string }{
		{KindAgent, "ycp/extra"},
		{KindDatapack, "smoke"},
		{KindDatapack, "a/b/c"},
		{KindDatapack, "/smoke"},
	} {
		err := b.Create(t.Context(), api.DesiredResource{Kind: tc.kind, Name: tc.name})
		if !isPermanent(err) {
			t.Errorf("Create(%s %q) = %v, want permanent", tc.kind, tc.name, err)
		}
	}
}

func TestParentLinksDatapackToItsAgent(t *testing.T) {
	b := serve(t, reject(t))
	if _, ok := b.Parent(api.DesiredResource{Kind: KindAgent, Name: "ycp"}); ok {
		t.Error("an agent must be a root")
	}
	p, ok := b.Parent(api.DesiredResource{Kind: KindDatapack, Name: "ycp/smoke"})
	if !ok || p.Kind != KindAgent || p.Name != "ycp" {
		t.Errorf("datapack parent = %v %v, want agent/ycp", p, ok)
	}
}

// Clearing a description cannot be applied: the field is omitempty and the
// api-server writes one only when non-empty, so the PATCH is a no-op. Returning nil
// would cache the bumped generation against work that never happened.
func TestClearingADescriptionIsDrift(t *testing.T) {
	packWithDesc := `{"datapack_id":"` + dpID + `","datapack_name":"smoke","description":"still here","created_by_agent_id":"ycp"}`
	b := listing(t, agentYCP, packWithDesc, reject(t))
	if _, err := b.List(t.Context()); err != nil {
		t.Fatalf("List: %v", err)
	}
	err := b.Update(t.Context(), api.DesiredResource{
		Kind: KindDatapack, Name: "ycp/smoke", Generation: 2, Spec: json.RawMessage(`{}`),
	})
	if !isPermanent(err) || !strings.Contains(err.Error(), "still here") {
		t.Fatalf("Update = %v, want drift naming the current description", err)
	}
}

// An agent has no updatable field, so a bumped generation can never be satisfied —
// drift, not an endless retry. Same shape as ampbackend reporting a pool update.
func TestAgentUpdateIsPermanentDrift(t *testing.T) {
	b := serve(t, reject(t))
	err := b.Update(t.Context(), api.DesiredResource{Kind: KindAgent, Name: "ycp", Generation: 2})
	if !isPermanent(err) {
		t.Fatalf("Update = %v, want permanent", err)
	}
}

func TestDeleteUsesTheListedIdsAndTreats404AsSuccess(t *testing.T) {
	var paths []string
	b := listing(t, agentYCP, packSmoke, func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.Method+" "+r.URL.Path)
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":{"code":"NOT_FOUND","message":"gone"}}`))
	})
	if _, err := b.List(t.Context()); err != nil {
		t.Fatalf("List: %v", err)
	}
	if err := b.Delete(t.Context(), reconcile.ResourceKey{Kind: KindDatapack, Name: "ycp/smoke"}); err != nil {
		t.Errorf("Delete datapack = %v, want 404 treated as success", err)
	}
	if err := b.Delete(t.Context(), reconcile.ResourceKey{Kind: KindAgent, Name: "ycp"}); err != nil {
		t.Errorf("Delete agent = %v, want 404 treated as success", err)
	}
	// The datapack twice: once directly, once through the agent's cascade. A 404 on
	// the second is exactly the already-gone case this asserts.
	want := "DELETE /datapacks/" + dpID + ",DELETE /datapacks/" + dpID + ",DELETE /agents/ycp"
	if strings.Join(paths, ",") != want {
		t.Errorf("paths = %v, want %v", paths, want)
	}
}

// Rejections that can never succeed belong on the resource, not inside a backoff.
// 402 is the counter-intuitive one: the free-tier gate counts lifetime creations
// and never refunds them, so withdrawing a datapack frees nothing.
//
// Which reason each carries matters as much as that they are permanent: the state
// is what the console picks a sentence from, and telling someone to re-declare a
// spec that a revoked token refused wastes their time (CLOUDGA-36992).
func TestUnsatisfiableRejectionsArePermanent(t *testing.T) {
	for _, tc := range []struct {
		name  string
		code  int
		state string
	}{
		{"validation", http.StatusBadRequest, "rejected"},
		{"conflict", http.StatusConflict, "rejected"},
		// blocked, not rejected: the counter is lifetime, so no rewrite clears it.
		{"free-tier cap", http.StatusPaymentRequired, "blocked"},
		{"unauthorized", http.StatusUnauthorized, "blocked"},
		{"forbidden", http.StatusForbidden, "blocked"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := serve(t, func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.code)
				_, _ = w.Write([]byte(`{"error":{"code":"X","message":"no"}}`))
			})
			err := b.Create(t.Context(), api.DesiredResource{Kind: KindAgent, Name: "ycp"})
			if !isPermanent(err) {
				t.Fatalf("Create = %v, want permanent", err)
			}
			if got := reconcile.PermanentReasonOf(err).State(); got != tc.state {
				t.Errorf("state = %q, want %q", got, tc.state)
			}
		})
	}
}

// A name the backend will not take and a spec that does not parse are the
// operator's to fix by re-declaring, so they report as rejected — unlike a kind
// with no verb, which reports as drift and no rewrite can help.
func TestPermanentReasonsAreDistinguished(t *testing.T) {
	b := listing(t, agentYCP, packSmoke, nil)

	badName := b.Create(t.Context(), api.DesiredResource{Kind: KindDatapack, Name: "no-slash"})
	if got := reconcile.PermanentReasonOf(badName).State(); got != "rejected" {
		t.Errorf("bad name = %v (state %q), want rejected", badName, got)
	}

	badSpec := b.Update(t.Context(), api.DesiredResource{
		Kind: KindDatapack, Name: "ycp/smoke", Spec: []byte(`{"description":`),
	})
	if got := reconcile.PermanentReasonOf(badSpec).State(); got != "rejected" {
		t.Errorf("bad spec = %v (state %q), want rejected", badSpec, got)
	}

	unknown := b.Update(t.Context(), api.DesiredResource{Kind: "nope", Name: "x"})
	if got := reconcile.PermanentReasonOf(unknown).State(); got != "drift" {
		t.Errorf("unknown kind = %v (state %q), want drift", unknown, got)
	}
}

// Every kind in allKinds must be dispatched by every verb; one added without a case
// reaches the unknown-kind arm and fails at runtime on a resource an operator
// legitimately declared.
func TestEveryKindIsDispatched(t *testing.T) {
	b := serve(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"data":{"datapack_id":"` + dpID + `","agent_id":"ycp"}}`))
	})
	names := map[string]string{KindAgent: "ycp", KindDatapack: "ycp/smoke"}
	for _, kind := range allKinds {
		r := api.DesiredResource{Kind: kind, Name: names[kind]}
		for verb, err := range map[string]error{
			"Create": b.Create(t.Context(), r),
			"Update": b.Update(t.Context(), r),
			"Delete": b.Delete(t.Context(), reconcile.ResourceKey{Kind: kind, Name: names[kind]}),
		} {
			if err != nil && strings.Contains(err.Error(), "unknown kind") {
				t.Errorf("%s(%s): %v", verb, kind, err)
			}
		}
	}
}

func TestUnknownKindIsPermanentOnEveryVerb(t *testing.T) {
	b := serve(t, reject(t))
	r := api.DesiredResource{Kind: "knowledge-base", Name: "ycp/smoke/docs"}
	if err := b.Create(t.Context(), r); !isPermanent(err) {
		t.Errorf("Create: %v", err)
	}
	if err := b.Update(t.Context(), r); !isPermanent(err) {
		t.Errorf("Update: %v", err)
	}
	if err := b.Delete(t.Context(), reconcile.ResourceKey{Kind: "knowledge-base", Name: "x"}); !isPermanent(err) {
		t.Errorf("Delete: %v", err)
	}
}

// The engine skips a child's delete when the parent is going too, on the contract
// that the backend cascades (loop.go:199, engine.go:47). meko's DeleteAgent does
// not — it drops the agent_access and agents rows and leaves the datapacks pointing
// at an id that no longer exists — so this backend has to. Without the cascade,
// withdrawing an agent and its datapacks together orphans them permanently: the next
// List cannot see them, so no later tick collects them, and each holds a free-tier
// creation slot that is never refunded.
func TestDeletingAnAgentCascadesToItsDatapacks(t *testing.T) {
	packs := packSmoke + `,{"datapack_id":"44444444-4444-4444-4444-444444444444","datapack_name":"other","created_by_agent_id":"ycp"}`
	var paths []string
	b := listing(t, agentYCP, packs, func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.Method+" "+r.URL.Path)
		_, _ = w.Write([]byte(`{"data":{}}`))
	})
	if _, err := b.List(t.Context()); err != nil {
		t.Fatalf("List: %v", err)
	}
	if err := b.Delete(t.Context(), reconcile.ResourceKey{Kind: KindAgent, Name: "ycp"}); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	// Datapacks first, then the agent: the other order leaves rows referencing an
	// agent the api-server has already dropped.
	want := "DELETE /datapacks/44444444-4444-4444-4444-444444444444" +
		",DELETE /datapacks/" + dpID +
		",DELETE /agents/ycp"
	if strings.Join(paths, ",") != want {
		t.Errorf("cascade order = %v,\n want %v", paths, want)
	}
}

// meko_agent is the row every account gets at signup, and the id the share and
// invite grants are written under. Reporting it would let desired state that does
// not name it plan a Delete, and DeleteAgent drops the account's agent_access rows —
// silently revoking every datapack shared into the account and breaking every later
// create that names no agent. It is account infrastructure, so it stays out of the
// inventory entirely, the way amp declines to model users as a kind.
func TestTheSignupAgentIsNeverInTheInventory(t *testing.T) {
	packs := packSmoke + "," + packOther + "," + packUnattr
	b := listing(t, agentYCP+","+agentOther, packs, reject(t))
	got, err := b.List(t.Context())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	var keys []string
	for _, a := range got {
		keys = append(keys, a.Key.String())
	}
	want := []string{"agent/ycp", "datapack/ycp/smoke"}
	if strings.Join(keys, ",") != strings.Join(want, ",") {
		t.Fatalf("List = %v, want %v — the signup agent and its rows must be invisible", keys, want)
	}
}

// And a spec cannot reintroduce the hazard by declaring it.
func TestTheSignupAgentCannotBeDeclared(t *testing.T) {
	b := serve(t, reject(t))
	for _, r := range []api.DesiredResource{
		{Kind: KindAgent, Name: defaultAgentID},
		{Kind: KindDatapack, Name: defaultAgentID + "/smoke"},
	} {
		if err := b.Create(t.Context(), r); !isPermanent(err) {
			t.Errorf("Create(%s %q) = %v, want permanent", r.Kind, r.Name, err)
		}
	}
}
