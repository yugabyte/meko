// Package mekobackend implements the reconcile engine's Backend against meko's
// api_server tenant REST API, the way ampbackend implements it against the amp
// controller. It owns the layer above the meko stack: the resources that live
// inside a running meko, not the workloads that make one up (those are the meko
// chart's, as the amp controller is yb-amp-controller's).
//
// # Ownership: the agent id is the boundary
//
// The engine is authoritative — anything List reports that desired state does not
// declare is deleted. Datapacks are created by real people through the UI and
// through MCP, so a List that returned all of them would put a customer's own work
// in the delete set on the first tick.
//
// So every datapack this backend creates is stamped with its own agent id
// (created_by_agent_id, which the api-server persists on create and returns on
// every read), and List reports only datapacks carrying it. Everything else is
// invisible to the engine: not converged, not reported, and above all not deleted.
//
// The account cannot be that boundary, though it is tempting. A BYOC data plane
// runs with Cognito off, where DevAuth puts every request — the agent's and the
// UI's — on one dev account, so account scoping would put a person's datapacks and
// the reconciler's in the same set. The agent id separates them in both
// configurations.
//
// The id must name an agent row that exists, so Create ensures it first and treats
// the api-server's 409 as success — the same shape as ampbackend materializing a
// pool's owner before creating the pool. POST /agents is on the PAT-accepting group
// for exactly this call; list and delete are not, and are not needed.
//
// # Kinds
//
//	datapack  "<datapack>"
//
// knowledge-base is the natural child, but it cannot be a kind yet: a PAT reaches
// its upload and document-status routes (both on dpDirect) and none of the rest —
// add, list and delete are all on the JWT-only group in internal/api/router.go. A
// child kind also needs a hierarchical name, which checkFlatName refuses today.
//
// # Why there is no generation echo here
//
// ampbackend echoes an applied generation because every amp verb is asynchronous:
// a 202 means "task planned", and a task that then failed used to latch as applied
// (meko#985). meko's datapack verbs are synchronous, so a verb returning nil really
// did land and the engine's own cache is a truthful record; List reports ObservedGen
// 0 for every resource. An asynchronous verb added here (knowledge-base ingestion is
// one) must report reconcile.Pending rather than nil, and that is the point at which
// an echo becomes necessary.
package mekobackend

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"

	"meko/ycp/internal/api"
	"meko/ycp/internal/mekoclient"
	"meko/ycp/reconcile"
)

// KindDatapack is the one kind an operator declares for a meko environment.
const KindDatapack = "datapack"

// allKinds is every kind this backend maps. TestAllKindsAreDispatched walks it,
// so a kind added here without a case in the verb switches fails rather than
// silently reaching the default arm.
var allKinds = []string{KindDatapack}

const sep = "/"

// Backend drives one meko api_server as one user. Safe for concurrent use within
// a tick: List rebuilds the name index under lock and the verbs only read it.
type Backend struct {
	api     *mekoclient.Client
	agentID string

	mu sync.RWMutex
	// agentEnsured records that the agent row this backend stamps its datapacks
	// with has been created (or was already there). In-process and lost on
	// restart, which costs one redundant 409 and nothing else.
	agentEnsured bool
	// row is each datapack this backend owns as the api-server last reported it,
	// keyed by name. The whole row rather than just its UUID because Update needs
	// the id for the request path.
	row map[string]mekoclient.Datapack
	// foreign is the names the account holds that this backend does NOT own. Not
	// reported to the engine — that would put them in the delete set — but kept
	// because datapack names are unique per account: a create colliding with one
	// can never succeed, and without this the collision is invisible here and
	// re-issued every tick.
	foreign map[string]bool
}

// defaultAgentID is repository.DefaultAgentID in meko's api_server: the agent every
// account gets at signup, and the one stamped on any create that names none — which
// is every datapack made through the UI. It is the one value this backend must never
// be given, so it is named here rather than left to a deployment to avoid.
const defaultAgentID = "meko_agent"

