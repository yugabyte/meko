// Package rest is the /api/v1 subset meko_ui calls, with Meko Cloud's routes and reply shapes in
// the {data, error} envelope: the local user, datapacks and their metrics, artifacts, memories and
// learnings, conversations, and knowledge files. Shapes MCP shares come from internal/wire; the rest are here.
// The linked Meko Cloud's datapacks go through the same routes (cloud.go).
package rest

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"

	"github.com/google/uuid"
	"github.com/labstack/echo/v4"

	"github.com/yugabyte/meko/src/meko/internal/callhome"
	"github.com/yugabyte/meko/src/meko/internal/cloudlink"
	"github.com/yugabyte/meko/src/meko/internal/cloudsync"
	"github.com/yugabyte/meko/src/meko/internal/envelope"
	"github.com/yugabyte/meko/src/meko/internal/memory"
)

// Options configure the routes.
type Options struct {
	Service *memory.Service
	// Port is the server's, for the MCP URL provision-status gives.
	Port int
	// ClientToken is what provision-status gives clients to send.
	ClientToken string
	// Credential is the credential a request got in with (admin, client or none), set by the
	// server's auth check.
	Credential func(echo.Context) string
	// UsageStats is the usage stats' setting the web UI shows; nil leaves its routes out.
	UsageStats UsageStats
	// Tool runs one of this Meko's MCP tools in-process, as /mcp does (mcp.Call), for the routes that
	// answer what a tool does: a datapack's search and memories.
	Tool func(ctx context.Context, cred, name string, args map[string]any) (string, error)
	// Cloud is the Meko Cloud link: the routes that take a datapack send one this Meko doesn't have to
	// the linked Meko Cloud (cloud.go). Nil leaves Meko Cloud out.
	Cloud *cloudlink.Service
	// Sync syncs this Meko's datapacks to the linked Meko Cloud; nil leaves its routes out.
	Sync *cloudsync.Syncer
	Log  *slog.Logger
}

// UsageStats reads and sets whether usage stats are sent (internal/callhome).
type UsageStats interface {
	State(ctx context.Context) (callhome.State, error)
	SetEnabled(ctx context.Context, on bool) (callhome.State, error)
}

type api struct {
	Options
}

// keyFor is the client token for a request that sent a credential, and "" otherwise. Not for
// browsers, with local access (every user of the computer) or remote (anyone who
// reaches its port: there's no login), nor auth = "none": the token lets a caller do what neither
// does (knowledgebase_upload_file reads a path as Meko's user), and keeps working when access or
// auth changes. Those callers run meko token for it.
func (a *api) keyFor(c echo.Context) string {
	switch a.Credential(c) {
	case "admin", "client":
		return a.ClientToken
	}
	return ""
}

// here gives a link the service made for this computer (http://127.0.0.1:<port>/...) to a browser's
// request with the origin the browser used, which it can reach
// (from another computer, with remote access). Other links are given as they are.
func (a *api) here(c echo.Context, link string) string {
	if cred := a.Credential(c); cred != "remote" && cred != "local" {
		return link
	}
	base := a.Service.URL("")
	if link == "" || !strings.HasPrefix(link, base) {
		return link
	}
	scheme := "http"
	if c.Request().TLS != nil {
		scheme = "https"
	}
	return scheme + "://" + c.Request().Host + strings.TrimPrefix(link, base)
}

