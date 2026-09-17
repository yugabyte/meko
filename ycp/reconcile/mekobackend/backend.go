// Package mekobackend implements the reconcile engine's Backend against meko's
// api_server tenant REST API, the way ampbackend implements it against the amp
// controller. The engine stays kind-agnostic; all the meko knowledge is here.
//
// It owns the layer above the meko stack: the resources that live inside a
// running meko, not the workloads that make one up. Those are the meko chart's,
// exactly as the amp controller is yb-amp-controller's — the agent never applies
// a manifest.
//
// # Kinds
//
//	agent     "<agent>"
//	datapack  "<agent>/<datapack>"
//
// The path carries the hierarchy, as amp's pool/cluster/branch/endpoint does: it
// yields the parent for the engine's ordering and, at apply time, the parent's id
// for the request. A datapack must name an agent that exists, so the engine's
// parent gating is what orders the two — where an earlier version reached for an
// ensureAgent side effect inside Create.
//
// # The reconciler is the api-server's only caller
//
// A BYOC data plane's api_server serves this agent and nothing else — no console,
// no MCP client, no person. That is what lets the mapping be amp's without
// qualification: the agent is authoritative over everything the API holds, exactly
// as it is over an amp controller's inventory, so List reports every agent and
// every datapack and the engine garbage-collects whatever desired state stops
// declaring.
//
// If that ever stops being true — if a UI or a human token reaches the same
// api-server — this is the assumption that breaks first, and it breaks by deleting
// their rows on the next tick. Ownership would then have to become a property of
// the rows (agent_details carries a JSONB field that would hold a mark) rather than
// of the deployment.
//
// # Why there is no generation echo here
//
// ampbackend echoes an applied generation because every amp verb is asynchronous:
// a 202 means "task planned", and a task that then failed used to latch as applied
// (meko#985). meko's verbs are synchronous, so a verb returning nil really did
// land and the engine's own cache is a truthful record; List reports ObservedGen 0
// for every resource. An asynchronous verb added here must report
// reconcile.Pending rather than nil, and that is the point at which an echo becomes
// necessary.
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

// Resource kinds an operator declares in desired state for a meko environment.
const (
	KindAgent    = "agent"
	KindDatapack = "datapack"
)

// allKinds is every kind this backend maps. TestEveryKindIsDispatched walks it, so
// a kind added above without a case in the verb switches fails rather than reaching
// the unknown-kind arm at runtime.
var allKinds = []string{KindAgent, KindDatapack}

const sep = "/"

// defaultAgentID is repository.DefaultAgentID in meko's api_server: the row every
// account gets at signup, and the agent the server stamps on a create that names
// none.
//
// It is account infrastructure rather than a resource, and is excluded from the
// inventory for the same reason amp does not model users as a kind: it cannot be
// safely deleted. DeleteAgent drops the account's agent_access rows along with the
// agent, and the share and invite grants are written under exactly this id — so
// collecting it because desired state does not name it would silently revoke every
// datapack shared into the account, and break every later create that names no
// agent, with nothing to recreate the row but a fresh signup.
const defaultAgentID = "meko_agent"

// Backend drives one meko api_server. Safe for concurrent use within a tick: List
// rebuilds the index under lock and the verbs only read it.
type Backend struct {
	api *mekoclient.Client

	mu sync.RWMutex
	// idx is what the last List saw, keyed by resource path. Rows rather than bare
	// ids because Update diffs against them and Delete needs the id for the path.
	idx index
}

// index maps a resource's path to what the api-server reported for it.
type index struct {
	agent    map[string]mekoclient.Agent    // "<agent>"
	datapack map[string]mekoclient.Datapack // "<agent>/<datapack>"
}

func newIndex() index {
	return index{
		agent:    map[string]mekoclient.Agent{},
		datapack: map[string]mekoclient.Datapack{},
	}
}

// New builds a Backend against the api-server at baseURL, which must include the
// API base path. token is an MCP PAT with the read and write scopes, and may be
// empty against an api-server running without Cognito, where every request is the
// dev account's.
func New(baseURL, token string, hc *http.Client) *Backend {
	return &Backend{api: mekoclient.New(baseURL, token, hc), idx: newIndex()}
}

