package memory

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/yugabyte/meko/backend/internal/auth"
	"github.com/yugabyte/meko/backend/internal/engine"
	"github.com/yugabyte/meko/backend/internal/paths"
	"github.com/yugabyte/meko/backend/internal/store"
	"github.com/yugabyte/meko/backend/internal/storeapi"
	"github.com/yugabyte/meko/backend/internal/supervisor"
)

// The embedders a test can start the engine with.
const (
	hashEmbedder = "hash" // deterministic, no download: for plumbing
	realEmbedder = ""     // the real model, from a cache shared across runs (CI keeps it)
)

// harness is the real thing minus the HTTP server: the store, its API on a unix socket, the Node
// engine under the supervisor, and the service over them.
type harness struct {
	t      *testing.T
	l      paths.Layout
	st     *store.Store
	svc    *Service
	sup    *supervisor.Supervisor
	dp     string
	userID string
	token  string
	node   string
	dir    string
	o      engine.Options
}

// engineDir is the built engine in this source tree. Without it (or Node) the test is skipped,
// except in CI, where make test builds it first and a skip would hide a broken build.
func engineDir(t *testing.T) (node, dir string) {
	t.Helper()
	missing := func(err error) {
		if os.Getenv("CI") != "" {
			t.Fatal(err)
		}
		t.Skipf("%v (make test builds the engine)", err)
	}
	dir, err := engine.FindDir(engine.Options{Dir: filepath.Join("..", "..", "engine")})
	if err != nil {
		missing(err)
	}
	if node, err = engine.FindNode(engine.Options{}); err != nil {
		missing(err)
	}
	return node, dir
}

// modelsDir is where the real model is kept between runs: MEKO_TEST_MODELS, or
// engine/.models in this source tree (ignored by git).
func modelsDir(t *testing.T, dir string) string {
	t.Helper()
	if d := os.Getenv("MEKO_TEST_MODELS"); d != "" {
		return d
	}
	d := filepath.Join(dir, ".models")
	if err := os.MkdirAll(d, 0o700); err != nil {
		t.Fatal(err)
	}
	return d
}

func newHarness(t *testing.T, embedder string) *harness {
	t.Helper()
	node, dir := engineDir(t)
	ctx := context.Background()
	root, err := os.MkdirTemp("/tmp", "mk-") // short: unix sockets have a path length limit
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	l := paths.New(root)
	if embedder == realEmbedder {
		l.ModelsDir = modelsDir(t, dir)
	}
	if err = l.Ensure(); err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(l.DB)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err = st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	dp, err := st.EnsureDefaultDatapack(ctx)
	if err != nil {
		t.Fatal(err)
	}
	userID, err := st.EnsureLocalUserID(ctx)
	if err != nil {
		t.Fatal(err)
	}
	token, err := auth.NewToken()
	if err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	ln, err := storeapi.Listen(l.StoreSock)
	if err != nil {
		t.Fatal(err)
	}
	api := storeapi.New(st, token, log, nil)
	go func() { _ = api.Serve(ln) }()
	t.Cleanup(func() { _ = api.Shutdown(context.Background()) })

	h := &harness{t: t, l: l, st: st, dp: dp, userID: userID, token: token, node: node, dir: dir,
		o: engine.Options{TestEmbedder: embedder}}
	h.svc = New(st, engine.NewClient(l.EngineSock, token), Options{UserID: userID, Log: log,
		FilesDir: l.KnowledgeFiles, BaseURL: "http://127.0.0.1:7778"})
	h.startEngine()
	return h
}

// startEngine starts the engine under a new supervisor and waits until it's ready.
func (h *harness) startEngine() {
	h.t.Helper()
	h.launchEngine()
	h.waitEngine("ready")
}

// launchEngine starts the engine under a new supervisor.
func (h *harness) launchEngine() {
	h.t.Helper()
	h.sup = supervisor.New(slog.New(slog.NewTextHandler(io.Discard, nil)))
	sp := engine.Spec(h.node, h.dir, h.l, h.token, h.o)
	sp.MinBackoff, sp.MaxBackoff = 50*time.Millisecond, 100*time.Millisecond
	h.sup.Start(sp)
	sup := h.sup
	h.t.Cleanup(func() { _ = sup.Stop(context.Background()) })
}

func (h *harness) stopEngine() {
	h.t.Helper()
	if err := h.sup.Stop(context.Background()); err != nil {
		h.t.Fatal(err)
	}
}

// waitEngine waits (2 minutes at most: the first run downloads the model) for the engine's state.
func (h *harness) waitEngine(state string) EngineStatus {
	h.t.Helper()
	var st EngineStatus
	for deadline := time.Now().Add(2 * time.Minute); time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
		if st = h.svc.EngineStatus(context.Background()); st.State == state {
			return st
		}
	}
	log, _ := os.ReadFile(h.l.EngineLog)
	h.t.Fatalf("the engine never got to %s: %+v\nengine.log:\n%s", state, st, log)
	return st
}
