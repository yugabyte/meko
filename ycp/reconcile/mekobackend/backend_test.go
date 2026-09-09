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

const (
	dpID = "11111111-1111-1111-1111-111111111111"
	// The ownership mark: only datapacks carrying it are this backend's.
	agentID = "ycp-reconcile-agent"
)

// serve runs h as the api-server and returns a Backend pointed at it.
func serve(t *testing.T, h http.HandlerFunc) *Backend {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	b, err := New(srv.URL, "pat-token", agentID, srv.Client())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return b
}

// listOne is an api-server serving exactly one owned datapack, so a test that
// only needs the index populated does not restate the envelope.
func listOne(t *testing.T, name string, extra func(w http.ResponseWriter, r *http.Request)) *Backend {
	t.Helper()
	return serve(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == "/datapacks" {
			_, _ = w.Write([]byte(`{"data":[{"datapack_id":"` + dpID +
				`","datapack_name":"` + name + `","created_by_agent_id":"` + agentID + `"}]}`))
			return
		}
		extra(w, r)
	})
}

func isPermanent(err error) bool {
	var p *reconcile.PermanentError
	return errors.As(err, &p)
}

// The token is the boundary: List reports the service user's datapacks and the
// engine deletes anything else, so what List returns is the whole managed world.
// This asserts the shape the engine consumes — key, readiness and the attrs the
// console renders.
func TestListReportsEveryDatapackAsAReadyRoot(t *testing.T) {
	b := serve(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"data":[
			{"datapack_id":"` + dpID + `","datapack_name":"beta","description":"d","memory_count":3,
			 "created_by_agent_id":"` + agentID + `"},
			{"datapack_id":"22222222-2222-2222-2222-222222222222","datapack_name":"alpha",
			 "created_by_agent_id":"` + agentID + `"}
		]}`))
	})

	got, err := b.List(t.Context())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("List returned %d resources, want 2", len(got))
	}
	// Sorted, so a status report does not reshuffle every tick.
	if got[0].Key.Name != "alpha" || got[1].Key.Name != "beta" {
		t.Fatalf("List order = %q, %q; want alpha, beta", got[0].Key.Name, got[1].Key.Name)
	}
	beta := got[1]
	if beta.Key.Kind != KindDatapack {
		t.Errorf("kind = %q, want %q", beta.Key.Kind, KindDatapack)
	}
	// A datapack has no provisioning phase, so listing at all means usable —
	// otherwise the engine would gate children on a readiness that never arrives.
	if !beta.Ready {
		t.Error("a listed datapack must be Ready")
	}
	// Synchronous verbs mean there is nothing to echo; the engine's cache carries
	// the generation. A nonzero value here would claim an echo this backend has not
	// got.
	if beta.ObservedGen != 0 {
		t.Errorf("ObservedGen = %d, want 0", beta.ObservedGen)
	}
	if beta.Attrs["id"] != dpID {
		t.Errorf("attrs[id] = %q, want %q", beta.Attrs["id"], dpID)
	}
	if beta.Attrs["memory_count"] != "3" {
		t.Errorf("attrs[memory_count] = %q, want 3", beta.Attrs["memory_count"])
	}
	// An unset count is omitted rather than reported as "0", so the console can
	// tell "none" from "not known".
	if _, ok := got[0].Attrs["memory_count"]; ok {
		t.Error("a zero memory_count must be omitted from attrs")
	}
}

// A failed List must abort the tick rather than read as an empty world: the
// engine deletes everything actual-but-not-desired, so an outage that returned
// no error here would garbage-collect the environment.
func TestListErrorIsReturned(t *testing.T) {
	b := serve(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte(`<html>gateway</html>`))
	})
	if _, err := b.List(t.Context()); err == nil {
		t.Fatal("List must return the api-server's failure, not an empty inventory")
	}
}

// The resource's Name is the datapack's name; the spec carries only what the
// create body can express besides it.
func TestCreateSendsTheResourceNameAndSpec(t *testing.T) {
	var body map[string]any
	b := serve(t, func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer pat-token" {
			t.Errorf("Authorization = %q, want the PAT bearer", got)
		}
		if r.URL.Path == "/agents" {
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"data":{"agent_id":"` + agentID + `"}}`))
			return
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"data":{"datapack_id":"` + dpID + `","name":"team-a"}}`))
	})

	err := b.Create(t.Context(), api.DesiredResource{
		Kind: KindDatapack,
		Name: "team-a",
		Spec: json.RawMessage(`{"description":"the team's pack"}`),
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if body["name"] != "team-a" {
		t.Errorf("create name = %v, want team-a", body["name"])
	}
	if body["description"] != "the team's pack" {
		t.Errorf("create description = %v", body["description"])
	}
	// The ownership mark, and the backend's to set: a datapack created without it
	// would be invisible to the next List and created again every tick.
	if body["agent_id"] != agentID {
		t.Errorf("create agent_id = %v, want %q", body["agent_id"], agentID)
	}
	// user_id is set by the api-server from the authenticated account; sending
	// one would be silently ignored, so the client must not imply otherwise.
	if _, ok := body["user_id"]; ok {
		t.Error("create body must not carry user_id")
	}
}

// Every spec field is optional: a bare resource must produce a working create.
func TestCreateWithNoSpec(t *testing.T) {
	var body map[string]any
	b := serve(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/agents" {
			w.WriteHeader(http.StatusConflict)
			_, _ = w.Write([]byte(`{"error":{"code":"CONFLICT","message":"exists"}}`))
			return
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"data":{"datapack_id":"` + dpID + `"}}`))
	})
	if err := b.Create(t.Context(), api.DesiredResource{Kind: KindDatapack, Name: "bare"}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if body["name"] != "bare" {
		t.Errorf("create name = %v, want bare", body["name"])
	}
}