// Register adds the routes to the /api/v1 group.
func Register(g *echo.Group, o Options) {
	if o.Log == nil {
		o.Log = slog.New(slog.DiscardHandler)
	}
	if o.Credential == nil {
		o.Credential = func(echo.Context) string { return "" }
	}
	a := &api{o}
	g.GET("/auth/config", a.authConfig)
	g.GET("/auth/me", a.me)
	g.PATCH("/auth/me", a.setOnboarding)
	g.PATCH("/auth/me/profile", a.setProfile)
	g.GET("/auth/me/usage", a.usage)
	if o.UsageStats != nil {
		g.GET("/settings/usage-stats", a.usageStats)
		g.PATCH("/settings/usage-stats", a.setUsageStats)
	}

	g.GET("/settings/memory-extractor", a.extractorStatus)
	g.PUT("/settings/memory-extractor", a.setExtractor)
	g.POST("/settings/memory-extractor/run", a.runExtractor)
	g.GET("/settings/answers", a.answersStatus)
	g.PUT("/settings/answers", a.setAnswers)
	g.GET("/settings/call-contents", a.callContents)
	g.PUT("/settings/call-contents", a.setCallContents)
	g.GET("/calls", a.listCalls)
	g.GET("/calls/:id", a.call)
	g.DELETE("/calls/contents", a.clearCallContents)

	// The routes that take a datapack in the path and that Meko Cloud has too go through byID: one
	// this Meko doesn't have goes to Meko Cloud while it's linked. Meko's own (usage, export, the maps,
	// answer, memory extraction) answer for this Meko's alone; search and memories call Meko Cloud's
	// tools (callTool), and Context Gaps keeps Meko Cloud's datapacks' questions here (questions.go).
	g.GET("/datapacks", a.listDatapacks)
	g.POST("/datapacks", a.createDatapack)
	g.GET("/datapacks/:id", a.byID(a.describeDatapack))
	g.PATCH("/datapacks/:id", a.byID(a.updateDatapack))
	g.DELETE("/datapacks/:id", a.byID(a.deleteDatapack))
	g.GET("/datapacks/:id/provision-status", a.byID(a.provisionStatus))
	g.GET("/datapacks/:id/agents", a.byID(a.agents))
	g.GET("/datapacks/:id/metrics", a.byID(a.metrics))
	g.GET("/datapacks/:id/token-savings", a.byID(a.tokenSavings))
	g.GET("/datapacks/:id/usage", a.callUsage)
	g.GET("/datapacks/:id/export", a.exportDatapack)
	g.GET("/datapacks/export", a.exportDatapack) // ?name=: one of this Meko's, by name
	g.GET("/datapacks/:id/memory-map", a.memoryMap)
	g.GET("/datapacks/:id/search-map", a.searchMap)
	g.POST("/datapacks/:id/search", a.search)
	g.POST("/datapacks/:id/answer", a.answer)
	g.POST("/datapacks/:id/memories", a.addMemory)

	g.GET("/datapacks/:id/artifacts", a.byID(a.listArtifacts))
	g.POST("/datapacks/:id/artifacts/:hash/download-url", a.byID(a.artifactDownloadURL))
	g.GET("/datapacks/:id/artifacts/:hash", a.byID(a.getArtifact))

	g.GET("/memories", a.listMemories)
	g.DELETE("/memories/:memory", a.deleteMemory)
	g.GET("/memories/:memory/history", a.memoryHistory)
	g.POST("/promote-memories", a.promoteMemories)
	g.POST("/keep-as-memory", a.keepMemories)
	g.GET("/promoted-memories", a.listPromoted)
	g.DELETE("/promoted-memories/:memory", a.deletePromoted)

	g.GET("/datapacks/:id/conversations", a.byID(a.listConversations))
	g.GET("/datapacks/:id/conversations/count", a.byID(a.countConversations))
	g.GET("/datapacks/:id/conversations/:conversation", a.byID(a.conversation))
	g.PATCH("/datapacks/:id/conversation-search-opt-out", a.byID(a.setOptOut))
	g.GET("/datapacks/:id/memory-extraction", a.datapackExtraction)
	g.PATCH("/datapacks/:id/memory-extraction", a.setDatapackExtraction)

	// Only Meko Cloud shares datapacks: these go there while it's linked (cloudOnly).
	g.GET("/datapacks/incoming-shares", a.cloudOnly)
	g.POST("/datapacks/invite-action", a.cloudOnly)
	g.POST("/share/:token/redeem", a.cloudOnly)
	g.POST("/datapacks/:id/share", a.cloudOnly)
	g.POST("/datapacks/:id/invite/:invite/resend", a.cloudOnly)
	g.DELETE("/datapacks/:id/invite/:invite", a.cloudOnly)
	g.GET("/datapacks/:id/member-list", a.cloudOnly)
	g.GET("/datapacks/:id/outgoing-shares", a.cloudOnly)
	g.GET("/datapacks/:id/share-link", a.cloudOnly)
	g.PUT("/datapacks/:id/share-link", a.cloudOnly)

	a.registerKnowledge(g)
	a.registerQuestions(g)
	a.registerProjects(g)
	a.registerCloudSync(g)
}