// Parent returns the resource's parent key derived from its path. An agent is a
// root; a datapack hangs off the agent its first segment names, which is what makes
// the engine create the agent first and gate the datapack on it.
func (b *Backend) Parent(r api.DesiredResource) (reconcile.ResourceKey, bool) {
	if r.Kind == KindDatapack {
		if segs := strings.Split(r.Name, sep); len(segs) >= 2 {
			return reconcile.ResourceKey{Kind: KindAgent, Name: segs[0]}, true
		}
	}
	return reconcile.ResourceKey{}, false
}

func leaf(name string) string {
	segs := strings.Split(name, sep)
	return segs[len(segs)-1]
}

func parentOf(name string) string { return strings.Split(name, sep)[0] }

// checkName refuses a path that does not match the kind's shape. A wrong shape is
// not a slow failure: Create and List would disagree about what the resource is
// called, so every tick would see the declared name missing and the created one
// undeclared — a create and a delete per tick, each create permanently spending
// free-tier quota, which counts creations and never refunds them.
func checkName(kind, name string) error {
	segs := strings.Split(name, sep)
	want := 1
	if kind == KindDatapack {
		want = 2
	}
	if segs[0] == defaultAgentID {
		return reconcile.Rejected(fmt.Errorf(
			"%s %q: %q is the account's signup agent and is not managed here — deleting "+
				"it would drop the account's share grants and break every create that "+
				"names no agent", kind, name, defaultAgentID))
	}
	if len(segs) != want || segs[0] == "" || segs[want-1] == "" {
		shape := "<agent>"
		if kind == KindDatapack {
			shape = "<agent>/<datapack>"
		}
		return reconcile.Rejected(fmt.Errorf("%s %q: name must be %q", kind, name, shape))
	}
	return nil
}

// agentSpec is the convergeable part of an agent. Its name is its id, so there is
// nothing else to declare today; the struct exists so a spec that carries fields a
// newer control plane sends is ignored rather than fatal.
type agentSpec struct{}

// datapackSpec is the convergeable part of a datapack. Two fields it deliberately
// does not have: a name, because the resource's Name is the datapack's name and a
// rename is a different key; and an agent id, because that comes from the parent
// path — one set from the spec could hand a datapack to an agent the engine is not
// managing.
type datapackSpec struct {
	Description string `json:"description,omitempty"`
}

// List reports every agent and every datapack the api-server holds, which is the
// whole inventory: this agent is its only caller. A failed List aborts the tick,
// which is what keeps a transient outage from being read as an empty world and
// garbage-collecting the lot.
func (b *Backend) List(ctx context.Context) ([]reconcile.Actual, error) {
	agents, err := b.api.ListAgents(ctx)
	if err != nil {
		return nil, fmt.Errorf("list agents: %w", err)
	}
	idx := newIndex()
	var out []reconcile.Actual
	for _, a := range agents {
		if a.AgentID == defaultAgentID {
			continue // account infrastructure, not a resource — see defaultAgentID
		}
		idx.agent[a.AgentID] = a
		out = append(out, reconcile.Actual{
			Key: reconcile.ResourceKey{Kind: KindAgent, Name: a.AgentID},
			// An agent is a row, not a workload: there is no provisioning phase to
			// wait out, so one that lists at all is usable and its children may be
			// created.
			Ready: true,
			State: "ready",
			Attrs: agentAttrs(a),
		})
	}

	dps, err := b.api.ListDatapacks(ctx)
	if err != nil {
		return nil, fmt.Errorf("list datapacks: %w", err)
	}
	for _, dp := range dps {
		agent := dp.CreatedByAgent
		if agent == "" || agent == defaultAgentID {
			// Attributed to the signup default, which is not in the inventory, so
			// neither is this. Reporting it would give it a parent the engine
			// cannot see, and loop.go skips a child whose parent is undeclared —
			// so it would sit in the report forever, never converged and never
			// collected.
			continue
		}
		path := agent + sep + dp.Name
		idx.datapack[path] = dp
		out = append(out, reconcile.Actual{
			Key:    reconcile.ResourceKey{Kind: KindDatapack, Name: path},
			Parent: reconcile.ResourceKey{Kind: KindAgent, Name: agent},
			Ready:  true,
			State:  "ready",
			Attrs:  datapackAttrs(dp),
		})
	}

	b.mu.Lock()
	b.idx = idx
	b.mu.Unlock()

	sort.Slice(out, func(i, j int) bool { return out[i].Key.String() < out[j].Key.String() })
	return out, nil
}