// A spec the api-server refuses cannot be fixed by re-issuing it. Reporting it
// as drift names the reason on the resource instead of flapping the environment
// to DEGRADED on a call that will always fail.
func TestRefusedSpecIsPermanentDrift(t *testing.T) {
	for _, tc := range []struct {
		name string
		code int
		body string
	}{
		{"validation", http.StatusBadRequest, `{"error":{"code":"VALIDATION_ERROR","message":"name too short"}}`},
		{"conflict", http.StatusConflict, `{"error":{"code":"CONFLICT","message":"name already in use"}}`},
		{"forbidden", http.StatusForbidden, `{"error":{"code":"FORBIDDEN","message":"scope"}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := serve(t, func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.code)
				_, _ = w.Write([]byte(tc.body))
			})
			err := b.Create(t.Context(), api.DesiredResource{Kind: KindDatapack, Name: "x"})
			if !isPermanent(err) {
				t.Fatalf("Create error = %v, want permanent", err)
			}
		})
	}
}

// A free-tier cap cannot be retried out of. The gate counts LIFETIME creations
// (MetricDatapackCreation, never decremented on delete), so withdrawing a datapack
// frees nothing and the backoff ladder would run forever while the environment sat
// DEGRADED with nothing actionable on the resource.
func TestFreeTierLimitIsPermanent(t *testing.T) {
	b := serve(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusPaymentRequired)
		_, _ = w.Write([]byte(`{"error":{"code":"FREE_TIER_DATAPACK_LIMIT","message":"upgrade"}}`))
	})
	err := b.Create(t.Context(), api.DesiredResource{Kind: KindDatapack, Name: "x"})
	if !isPermanent(err) {
		t.Fatalf("Create error = %v, want permanent", err)
	}
}

// The api-server addresses a datapack by UUID — its ownership middleware rejects
// anything that does not parse as one — so Update has to use the id List saw,
// not the resource's name.
func TestUpdatePatchesTheListedUUID(t *testing.T) {
	var path string
	var body map[string]any
	b := listOne(t, "team-a", func(w http.ResponseWriter, r *http.Request) {
		path = r.Method + " " + r.URL.Path
		_ = json.NewDecoder(r.Body).Decode(&body)
		_, _ = w.Write([]byte(`{"data":{}}`))
	})
	if _, err := b.List(t.Context()); err != nil {
		t.Fatalf("List: %v", err)
	}

	err := b.Update(t.Context(), api.DesiredResource{
		Kind: KindDatapack, Name: "team-a", Generation: 2,
		Spec: json.RawMessage(`{"description":"renamed purpose"}`),
	})
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if want := "PATCH /datapacks/" + dpID; path != want {
		t.Errorf("update path = %q, want %q", path, want)
	}
	if body["description"] != "renamed purpose" {
		t.Errorf("update description = %v", body["description"])
	}
	// The resource key IS the datapack name, so a rename is a delete plus a
	// create as far as the engine is concerned. Sending a name here would rename
	// the row out from under the key and orphan it.
	if _, ok := body["name"]; ok {
		t.Error("update must not send a name")
	}
}

func TestDeleteUsesTheListedUUID(t *testing.T) {
	var path string
	b := listOne(t, "team-a", func(w http.ResponseWriter, r *http.Request) {
		path = r.Method + " " + r.URL.Path
		_, _ = w.Write([]byte(`{"data":{}}`))
	})
	if _, err := b.List(t.Context()); err != nil {
		t.Fatalf("List: %v", err)
	}
	if err := b.Delete(t.Context(), reconcile.ResourceKey{Kind: KindDatapack, Name: "team-a"}); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if want := "DELETE /datapacks/" + dpID; path != want {
		t.Errorf("delete path = %q, want %q", path, want)
	}
}

// A datapack the api-server no longer has is the outcome the caller wanted.
// Without this a delete that raced a concurrent removal backs off forever
// against a resource that is already gone.
func TestDelete404IsSuccess(t *testing.T) {
	b := listOne(t, "team-a", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":{"code":"NOT_FOUND","message":"gone"}}`))
	})
	if _, err := b.List(t.Context()); err != nil {
		t.Fatalf("List: %v", err)
	}
	if err := b.Delete(t.Context(), reconcile.ResourceKey{Kind: KindDatapack, Name: "team-a"}); err != nil {
		t.Fatalf("Delete on a missing datapack = %v, want success", err)
	}
}