// New builds a Backend against the api-server at baseURL (which must include the
// API base path). token is an MCP PAT with the read and write scopes, and may be
// empty against an api-server running without Cognito, where every request is the
// dev account's.
//
// agentID is the ownership mark, and the only argument that can be catastrophically
// wrong: this backend reports every datapack carrying it and the engine deletes
// everything it reports that desired state does not declare. "meko_agent" therefore
// adopts every datapack a person made in the UI and deletes the lot on the first
// tick. It takes a deliberate misconfiguration to reach — the chart defaults the
// value and an unset variable falls back to the same default — but the outcome is
// irreversible, so it is refused here rather than defended by convention.
func New(baseURL, token, agentID string, hc *http.Client) (*Backend, error) {
	switch agentID {
	case "":
		return nil, fmt.Errorf("agent id is required: it is what separates this reconciler's datapacks from everyone else's")
	case defaultAgentID:
		return nil, fmt.Errorf("agent id %q is meko's default, stamped on every datapack created without one "+
			"(the UI path): a reconciler using it would adopt them all and delete every one desired state "+
			"does not declare", agentID)
	}
	return &Backend{
		api:     mekoclient.New(baseURL, token, hc),
		agentID: agentID,
		row:     map[string]mekoclient.Datapack{},
		foreign: map[string]bool{},
	}, nil
}

// Parent reports the resource's parent key. Every kind is a root today; the
// path split is here so knowledge-base and agent slot in without reshaping the
// caller when their routes become PAT-reachable.
func (b *Backend) Parent(api.DesiredResource) (reconcile.ResourceKey, bool) {
	return reconcile.ResourceKey{}, false
}

// checkFlatName refuses a datapack name carrying the path separator. A datapack is
// a root — there is no hierarchy to encode — and splitting the name would make
// Create and List disagree about what the resource is called: the create would
// strip the prefix, List would key the resource by the name the api-server returned,
// and every tick would then see the declared name missing and the created one
// undeclared. That is a create and a delete per tick, and each create permanently
// consumes free-tier quota (the cap counts creations and is never refunded).
func checkFlatName(name string) error {
	if strings.Contains(name, sep) {
		return reconcile.Permanent(fmt.Errorf(
			"datapack %q: a datapack name cannot contain %q — datapacks are flat, "+
				"and a split name would be created and deleted on every tick", name, sep))
	}
	return nil
}

// datapackSpec is the convergeable part of a datapack. Two fields it deliberately
// does not have: a name, because the resource's Name is the datapack's name and a
// rename is a different key (which the engine already expresses as a delete plus a
// create); and an agent id, because that is this backend's ownership mark rather
// than an operator's choice — one set from desired state would hand away, or take
// over, resources the reconciler must not touch.
type datapackSpec struct {
	Description string `json:"description,omitempty"`
}

// List reports the datapacks this backend owns — those stamped with its agent id,
// and only those, since the engine deletes everything it reports that desired
// state does not declare. A failed List aborts the tick, which is what keeps a
// transient api-server outage from being read as an empty world and
// garbage-collecting the lot.
func (b *Backend) List(ctx context.Context) ([]reconcile.Actual, error) {
	dps, err := b.api.ListDatapacks(ctx)
	if err != nil {
		return nil, fmt.Errorf("list datapacks: %w", err)
	}

	rows := make(map[string]mekoclient.Datapack, len(dps))
	foreign := map[string]bool{}
	out := make([]reconcile.Actual, 0, len(dps))
	for _, dp := range dps {
		if dp.CreatedByAgent != b.agentID {
			// Someone else's datapack: not ours to converge or to delete. Its name
			// is still ours to know about — see Create.
			foreign[dp.Name] = true
			continue
		}
		rows[dp.Name] = dp
		out = append(out, reconcile.Actual{
			Key: reconcile.ResourceKey{Kind: KindDatapack, Name: dp.Name},
			// A datapack has no provisioning phase to wait out: Create inserts the
			// row and the reply carries it, so one that lists at all is usable.
			Ready: true,
			State: "ready",
			Attrs: datapackAttrs(dp),
		})
	}

	b.mu.Lock()
	b.row, b.foreign = rows, foreign
	b.mu.Unlock()

	sort.Slice(out, func(i, j int) bool { return out[i].Key.Name < out[j].Key.Name })
	return out, nil
}

