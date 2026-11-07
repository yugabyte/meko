// Package storeapi is the store API the context engine calls on run/store.sock: mem0's two storage
// interfaces (VectorStore and HistoryManager) as HTTP/JSON routes over internal/store, plus the
// context engine's hello. It listens only on the unix socket (0600, in the 0700 run folder) and needs the
// per-start internal token. The context engine keeps no database of its own: every row goes through here.
package storeapi

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/labstack/echo/v4"
	"github.com/labstack/echo/v4/middleware"

	"github.com/yugabyte/meko/src/meko/internal/auth"
	"github.com/yugabyte/meko/src/meko/internal/envelope"
	"github.com/yugabyte/meko/src/meko/internal/store"
)

// BodyLimit caps a request: an insert of a batch of vectors and payloads.
const BodyLimit = "16M"

// entitiesSuffix marks mem0's entity collection for a datapack: "<datapack id>_entities".
const entitiesSuffix = "_entities"

// Server serves the store API.
type Server struct {
	st      *store.Store
	log     *slog.Logger
	srv     *http.Server
	onHello func(store.Embedder)
}

// New builds the store API over st. onHello, if set, hears the embedding model of each context
// engine that says hello and is accepted.
func New(st *store.Store, token string, log *slog.Logger, onHello func(store.Embedder)) *Server {
	e := echo.New()
	e.HideBanner, e.HidePort = true, true
	e.HTTPErrorHandler = envelope.ErrorHandler(log)
	s := &Server{st: st, log: log, onHello: onHello}
	e.Use(middleware.Recover())
	e.Use(func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) error {
			if !auth.CheckBearer(c.Request().Header.Get(echo.HeaderAuthorization), token) {
				return envelope.Fail(c, http.StatusUnauthorized, "UNAUTHORIZED", "Missing or wrong internal token.")
			}
			return next(c)
		}
	})
	e.Use(middleware.BodyLimit(BodyLimit))

	v1 := e.Group("/v1")
	v1.POST("/context_engine/hello", s.hello)
	c := v1.Group("/collections/:collection")
	c.POST("/insert", s.insert)
	c.POST("/search", s.search)
	c.POST("/keyword-search", s.keywordSearch)
	c.POST("/list", s.list)
	c.GET("/vectors/:id", s.get)
	c.PUT("/vectors/:id", s.update)
	c.DELETE("/vectors/:id", s.remove)
	c.DELETE("", s.deleteCollection)
	v1.POST("/history/:datapack", s.addHistory)
	v1.GET("/history/:datapack/:memory", s.history)
	s.srv = &http.Server{Handler: e, ReadHeaderTimeout: 10 * time.Second}
	return s
}

// ServeHTTP lets tests drive the API without a socket.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) { s.srv.Handler.ServeHTTP(w, r) }

// Listen opens the unix socket at path, replacing one a killed server left, readable only by this
// user.
func Listen(path string) (net.Listener, error) {
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	ln, err := net.Listen("unix", path)
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(path, 0o600); err != nil {
		ln.Close()
		return nil, err
	}
	return ln, nil
}

// Serve answers requests on ln until Shutdown; it returns nil after a shutdown.
func (s *Server) Serve(ln net.Listener) error {
	if err := s.srv.Serve(ln); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// Shutdown stops accepting requests and waits for those in flight, until ctx ends.
func (s *Server) Shutdown(ctx context.Context) error { return s.srv.Shutdown(ctx) }

// fail maps the store's errors to replies; anything else is an internal error (logged, generic).
func fail(c echo.Context, err error) error {
	var changed *store.ModelChangedError
	switch {
	case errors.As(err, &changed):
		return envelope.Fail(c, http.StatusConflict, "MODEL_CHANGED", changed.Error())
	case errors.Is(err, store.ErrNoVectors):
		return envelope.Fail(c, http.StatusServiceUnavailable, "NO_VECTORS", err.Error())
	case errors.Is(err, store.ErrNotFound):
		return envelope.Fail(c, http.StatusNotFound, "NOT_FOUND", "No such row.")
	case errors.Is(err, store.ErrNoDatapack):
		return envelope.Fail(c, http.StatusNotFound, "NO_DATAPACK", "No such datapack.")
	case errors.Is(err, store.ErrOtherDatapack):
		return envelope.Fail(c, http.StatusConflict, "CONFLICT", err.Error())
	case errors.Is(err, store.ErrBadFilter), errors.Is(err, store.ErrBadVector):
		return envelope.Fail(c, http.StatusBadRequest, "BAD_REQUEST", err.Error())
	}
	return err
}

// bind decodes the body. Its error (like collection's) is an echo.HTTPError, which the error
// handler writes as an envelope; the handler just returns it.
func bind(c echo.Context, v any) error {
	if err := c.Bind(v); err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "The body isn't the JSON this route takes.")
	}
	return nil
}

// collection maps mem0's collection name to a table and a datapack.
func collection(c echo.Context) (store.Kind, string, error) {
	name := c.Param("collection")
	k := store.Memories
	if dp, ok := strings.CutSuffix(name, entitiesSuffix); ok {
		k, name = store.Entities, dp
	}
	if name == "" {
		return 0, "", echo.NewHTTPError(http.StatusBadRequest, "The collection names no datapack.")
	}
	return k, name, nil
}