func TestDeleteUnknownNameIssuesNothing(t *testing.T) {
	b := serve(t, func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("no request should be issued, got %s %s", r.Method, r.URL.Path)
	})
	if err := b.Delete(t.Context(), reconcile.ResourceKey{Kind: KindDatapack, Name: "never-listed"}); err != nil {
		t.Fatalf("Delete: %v", err)
	}
}

// Every kind in allKinds must be dispatched by every verb. A kind added to the
// list without a case reaches the unknown-kind arm, and this is what says so —
// otherwise it fails at runtime as permanent drift on a resource an operator
// legitimately declared.
func TestAllKindsAreDispatched(t *testing.T) {
	b := serve(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"data":{"datapack_id":"` + dpID + `"}}`))
	})
	for _, kind := range allKinds {
		r := api.DesiredResource{Kind: kind, Name: "x"}
		if err := b.Create(t.Context(), r); isUnknownKind(err) {
			t.Errorf("Create(%s): %v", kind, err)
		}
		if err := b.Update(t.Context(), r); isUnknownKind(err) {
			t.Errorf("Update(%s): %v", kind, err)
		}
		if err := b.Delete(t.Context(), reconcile.ResourceKey{Kind: kind, Name: "x"}); isUnknownKind(err) {
			t.Errorf("Delete(%s): %v", kind, err)
		}
	}
}

// A kind this backend does not map is permanent, not a retry: it means the
// control plane declared something this agent version cannot converge, and
// re-issuing it every tick says nothing new.
func TestUnknownKindIsPermanentOnEveryVerb(t *testing.T) {
	b := serve(t, func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("no request should be issued, got %s %s", r.Method, r.URL.Path)
	})
	r := api.DesiredResource{Kind: "knowledge-base", Name: "team-a/docs"}
	if err := b.Create(t.Context(), r); !isPermanent(err) {
		t.Errorf("Create: %v, want permanent", err)
	}
	if err := b.Update(t.Context(), r); !isPermanent(err) {
		t.Errorf("Update: %v, want permanent", err)
	}
	err := b.Delete(t.Context(), reconcile.ResourceKey{Kind: "knowledge-base", Name: "team-a/docs"})
	if !isPermanent(err) {
		t.Errorf("Delete: %v, want permanent", err)
	}
}

// Nothing has a parent yet, but the engine calls Parent for every create it
// plans, so the contract is asserted rather than assumed.
func TestDatapackIsARoot(t *testing.T) {
	b := serve(t, func(w http.ResponseWriter, r *http.Request) {})
	if _, ok := b.Parent(api.DesiredResource{Kind: KindDatapack, Name: "team-a"}); ok {
		t.Error("a datapack must be a root")
	}
}

func isUnknownKind(err error) bool {
	return err != nil && strings.Contains(err.Error(), "unknown kind")
}

// The whole safety story. A datapack a person made through the UI carries a
// different agent id (or none), and the engine deletes everything List reports
// that desired state does not declare — so anything not ours must not be
// reported at all. In a BYOC data plane this is not hypothetical: Cognito is off
// there, so DevAuth puts the agent's requests and the UI's on the same account
// and every datapack comes back from GET /datapacks.
func TestListReportsOnlyItsOwnDatapacks(t *testing.T) {
	b := serve(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"data":[
			{"datapack_id":"` + dpID + `","datapack_name":"ours","created_by_agent_id":"` + agentID + `"},
			{"datapack_id":"22222222-2222-2222-2222-222222222222","datapack_name":"a-person's",
			 "created_by_agent_id":"meko_agent"},
			{"datapack_id":"33333333-3333-3333-3333-333333333333","datapack_name":"unstamped"}
		]}`))
	})

	got, err := b.List(t.Context())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != 1 || got[0].Key.Name != "ours" {
		var names []string
		for _, a := range got {
			names = append(names, a.Key.Name)
		}
		t.Fatalf("List returned %v, want only [ours]", names)
	}
}