// datapackAttrs reports what the console can render without a second call. The
// api-server redacts every credential field out of a datapack before returning
// it, so there is nothing here to withhold.
func datapackAttrs(dp mekoclient.Datapack) map[string]string {
	var m map[string]string
	m = attrPut(m, "id", dp.ID)
	m = attrPut(m, "description", dp.Description)
	m = attrPut(m, "created_by_agent_id", dp.CreatedByAgent)
	m = attrPut(m, "grant", dp.Grant)
	m = attrPutInt(m, "memory_count", dp.MemoryCount)
	m = attrPutInt(m, "knowledge_count", dp.KnowledgeCount)
	if !dp.CreatedAt.IsZero() {
		m = attrPut(m, "created_at", dp.CreatedAt.UTC().Format("2006-01-02T15:04:05Z"))
	}
	return m
}

func attrPut(m map[string]string, k, v string) map[string]string {
	if v == "" {
		return m
	}
	if m == nil {
		m = map[string]string{}
	}
	m[k] = v
	return m
}

func attrPutInt(m map[string]string, k string, v int) map[string]string {
	if v == 0 {
		return m
	}
	return attrPut(m, k, strconv.Itoa(v))
}

// Create makes one resource. Idempotency is by presence: the engine only issues a
// create for something List did not report.
//
// A name the account already holds under another agent is refused here rather than
// at the api-server. It has to be: datapack names are unique per account
// (uq_datapacks_user_name), but the repository does not map that violation to a
// conflict — it comes back as a 500, which is retryable, and List does not report
// the colliding datapack because it is not ours. So the create would be re-issued
// every tick forever, with nothing on the resource saying why.
func (b *Backend) Create(ctx context.Context, r api.DesiredResource) error {
	switch r.Kind {
	case KindDatapack:
		spec, err := specOf(r)
		if err != nil {
			return err
		}
		name := r.Name
		if err := checkFlatName(name); err != nil {
			return err
		}
		if b.foreignName(name) {
			return reconcile.Permanent(fmt.Errorf(
				"datapack %q: the account already holds a datapack of that name under "+
					"another agent; names are unique per account", name))
		}
		if err := b.ensureAgent(ctx); err != nil {
			return err
		}
		_, err = b.api.CreateDatapack(ctx, mekoclient.CreateDatapackRequest{
			Name:        name,
			AgentID:     b.agentID,
			Description: spec.Description,
		})
		return classify(err)
	}
	return reconcile.Permanent(fmt.Errorf("create: unknown kind %q", r.Kind))
}

// Update converges a resource whose generation moved. It is synchronous: the
// call returns after the api-server applied the patch, so nil means applied.
func (b *Backend) Update(ctx context.Context, r api.DesiredResource) error {
	switch r.Kind {
	case KindDatapack:
		return b.updateDatapack(ctx, r)
	}
	return reconcile.Permanent(fmt.Errorf("update: unknown kind %q", r.Kind))
}

func (b *Backend) updateDatapack(ctx context.Context, r api.DesiredResource) error {
	spec, err := specOf(r)
	if err != nil {
		return err
	}
	if err := checkFlatName(r.Name); err != nil {
		return err
	}
	cur, ok := b.rowOf(r.Name)
	if !ok {
		// The engine only plans an update for something List reported, so this is
		// a resource that vanished between the List and the dispatch. Retryable:
		// the next tick sees it absent and creates it.
		return fmt.Errorf("update datapack %q: not in the current inventory", r.Name)
	}
	// The api-server writes a description only when it is non-empty (UpdateByID),
	// and the field is omitempty on the way out, so a desired state that drops it
	// sends {} and changes nothing. Returning nil there would cache the new
	// generation against work that never happened and no later tick would revisit
	// it — the meko#985 latch, reached from the one direction this backend's
	// synchronous verbs do not otherwise expose. Report it as drift instead.
	if spec.Description == "" && cur.Description != "" {
		return reconcile.Permanent(fmt.Errorf(
			"datapack %q: a description cannot be removed through the api-server, only "+
				"replaced; it still reads %q", r.Name, cur.Description))
	}
	return classify(b.api.UpdateDatapack(ctx, cur.ID, mekoclient.UpdateDatapackRequest{
		Description: spec.Description,
	}))
}