func agentAttrs(a mekoclient.Agent) map[string]string {
	var m map[string]string
	m = attrPut(m, "user_id", a.UserID)
	if !a.CreatedAt.IsZero() {
		m = attrPut(m, "created_at", a.CreatedAt.UTC().Format("2006-01-02T15:04:05Z"))
	}
	return m
}

// datapackAttrs reports what a console can render without a second call. The
// api-server redacts every credential field out of a datapack before returning it,
// so there is nothing here to withhold.
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
// create for something List did not report, and the parent gating means an agent
// exists before any datapack naming it is attempted.
func (b *Backend) Create(ctx context.Context, r api.DesiredResource) error {
	if err := checkName(r.Kind, r.Name); err != nil {
		return err
	}
	switch r.Kind {
	case KindAgent:
		return classify(b.api.CreateAgent(ctx, r.Name))
	case KindDatapack:
		spec, err := datapackSpecOf(r)
		if err != nil {
			return err
		}
		_, err = b.api.CreateDatapack(ctx, mekoclient.CreateDatapackRequest{
			Name:        leaf(r.Name),
			AgentID:     parentOf(r.Name),
			Description: spec.Description,
		})
		return classify(err)
	}
	return reconcile.Permanent(fmt.Errorf("create: unknown kind %q", r.Kind))
}

// Update converges a resource whose generation moved. It is synchronous: the call
// returns after the api-server applied the patch, so nil means applied.
func (b *Backend) Update(ctx context.Context, r api.DesiredResource) error {
	switch r.Kind {
	case KindAgent:
		// An agent is its id and nothing else; there is no field to change, so a
		// bumped generation cannot be satisfied. Reported as drift rather than
		// retried, the way ampbackend reports a pool or cluster update.
		return reconcile.Permanent(fmt.Errorf("agent %q: an agent has no updatable field", r.Name))
	case KindDatapack:
		return b.updateDatapack(ctx, r)
	}
	return reconcile.Permanent(fmt.Errorf("update: unknown kind %q", r.Kind))
}

func (b *Backend) updateDatapack(ctx context.Context, r api.DesiredResource) error {
	if err := checkName(r.Kind, r.Name); err != nil {
		return err
	}
	spec, err := datapackSpecOf(r)
	if err != nil {
		return err
	}
	cur, ok := b.datapackAt(r.Name)
	if !ok {
		// The engine only plans an update for something List reported, so this is a
		// resource that vanished between the List and the dispatch. Retryable: the
		// next tick sees it absent and creates it.
		return fmt.Errorf("update datapack %q: not in the current inventory", r.Name)
	}
	// The api-server writes a description only when it is non-empty (UpdateByID),
	// and the field is omitempty on the way out, so a desired state that drops it
	// sends {} and changes nothing. Returning nil there would cache the new
	// generation against work that never happened and no later tick would revisit
	// it — the meko#985 latch, reached from the one direction synchronous verbs do
	// not otherwise expose.
	if spec.Description == "" && cur.Description != "" {
		return reconcile.Permanent(fmt.Errorf(
			"datapack %q: a description cannot be removed through the api-server, only "+
				"replaced; it still reads %q", r.Name, cur.Description))
	}
	return classify(b.api.UpdateDatapack(ctx, cur.ID, mekoclient.UpdateDatapackRequest{
		Description: spec.Description,
	}))
}