// A datapack outside this backend's world is not deletable either, even when
// named explicitly: Delete resolves through the index List built, so a name it
// never reported issues nothing. Without this a withdrawn resource that happened
// to share a person's datapack name would delete theirs.
func TestDeleteWillNotTouchAnotherAgentsDatapack(t *testing.T) {
	b := serve(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			_, _ = w.Write([]byte(`{"data":[{"datapack_id":"` + dpID +
				`","datapack_name":"theirs","created_by_agent_id":"meko_agent"}]}`))
			return
		}
		t.Errorf("no mutating request should be issued, got %s %s", r.Method, r.URL.Path)
	})
	if _, err := b.List(t.Context()); err != nil {
		t.Fatalf("List: %v", err)
	}
	if err := b.Delete(t.Context(), reconcile.ResourceKey{Kind: KindDatapack, Name: "theirs"}); err != nil {
		t.Fatalf("Delete: %v", err)
	}
}

// The agent row must exist before a datapack can name it, so the create is
// ordered behind it — and an agent that is already there (409) is the state the
// caller wanted, not a failure.
func TestCreateEnsuresTheAgentFirst(t *testing.T) {
	var hits []string
	b := serve(t, func(w http.ResponseWriter, r *http.Request) {
		hits = append(hits, r.Method+" "+r.URL.Path)
		if r.URL.Path == "/agents" {
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			if body["agent_id"] != agentID {
				t.Errorf("ensure-agent id = %v, want %q", body["agent_id"], agentID)
			}
			w.WriteHeader(http.StatusConflict)
			_, _ = w.Write([]byte(`{"error":{"code":"CONFLICT","message":"already exists"}}`))
			return
		}
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"data":{"datapack_id":"` + dpID + `"}}`))
	})

	for range 2 {
		if err := b.Create(t.Context(), api.DesiredResource{Kind: KindDatapack, Name: "team-a"}); err != nil {
			t.Fatalf("Create: %v", err)
		}
	}
	// Ensured once per process: the row is never deleted, so re-checking every
	// create would spend a request per tick to learn the same thing.
	want := []string{"POST /agents", "POST /datapacks", "POST /datapacks"}
	if strings.Join(hits, ",") != strings.Join(want, ",") {
		t.Errorf("request sequence = %v, want %v", hits, want)
	}
}

// An agent create that fails for a real reason must stop the datapack create:
// the datapack would be rejected for naming an agent that does not exist, and
// reporting the agent failure names the actual cause.
func TestCreateStopsWhenTheAgentCannotBeEnsured(t *testing.T) {
	b := serve(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/agents" {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"error":{"code":"INTERNAL_ERROR","message":"db down"}}`))
			return
		}
		t.Errorf("the datapack create must not be issued, got %s %s", r.Method, r.URL.Path)
	})
	err := b.Create(t.Context(), api.DesiredResource{Kind: KindDatapack, Name: "team-a"})
	if err == nil || !strings.Contains(err.Error(), "ensure agent") {
		t.Fatalf("Create error = %v, want an ensure-agent failure", err)
	}
}

