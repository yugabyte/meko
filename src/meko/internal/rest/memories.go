package rest

import (
	"encoding/base64"
	"errors"
	"net/http"
	"regexp"
	"slices"
	"strings"

	"github.com/google/uuid"
	"github.com/labstack/echo/v4"

	"github.com/yugabyte/meko/src/meko/internal/envelope"
	"github.com/yugabyte/meko/src/meko/internal/memory"
	"github.com/yugabyte/meko/src/meko/internal/store"
	"github.com/yugabyte/meko/src/meko/internal/wire"
)

// Page sizes, as Meko Cloud's.
const (
	defaultPage   = 20
	maxPage       = 100
	artifactPage  = 25
	maxArtifactPg = 200
)

// paging reads page and page_size; ok is false when the reply has been written.
func paging(c echo.Context, def, most int) (page, size int, ok bool, err error) {
	if page, ok, err = queryInt(c, "page", 1); !ok {
		return
	}
	if size, ok, err = queryInt(c, "page_size", def); !ok {
		return
	}
	return max(page, 1), min(max(size, 1), most), true, nil
}

func (a *api) listMemories(c echo.Context) error {
	dp, ok, err := a.datapack(c, c.QueryParam("datapack_id"))
	if !ok {
		return err
	}
	page, size, ok, err := paging(c, defaultPage, maxPage)
	if !ok {
		return err
	}
	scope, q := c.QueryParam("scope"), c.QueryParam("q")
	if scope != "" && scope != store.ScopeLearnings && scope != store.ScopeMemories {
		return badRequest(c, "scope must be learnings or memories")
	}
	ms, total, err := a.Service.ListMemoryPage(ctx(c), memory.Page{DatapackID: dp, Scope: scope, Query: q,
		Page: page, PageSize: size})
	if err != nil {
		return serviceError(c, err)
	}
	criteria := map[string]any{"datapack_id": dp, "page": page, "page_size": size}
	if q != "" {
		criteria["q"] = q
	}
	if scope != "" {
		criteria["scope"] = scope
	}
	out := wire.MemoryListOf(ms, criteria, total, page, size)
	out.TopK = 0 // Cloud ranks paid accounts' lists by similarity; Meko lists newest first
	p, err := a.Service.Profile(ctx(c))
	if err != nil {
		return err
	}
	for i := range out.Memories {
		out.Memories[i].UserName, out.Memories[i].UserEmail = fullName(p), p.Email
	}
	return envelope.OK(c, out)
}

func (a *api) deleteMemory(c echo.Context) error {
	dp, ok, err := a.datapack(c, c.QueryParam("datapack_id"))
	if !ok {
		return err
	}
	id := c.Param("memory")
	if _, perr := uuid.Parse(id); perr != nil {
		return badRequest(c, "memory_id must be a valid UUID")
	}
	if err = a.Service.DeleteMemory(ctx(c), dp, id); err != nil {
		if errors.Is(err, memory.ErrPromoted) {
			return notFound(c, "memory not found")
		}
		return serviceError(c, err)
	}
	return envelope.OK(c, map[string]string{"id": id})
}

// memoryHistory is a memory's changes, oldest first: Meko's own route (Cloud has none).
func (a *api) memoryHistory(c echo.Context) error {
	dp, ok, err := a.datapack(c, c.QueryParam("datapack_id"))
	if !ok {
		return err
	}
	u, perr := uuid.Parse(c.Param("memory"))
	if perr != nil {
		return badRequest(c, "memory_id must be a valid UUID")
	}
	id := u.String() // stored ids are lowercase
	h, err := a.Service.MemoryHistory(ctx(c), dp, id)
	if errors.Is(err, memory.ErrPromoted) {
		return notFound(c, "memory not found")
	}
	if err != nil {
		return serviceError(c, err)
	}
	return envelope.OK(c, map[string]any{"memory_id": id, "history": h})
}

// memoryIDs reads a body's datapack and memory ids; ok is false when the reply has been written.
func (a *api) memoryIDs(c echo.Context) (dp, agent string, ids []string, ok bool, err error) {
	var body struct {
		DatapackID string   `json:"datapack_id"`
		AgentID    string   `json:"agent_id"`
		MemoryIDs  []string `json:"memory_ids"`
	}
	if ok, err = bind(c, &body); !ok {
		return
	}
	if dp, ok, err = a.datapack(c, body.DatapackID); !ok {
		return
	}
	if len(body.MemoryIDs) == 0 {
		return "", "", nil, false, badRequest(c, "memory_ids is required")
	}
	ids = make([]string, len(body.MemoryIDs))
	for i, id := range body.MemoryIDs {
		u, perr := uuid.Parse(id)
		if perr != nil || u.String() != strings.ToLower(id) {
			return "", "", nil, false, badRequest(c, "memory_ids must be UUIDs")
		}
		ids[i] = u.String() // memory ids are stored lowercase
	}
	return dp, body.AgentID, ids, true, nil
}

func (a *api) promoteMemories(c echo.Context) error {
	dp, agent, ids, ok, err := a.memoryIDs(c)
	if !ok {
		return err
	}
	res, err := a.Service.PromoteMemories(ctx(c), dp, agent, ids)
	if err != nil && len(res.Inserted) > 0 {
		// Stopped partway: the generic failure, with the ids already promoted, so a retry knows.
		a.Log.Error("promote stopped partway", "promoted", len(res.Inserted), "error", err)
		details := make([]string, len(res.Inserted))
		for i, id := range res.Inserted {
			details[i] = "promoted " + id
		}
		return envelope.FailDetails(c, http.StatusInternalServerError, "INTERNAL_ERROR",
			"Could not promote every memory. Please try again.", details)
	}
	if err != nil {
		return serviceError(c, err)
	}
	return envelope.OK(c, res)
}