// Errors as Meko Cloud's API replies them.
func badRequest(c echo.Context, msg string) error {
	return envelope.Fail(c, http.StatusBadRequest, "VALIDATION_ERROR", msg)
}

func notFound(c echo.Context, msg string) error {
	return envelope.Fail(c, http.StatusNotFound, "NOT_FOUND", msg)
}

// datapack is the datapack a request names (in the path as :id, or the query or body as
// datapack_id): it must be a UUID (else 400) and exist (else 404). ok is false when the reply has
// been written.
func (a *api) datapack(c echo.Context, id string) (string, bool, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		return "", false, badRequest(c, "datapack_id is required")
	}
	u, err := uuid.Parse(id)
	if err != nil {
		return "", false, badRequest(c, "datapack_id must be a valid UUID")
	}
	id = u.String() // ids are stored lowercase with hyphens; Cloud's uuid column takes any form

	if _, err := a.Service.Datapack(c.Request().Context(), id); err != nil {
		if errors.Is(err, memory.ErrNoDatapack) {
			return "", false, notFound(c, fmt.Sprintf("datapack %q not found", id))
		}
		return "", false, err
	}
	return id, true, nil
}

// pathDatapack is the datapack in the path.
func (a *api) pathDatapack(c echo.Context) (string, bool, error) { return a.datapack(c, c.Param("id")) }

// queryInt is an integer query parameter, def when absent; ok is false when it isn't a number and
// the reply has been written.
func queryInt(c echo.Context, name string, def int) (int, bool, error) {
	v := c.QueryParam(name)
	if v == "" {
		return def, true, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return 0, false, badRequest(c, name+" must be a number")
	}
	return n, true, nil
}

// bind reads a JSON body; ok is false when it isn't valid JSON and the reply has been written.
func bind(c echo.Context, v any) (bool, error) {
	if err := (&echo.DefaultBinder{}).BindBody(c, v); err != nil {
		return false, envelope.Fail(c, http.StatusBadRequest, "INVALID_JSON", "The request body isn't valid JSON.")
	}
	return true, nil
}

// created replies 201 with data.
func created(c echo.Context, data any) error {
	return c.JSON(http.StatusCreated, envelope.Response{Data: data})
}

// serviceError replies for the service's errors a caller can act on; any other is returned for
// the server's handler, which logs it and replies with a generic 500.
func serviceError(c echo.Context, err error) error {
	switch {
	case errors.Is(err, memory.ErrNoDatapack):
		return notFound(c, "datapack not found")
	case errors.Is(err, memory.ErrNotFound):
		return notFound(c, "not found")
	case errors.Is(err, memory.ErrNoConversation):
		return notFound(c, "conversation not found")
	case errors.Is(err, memory.ErrDuplicateName):
		return envelope.Fail(c, http.StatusConflict, "CONFLICT", "A datapack with that name already exists.")
	case errors.Is(err, memory.ErrBadRequest):
		return badRequest(c, strings.TrimPrefix(err.Error(), memory.ErrBadRequest.Error()+": "))
	case errors.Is(err, memory.ErrStorageFull):
		// 402, as Meko Cloud answers a plan's limit.
		return envelope.Fail(c, http.StatusPaymentRequired, "STORAGE_LIMIT_EXCEEDED", err.Error())
	case errors.Is(err, memory.ErrDatapackSyncing):
		return envelope.Fail(c, http.StatusConflict, "DATAPACK_SYNCING", err.Error())
	}
	return err
}

func ctx(c echo.Context) context.Context { return c.Request().Context() }