// Datapack names are unique per account, but the repository does not map that
// violation to a conflict — it surfaces as a 500, which is retryable — and List
// does not report the colliding datapack because it is not ours. Refusing the
// create here is what turns an invisible forever-retry into a reason on the
// resource.
func TestCreateRefusesANameAnotherAgentHolds(t *testing.T) {
	b := serve(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			_, _ = w.Write([]byte(`{"data":[{"datapack_id":"` + dpID +
				`","datapack_name":"team-a","created_by_agent_id":"meko_agent"}]}`))
			return
		}
		t.Errorf("no create should be issued, got %s %s", r.Method, r.URL.Path)
	})
	if _, err := b.List(t.Context()); err != nil {
		t.Fatalf("List: %v", err)
	}
	err := b.Create(t.Context(), api.DesiredResource{Kind: KindDatapack, Name: "team-a"})
	if !isPermanent(err) {
		t.Fatalf("Create error = %v, want permanent drift", err)
	}
	if !strings.Contains(err.Error(), "unique per account") {
		t.Errorf("error should name the cause, got %v", err)
	}
}

// A name that was foreign and is not any more must be creatable again — the mark
// lives on the last List, not for the life of the process.
func TestAForeignNameStopsBlockingOnceItIsGone(t *testing.T) {
	var listed string
	b := serve(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			_, _ = w.Write([]byte(listed))
			return
		}
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"data":{"datapack_id":"` + dpID + `"}}`))
	})

	listed = `{"data":[{"datapack_id":"` + dpID + `","datapack_name":"team-a","created_by_agent_id":"meko_agent"}]}`
	if _, err := b.List(t.Context()); err != nil {
		t.Fatalf("List: %v", err)
	}
	if err := b.Create(t.Context(), api.DesiredResource{Kind: KindDatapack, Name: "team-a"}); !isPermanent(err) {
		t.Fatalf("Create error = %v, want permanent while the name is taken", err)
	}

	listed = `{"data":[]}`
	if _, err := b.List(t.Context()); err != nil {
		t.Fatalf("List: %v", err)
	}
	if err := b.Create(t.Context(), api.DesiredResource{Kind: KindDatapack, Name: "team-a"}); err != nil {
		t.Fatalf("Create after the name freed up = %v, want success", err)
	}
}

// A rejected credential is never fixed by asking again, so it belongs on the
// resource as drift rather than inside a backoff that hides it.
func TestUnauthorizedIsPermanent(t *testing.T) {
	b := serve(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":{"code":"UNAUTHORIZED","message":"no token"}}`))
	})
	if err := b.Create(t.Context(), api.DesiredResource{Kind: KindDatapack, Name: "x"}); !isPermanent(err) {
		t.Fatalf("Create error = %v, want permanent", err)
	}
}