func (a *api) keepMemories(c echo.Context) error {
	dp, _, ids, ok, err := a.memoryIDs(c)
	if !ok {
		return err
	}
	n, err := a.Service.KeepMemories(ctx(c), dp, ids)
	if err != nil {
		return serviceError(c, err)
	}
	return envelope.OK(c, map[string]int{"updated": n})
}

func (a *api) listPromoted(c echo.Context) error {
	dp, ok, err := a.datapack(c, c.QueryParam("datapack_id"))
	if !ok {
		return err
	}
	page, size, ok, err := paging(c, defaultPage, maxPage)
	if !ok {
		return err
	}
	ps, total, err := a.Service.PromotedMemories(ctx(c), dp, page, size)
	if err != nil {
		return serviceError(c, err)
	}
	return envelope.OK(c, wire.PromotedListOf(ps, map[string]any{"datapack_id": dp, "page": page, "page_size": size},
		total, page, size))
}

func (a *api) deletePromoted(c echo.Context) error {
	dp, ok, err := a.datapack(c, c.QueryParam("datapack_id"))
	if !ok {
		return err
	}
	if err = a.Service.DeletePromoted(ctx(c), dp, c.Param("memory")); err != nil {
		return serviceError(c, err)
	}
	return envelope.OK(c, map[string]bool{"deleted": true})
}

var sha256Hex = regexp.MustCompile(`^[0-9a-f]{64}$`)

func (a *api) listArtifacts(c echo.Context) error {
	dp, ok, err := a.pathDatapack(c)
	if !ok {
		return err
	}
	page, size, ok, err := paging(c, artifactPage, maxArtifactPg)
	if !ok {
		return err
	}
	arts, total, err := a.Service.ListArtifacts(ctx(c), dp, c.QueryParam("q"), page, size)
	if err != nil {
		return serviceError(c, err)
	}
	items := make([]wire.Artifact, len(arts))
	for i, art := range arts {
		items[i] = wire.ArtifactOf(art)
	}
	return envelope.OK(c, map[string]any{"items": items, "total": total, "page": page, "page_size": size,
		"has_more": page*size < total})
}

// artifact is the file a route names by hash, with its bytes; ok is false when the reply has been
// written.
func (a *api) artifact(c echo.Context) (store.Artifact, bool, error) {
	dp, ok, err := a.pathDatapack(c)
	if !ok {
		return store.Artifact{}, false, err
	}
	hash := strings.ToLower(c.Param("hash"))
	if !sha256Hex.MatchString(hash) {
		return store.Artifact{}, false, envelope.Fail(c, http.StatusBadRequest, "INVALID_PARAM",
			"content_hash must be 64 hex characters")
	}
	art, err := a.Service.GetArtifact(ctx(c), dp, hash)
	if err != nil {
		if errors.Is(err, memory.ErrNotFound) {
			return store.Artifact{}, false, envelope.Fail(c, http.StatusNotFound, "ARTIFACT_NOT_FOUND", "artifact not found")
		}
		return store.Artifact{}, false, err
	}
	return art, true, nil
}

// artifactDownloadURL has no URL to give (the bytes are in the database), so meko_ui falls back to
// GET .../artifacts/:hash.
func (a *api) artifactDownloadURL(c echo.Context) error {
	art, ok, err := a.artifact(c)
	if !ok {
		return err
	}
	return envelope.OK(c, map[string]any{"stored_in": "db", "filename": art.Filename, "content_type": art.ContentType,
		"size_bytes": art.SizeBytes})
}

func (a *api) getArtifact(c echo.Context) error {
	art, ok, err := a.artifact(c)
	if !ok {
		return err
	}
	out := wire.ArtifactOf(art)
	out.ContentBase64 = base64.StdEncoding.EncodeToString(art.Bytes)
	return envelope.OK(c, out)
}

// memoryMap is the datapack's memories laid out by meaning, and where ?q= lands among them (Meko's
// own; Cloud has none).
func (a *api) memoryMap(c echo.Context) error {
	id, ok, err := a.pathDatapack(c)
	if !ok {
		return err
	}
	m, err := a.Service.MemoryMap(ctx(c), id, strings.TrimSpace(c.QueryParam("q")), csv(c.QueryParam("ids")))
	if err != nil {
		return serviceError(c, err)
	}
	return envelope.OK(c, m)
}

// searchMap is the memories, knowledge-base chunks and conversation turns nearest to ?q= among
// ?sources= (all by default), laid out by meaning, with ?hits= (what the search returned) drawn
// too: Search's Graph view (Meko's own).
func (a *api) searchMap(c echo.Context) error {
	id, ok, err := a.pathDatapack(c)
	if !ok {
		return err
	}
	q := strings.TrimSpace(c.QueryParam("q"))
	if q == "" {
		return badRequest(c, "q is required")
	}
	all := []string{store.MapMemory, store.MapKnowledge, store.MapConversation}
	sources := csv(c.QueryParam("sources"))
	for _, s := range sources {
		if !slices.Contains(all, s) {
			return badRequest(c, "sources takes "+strings.Join(all, ", "))
		}
	}
	if len(sources) == 0 {
		sources = all
	}
	m, err := a.Service.SearchMap(ctx(c), id, q, sources, csv(c.QueryParam("hits")))
	if err != nil {
		return serviceError(c, err)
	}
	return envelope.OK(c, m)
}

// csv is a comma-separated query parameter's values, trimmed and lowercased, blanks left out.
func csv(param string) []string {
	var out []string
	for _, s := range strings.Split(param, ",") {
		if s = strings.ToLower(strings.TrimSpace(s)); s != "" {
			out = append(out, s)
		}
	}
	return out
}