func (s *Server) hello(c echo.Context) error {
	var e store.Embedder
	if err := bind(c, &e); err != nil {
		return err
	}
	if e.Model == "" || e.Dims <= 0 {
		return envelope.Fail(c, http.StatusBadRequest, "BAD_REQUEST", "The hello needs a model and its dimension.")
	}
	if err := s.st.EnsureVectors(c.Request().Context(), e); err != nil {
		var changed *store.ModelChangedError
		if errors.As(err, &changed) {
			s.log.Error("context engine refused: its embedding model isn't the one the vectors were made with",
				"have", changed.Have.Model, "got", changed.Got.Model)
		}
		return fail(c, err)
	}
	s.log.Info("context engine said hello", "provider", e.Provider, "model", e.Model, "dims", e.Dims)
	if s.onHello != nil {
		s.onHello(e)
	}
	return envelope.OK(c, map[string]bool{"ok": true})
}

type insertReq struct {
	IDs      []string         `json:"ids"`
	Vectors  [][]float32      `json:"vectors"`
	Payloads []map[string]any `json:"payloads"`
}

func (s *Server) insert(c echo.Context) error {
	k, dp, err := collection(c)
	if err != nil {
		return err
	}
	var r insertReq
	if err := bind(c, &r); err != nil {
		return err
	}
	if len(r.IDs) != len(r.Vectors) || len(r.IDs) != len(r.Payloads) {
		return envelope.Fail(c, http.StatusBadRequest, "BAD_REQUEST", "ids, vectors and payloads must be the same length.")
	}
	if err := s.st.InsertVectors(c.Request().Context(), k, dp, r.IDs, r.Vectors, r.Payloads); err != nil {
		return fail(c, err)
	}
	return envelope.OK(c, map[string]int{"inserted": len(r.IDs)})
}

type searchReq struct {
	Vector  []float32    `json:"vector"`
	Query   string       `json:"query"`
	TopK    int          `json:"top_k"`
	Filters store.Filter `json:"filters"`
}

type results struct {
	Results []store.Record `json:"results"`
	Total   *int           `json:"total,omitempty"`
}

func (s *Server) search(c echo.Context) error {
	k, dp, err := collection(c)
	if err != nil {
		return err
	}
	var r searchReq
	if err = bind(c, &r); err != nil {
		return err
	}
	recs, err := s.st.SearchVectors(c.Request().Context(), k, dp, r.Vector, r.TopK, r.Filters)
	if err != nil {
		return fail(c, err)
	}
	return envelope.OK(c, results{Results: recs})
}

// keywordSearch is mem0's optional keywordSearch: memories only (mem0 never asks the entity store).
func (s *Server) keywordSearch(c echo.Context) error {
	k, dp, err := collection(c)
	if err != nil {
		return err
	}
	var r searchReq
	if err = bind(c, &r); err != nil {
		return err
	}
	if k != store.Memories {
		return envelope.OK(c, results{Results: []store.Record{}})
	}
	recs, err := s.st.KeywordSearch(c.Request().Context(), dp, r.Query, r.TopK, r.Filters)
	if err != nil {
		return fail(c, err)
	}
	return envelope.OK(c, results{Results: recs})
}

func (s *Server) list(c echo.Context) error {
	k, dp, err := collection(c)
	if err != nil {
		return err
	}
	var r searchReq
	if err = bind(c, &r); err != nil {
		return err
	}
	recs, total, err := s.st.ListVectors(c.Request().Context(), k, dp, r.Filters, r.TopK)
	if err != nil {
		return fail(c, err)
	}
	return envelope.OK(c, results{Results: recs, Total: &total})
}

// get answers null for a missing id, as mem0's VectorStore.get does.
func (s *Server) get(c echo.Context) error {
	k, dp, err := collection(c)
	if err != nil {
		return err
	}
	rec, err := s.st.GetVector(c.Request().Context(), k, dp, c.Param("id"))
	if err != nil {
		return fail(c, err)
	}
	if rec == nil {
		return envelope.OK(c, nil)
	}
	return envelope.OK(c, rec)
}

type updateReq struct {
	Vector  []float32      `json:"vector"`
	Payload map[string]any `json:"payload"`
}

func (s *Server) update(c echo.Context) error {
	k, dp, err := collection(c)
	if err != nil {
		return err
	}
	var r updateReq
	if err := bind(c, &r); err != nil {
		return err
	}
	if err := s.st.UpdateVector(c.Request().Context(), k, dp, c.Param("id"), r.Vector, r.Payload); err != nil {
		return fail(c, err)
	}
	return envelope.OK(c, map[string]bool{"ok": true})
}

func (s *Server) remove(c echo.Context) error {
	k, dp, err := collection(c)
	if err != nil {
		return err
	}
	if err := s.st.DeleteVector(c.Request().Context(), k, dp, c.Param("id")); err != nil {
		return fail(c, err)
	}
	return envelope.OK(c, map[string]bool{"ok": true})
}

// deleteCollection is mem0's deleteCol, which would drop a datapack's memories: refused.
func (s *Server) deleteCollection(c echo.Context) error {
	return envelope.Fail(c, http.StatusForbidden, "FORBIDDEN", "The context engine can't drop a datapack's memories.")
}

func (s *Server) addHistory(c echo.Context) error {
	var r struct {
		Entries []store.HistoryEntry `json:"entries"`
	}
	if err := bind(c, &r); err != nil {
		return err
	}
	if err := s.st.AddHistory(c.Request().Context(), c.Param("datapack"), r.Entries); err != nil {
		return fail(c, err)
	}
	return envelope.OK(c, map[string]int{"added": len(r.Entries)})
}

func (s *Server) history(c echo.Context) error {
	h, err := s.st.History(c.Request().Context(), c.Param("datapack"), c.Param("memory"))
	if err != nil {
		return fail(c, err)
	}
	return envelope.OK(c, h)
}