// meko_agent is the id the api-server stamps on any create that names none — every
// datapack made in the UI. A reconciler holding it would report them all as its own
// and delete every one desired state does not declare, which is unrecoverable. It
// takes a deliberate misconfiguration to reach, which is exactly why the refusal is
// in the constructor rather than in a runbook.
func TestNewRefusesAnAgentIDThatWouldAdoptEveryonesDatapacks(t *testing.T) {
	for _, id := range []string{"", "meko_agent"} {
		if _, err := New("http://api.example/api/v1", "tok", id, nil); err == nil {
			t.Errorf("New with agent id %q must be refused", id)
		}
	}
	if _, err := New("http://api.example/api/v1", "tok", "ycp-reconcile-agent", nil); err != nil {
		t.Errorf("New with a distinct agent id = %v, want success", err)
	}
}

// A datapack is a root, so a name carrying the path separator is a spec this
// backend cannot converge: Create would strip the prefix, List would key the
// resource by what the api-server returned, and the two would never agree — a
// create and a delete every tick, each create permanently spending free-tier quota.
func TestASplitNameIsRefusedRatherThanFlapped(t *testing.T) {
	b := serve(t, func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("no request should be issued, got %s %s", r.Method, r.URL.Path)
	})
	err := b.Create(t.Context(), api.DesiredResource{Kind: KindDatapack, Name: "team-a/docs"})
	if !isPermanent(err) {
		t.Fatalf("Create error = %v, want permanent", err)
	}
	err = b.Update(t.Context(), api.DesiredResource{
		Kind: KindDatapack, Name: "team-a/docs", Generation: 2,
		Spec: json.RawMessage(`{"description":"d"}`),
	})
	if !isPermanent(err) {
		t.Fatalf("Update error = %v, want permanent", err)
	}
}

// Clearing a description cannot be applied: the request field is omitempty and the
// api-server writes a description only when it is non-empty, so the PATCH is a
// no-op. Returning nil there would cache the bumped generation against work that
// never happened and no later tick would revisit it — the meko#985 latch, reached
// from the one direction synchronous verbs do not otherwise expose.
func TestClearingADescriptionIsDriftNotASilentNoOp(t *testing.T) {
	b := serve(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			_, _ = w.Write([]byte(`{"data":[{"datapack_id":"` + dpID +
				`","datapack_name":"team-a","description":"still here",` +
				`"created_by_agent_id":"` + agentID + `"}]}`))
			return
		}
		t.Errorf("no PATCH should be issued, got %s %s", r.Method, r.URL.Path)
	})
	if _, err := b.List(t.Context()); err != nil {
		t.Fatalf("List: %v", err)
	}
	err := b.Update(t.Context(), api.DesiredResource{
		Kind: KindDatapack, Name: "team-a", Generation: 2, Spec: json.RawMessage(`{}`),
	})
	if !isPermanent(err) {
		t.Fatalf("Update error = %v, want permanent drift", err)
	}
	if !strings.Contains(err.Error(), "still here") {
		t.Errorf("the drift should name what the description still reads, got %v", err)
	}
}

// Replacing a description is ordinary work and must still go through.
func TestReplacingADescriptionStillApplies(t *testing.T) {
	var body map[string]any
	b := listOne(t, "team-a", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&body)
		_, _ = w.Write([]byte(`{"data":{}}`))
	})
	if _, err := b.List(t.Context()); err != nil {
		t.Fatalf("List: %v", err)
	}
	err := b.Update(t.Context(), api.DesiredResource{
		Kind: KindDatapack, Name: "team-a", Generation: 2,
		Spec: json.RawMessage(`{"description":"replaced"}`),
	})
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if body["description"] != "replaced" {
		t.Errorf("update description = %v", body["description"])
	}
}