// Delete removes a withdrawn resource. A row the api-server no longer has is the
// outcome the caller wanted, so a 404 is success — otherwise a delete that raced a
// concurrent removal would back off forever against something already gone.
func (b *Backend) Delete(ctx context.Context, key reconcile.ResourceKey) error {
	switch key.Kind {
	case KindAgent:
		if _, ok := b.agentAt(key.Name); !ok {
			return nil
		}
		// Cascade, because the engine assumes it. loop.go skips a child's delete
		// when the parent is going too ("the backend cascades the rest"), and
		// meko's DeleteAgent does not: it removes the agent_access and agents rows
		// and leaves the datapacks pointing at an id that no longer exists. Without
		// this, withdrawing an agent together with its datapacks orphans them
		// permanently — the next List cannot report them (their agent is gone), so
		// no later tick can collect them, and each one holds a free-tier creation
		// slot that is never refunded.
		for _, dp := range b.datapacksUnder(key.Name) {
			if err := notFoundIsFine(b.api.DeleteDatapack(ctx, dp.ID)); err != nil {
				return fmt.Errorf("cascade delete datapack %q of agent %q: %w", dp.Name, key.Name, err)
			}
		}
		return notFoundIsFine(b.api.DeleteAgent(ctx, key.Name))
	case KindDatapack:
		cur, ok := b.datapackAt(key.Name)
		if !ok {
			return nil // not in the inventory: nothing to delete
		}
		return notFoundIsFine(b.api.DeleteDatapack(ctx, cur.ID))
	}
	return reconcile.Permanent(fmt.Errorf("delete: unknown kind %q", key.Kind))
}

func notFoundIsFine(err error) error {
	var apiErr *mekoclient.Error
	if errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusNotFound {
		return nil
	}
	return classify(err)
}

// datapacksUnder returns the datapacks the last List saw under an agent, for the
// cascade its delete has to perform itself.
func (b *Backend) datapacksUnder(agent string) []mekoclient.Datapack {
	b.mu.RLock()
	defer b.mu.RUnlock()
	var out []mekoclient.Datapack
	for path, dp := range b.idx.datapack {
		if strings.SplitN(path, sep, 2)[0] == agent {
			out = append(out, dp)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func (b *Backend) agentAt(path string) (mekoclient.Agent, bool) {
	b.mu.RLock()
	defer b.mu.RUnlock()
	a, ok := b.idx.agent[path]
	return a, ok
}

func (b *Backend) datapackAt(path string) (mekoclient.Datapack, bool) {
	b.mu.RLock()
	defer b.mu.RUnlock()
	dp, ok := b.idx.datapack[path]
	return dp, ok
}

// datapackSpecOf unmarshals a datapack's spec. An absent spec is a valid resource
// with every field defaulted, per the "every spec field is optional" property the
// desired-state contract holds to.
func datapackSpecOf(r api.DesiredResource) (datapackSpec, error) {
	var s datapackSpec
	if len(r.Spec) == 0 {
		return s, nil
	}
	if err := json.Unmarshal(r.Spec, &s); err != nil {
		return s, reconcile.Rejected(fmt.Errorf("%s %q: bad spec: %w", r.Kind, r.Name, err))
	}
	return s, nil
}

// classify turns an api-server rejection into the verdict the engine acts on. The
// distinction that matters is whether re-issuing the same call could ever succeed:
// a spec the api-server refuses cannot, and retrying it forever flaps the
// environment to DEGRADED on a call that will always fail.
//
// A free-tier cap (402) is permanent, which is not the intuitive answer. The gate
// counts LIFETIME creations (repository.MetricDatapackCreation, "never decremented
// on delete"), so withdrawing a datapack does not free a slot and no amount of
// retrying gets past it — and it is reachable in the configuration this backend
// targets, since a BYOC api-server runs Cognito-off and DevAuth's account defaults
// to the free tier.
func classify(err error) error {
	if err == nil {
		return nil
	}
	var apiErr *mekoclient.Error
	if !errors.As(err, &apiErr) {
		return err
	}
	switch apiErr.StatusCode {
	case http.StatusBadRequest, http.StatusConflict:
		return reconcile.Rejected(err)
	case http.StatusPaymentRequired:
		// Blocked, not Rejected, for the reason above: the counter is lifetime, so
		// no re-declaration under any name or description gets past it. Telling the
		// operator to rewrite the spec would be the misdirection this vocabulary
		// exists to remove.
		return reconcile.Blocked(err)
	case http.StatusUnauthorized, http.StatusForbidden:
		// A missing or rejected token, a scope the token lacks, or a row this
		// account does not own. None is fixed by asking again, and each is worth
		// naming on the resource instead of hiding in a backoff. Blocked, not
		// Rejected: nothing is wrong with the declaration, so "re-declare it" is the
		// wrong thing to tell whoever reads the row.
		return reconcile.Blocked(err)
	}
	return err
}