// Delete removes a withdrawn resource. A datapack the api-server no longer has
// is the outcome the caller wanted, so a 404 is success — otherwise a delete
// that raced a concurrent removal would back off forever against a resource
// that is already gone.
func (b *Backend) Delete(ctx context.Context, key reconcile.ResourceKey) error {
	switch key.Kind {
	case KindDatapack:
		cur, ok := b.rowOf(key.Name)
		if !ok {
			return nil // not in the inventory: nothing to delete
		}
		err := b.api.DeleteDatapack(ctx, cur.ID)
		var apiErr *mekoclient.Error
		if errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusNotFound {
			return nil
		}
		return classify(err)
	}
	return reconcile.Permanent(fmt.Errorf("delete: unknown kind %q", key.Kind))
}

// ensureAgent creates the agent row this backend stamps its datapacks with. A
// datapack create naming an agent the user does not have is rejected, so this runs
// first; 409 means it is already there, which is the state the caller wanted.
// Ensured once per process — the row is never deleted, so re-checking every tick
// would buy nothing.
func (b *Backend) ensureAgent(ctx context.Context) error {
	b.mu.RLock()
	done := b.agentEnsured
	b.mu.RUnlock()
	if done {
		return nil
	}
	err := b.api.CreateAgent(ctx, b.agentID)
	var apiErr *mekoclient.Error
	if err != nil && !(errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusConflict) {
		return fmt.Errorf("ensure agent %q: %w", b.agentID, classify(err))
	}
	b.mu.Lock()
	b.agentEnsured = true
	b.mu.Unlock()
	return nil
}

// foreignName reports whether the last List saw this name on a datapack belonging
// to another agent.
func (b *Backend) foreignName(name string) bool {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.foreign[name]
}

// rowOf returns the datapack as the last List saw it.
func (b *Backend) rowOf(name string) (mekoclient.Datapack, bool) {
	b.mu.RLock()
	defer b.mu.RUnlock()
	dp, ok := b.row[name]
	return dp, ok
}

// specOf unmarshals a resource's spec. An absent spec is a valid resource with
// every field defaulted, per the "every spec field is optional" property the
// desired-state contract holds to.
func specOf(r api.DesiredResource) (datapackSpec, error) {
	var s datapackSpec
	if len(r.Spec) == 0 {
		return s, nil
	}
	if err := json.Unmarshal(r.Spec, &s); err != nil {
		return s, reconcile.Permanent(fmt.Errorf("%s %q: bad spec: %w", r.Kind, r.Name, err))
	}
	return s, nil
}

// classify turns an api-server rejection into the verdict the engine acts on.
// The distinction that matters is whether re-issuing the same call could ever
// succeed: a spec the api-server refuses cannot, and retrying it forever flaps
// the environment to DEGRADED on a call that will always fail.
//
// A free-tier cap (402) is permanent, which is not the intuitive answer. The gate
// counts LIFETIME creations (repository.MetricDatapackCreation, "never decremented
// on delete"), so withdrawing a datapack does not free a slot and no amount of
// retrying gets past it. Left retryable it would ride the backoff ladder forever
// while holding the environment DEGRADED with nothing actionable on the resource —
// and it is reachable in the configuration this backend targets, since a BYOC
// api-server runs Cognito-off and DevAuth's account defaults to the free tier.
func classify(err error) error {
	if err == nil {
		return nil
	}
	var apiErr *mekoclient.Error
	if !errors.As(err, &apiErr) {
		return err
	}
	switch apiErr.StatusCode {
	case http.StatusBadRequest, http.StatusConflict, http.StatusPaymentRequired:
		return reconcile.Permanent(err)
	case http.StatusUnauthorized, http.StatusForbidden:
		// A missing or rejected token, a scope the token lacks, or a datapack this
		// account does not own. None of them is fixed by asking again, and each is
		// worth naming on the resource instead of hiding in a backoff.
		return reconcile.Permanent(err)
	}
	return err
}
