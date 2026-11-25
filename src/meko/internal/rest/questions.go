package rest

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/labstack/echo/v4"

	"github.com/yugabyte/meko/src/meko/internal/envelope"
	"github.com/yugabyte/meko/src/meko/internal/memory"
	"github.com/yugabyte/meko/src/meko/internal/store"
	"github.com/yugabyte/meko/src/meko/internal/wire"
)

// Context Gaps (Meko's own; docs/api.md): a datapack's questions, the searches Meko answered
// poorly, and what was done about each. This Meko's datapacks, and the linked Meko Cloud's, whose
// searches pass through /mcp: their questions are kept here too, under the datapack's id.

func (a *api) registerQuestions(g *echo.Group) {
	g.GET("/datapacks/:id/questions", a.listQuestions)
	g.POST("/datapacks/:id/questions/mark", a.markQuestion)
	g.POST("/datapacks/:id/questions/teach", a.teachQuestion)
	g.DELETE("/datapacks/:id/questions/asks", a.forgetAsks)
}

// questionsDatapack is the datapack in the path: this Meko's, or, while Meko Cloud is linked, an id
// this Meko doesn't have, taken as Meko Cloud's (cloud). Any credential gets a Meko Cloud datapack's
// questions: they're what this Meko recorded of the searches it passed on, and reading them never
// calls Meko Cloud or uses its key. Teaching one writes its memory to Meko Cloud with the key, for
// any credential too, as POST /datapacks/:id/memories does. ?location= picks
// the side: the one route left that takes it, since Kode links a Meko Cloud datapack's questions
// to their own page. ok is false when the reply has been written.
func (a *api) questionsDatapack(c echo.Context) (id string, cloud, ok bool, err error) {
	switch c.QueryParam("location") {
	case "local":
		id, ok, err = a.pathDatapack(c)
		return id, false, ok, err
	case "cloud", "":
	default:
		return "", false, false, badRequest(c, "location must be local or cloud")
	}
	u, perr := uuid.Parse(strings.TrimSpace(c.Param("id")))
	if perr != nil {
		return "", false, false, badRequest(c, "datapack_id must be a valid UUID")
	}
	id = u.String()
	if c.QueryParam("location") == "" {
		if _, err = a.Service.Datapack(ctx(c), id); !errors.Is(err, memory.ErrNoDatapack) {
			return id, false, err == nil, err
		}
	}
	if linked, lerr := a.linked(); lerr != nil || !linked {
		if c.QueryParam("location") == "cloud" {
			return "", false, false, notLinkedOr(c, lerr)
		}
		if lerr != nil {
			return "", false, false, lerr
		}
		return "", false, false, notFound(c, fmt.Sprintf("datapack %q not found", id))
	}
	return id, true, true, nil
}

// questionRow is a question as the routes reply it.
func questionRow(q memory.Question) map[string]any {
	var marked any
	if q.MarkedAt != nil {
		marked = wire.Time(*q.MarkedAt)
	}
	var taught, dismissal any
	if t := q.Taught; t != nil {
		taught = map[string]any{"text": t.Text, "memory_id": orNil(t.Memory), "at": wire.Time(t.At),
			"conversation_id": orNil(t.By)}
	}
	if d := q.Dismissal; d != nil {
		dismissal = map[string]any{"conversation_id": orNil(d.By), "memory_id": orNil(d.Memory),
			"memory_text": orNil(d.MemoryText)}
	}
	return map[string]any{"key": q.Key, "query": q.Query, "quality": q.Quality, "tool": q.Tool,
		"results": q.Results, "top_score": q.TopScore, "top_raw": q.TopRaw, "asks": q.Asks, "agents": q.Agents,
		"conversations": q.Conversations, "asked_by_conversation": q.AskedBy, "first_at": wire.Time(q.FirstAt),
		"last_at": wire.Time(q.LastAt), "state": q.State, "marked_at": marked, "taught": taught,
		"dismissal": dismissal}
}

func orNil(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// listQuestions is GET /datapacks/:id/questions?state=open|dismissed|taught|all (open by default),
// after= and after_key= (the last question shown: the page starts after it) and conversation_id=
// (which sets each question's asked_by_conversation).
func (a *api) listQuestions(c echo.Context) error {
	id, cloud, ok, err := a.questionsDatapack(c)
	if !ok {
		return err
	}
	f := memory.QuestionFilter{State: c.QueryParam("state"), Cloud: cloud,
		Conversation: strings.TrimSpace(c.QueryParam("conversation_id"))}
	switch f.State {
	case "":
		f.State = memory.QuestionOpen
	case memory.QuestionOpen, memory.QuestionDismissed, memory.QuestionTaught, "all":
	default:
		return badRequest(c, "state must be open, dismissed, taught or all")
	}
	if after := c.QueryParam("after"); after != "" {
		t, perr := time.Parse(time.RFC3339Nano, after)
		if perr != nil {
			return badRequest(c, "after must be a time (RFC 3339), the last question's last_at")
		}
		f.After = &memory.QuestionCursor{LastAt: t, Key: c.QueryParam("after_key")}
	}
	qs, err := a.Service.ListQuestions(ctx(c), id, f)
	if err != nil {
		return serviceError(c, err)
	}
	rows := make([]map[string]any, len(qs.Questions))
	for i, q := range qs.Questions {
		rows[i] = questionRow(q)
	}
	var since any
	if qs.Since != nil {
		since = wire.Time(*qs.Since)
	}
	location := "local"
	if cloud {
		location = "cloud"
	}
	return envelope.OK(c, map[string]any{"questions": rows, "more": qs.More, "counts": qs.Counts, "open": qs.Open,
		"searches": qs.Searches, "since": since, "location": location})
}

// markQuestion is POST /datapacks/:id/questions/mark {key, state, text?, memory_id?, conversation_id?}:
// dismiss a question, mark it taught, or restore it (state null: a dismissal is cleared, what was
// taught is kept). conversation_id says an agent did it, in that conversation; without it, a person
// did. Marking it taught records what an agent saved itself (text, and memory_id when known); a
// person teaches with teach.
func (a *api) markQuestion(c echo.Context) error {
	id, _, ok, err := a.questionsDatapack(c)
	if !ok {
		return err
	}
	var body struct {
		Key            string  `json:"key"`
		State          *string `json:"state"`
		Text           string  `json:"text"`
		MemoryID       string  `json:"memory_id"`
		ConversationID string  `json:"conversation_id"`
	}
	if ok, err := bind(c, &body); !ok {
		return err
	}
	if body.Key == "" {
		return badRequest(c, "key is required")
	}
	m := store.QuestionMark{State: memory.QuestionOpen, By: strings.TrimSpace(body.ConversationID),
		Memory: strings.TrimSpace(body.MemoryID)}
	if body.State != nil {
		m.State = *body.State
	}
	switch m.State {
	case memory.QuestionOpen, memory.QuestionDismissed:
	case memory.QuestionTaught:
		if m.Text = strings.TrimSpace(body.Text); m.Text == "" {
			return badRequest(c, "text is required to mark a question taught")
		}
	default:
		return badRequest(c, "state must be dismissed, taught or null")
	}
	if err := a.Service.MarkQuestion(ctx(c), id, body.Key, m); err != nil {
		return questionError(c, err)
	}
	return c.NoContent(http.StatusNoContent)
}

// teachQuestion is POST /datapacks/:id/questions/teach {key, text}: a person saves what Meko should
// know about a question as a memory in the datapack (as memory_add does, with metadata {source:
// "meko", kind: "taught", question}) and marks it taught. Nothing checks the text against what Meko
// has: the person wrote it on purpose. It replies with the question.
func (a *api) teachQuestion(c echo.Context) error {
	id, cloud, ok, err := a.questionsDatapack(c)
	if !ok {
		return err
	}
	var body struct {
		Key  string `json:"key"`
		Text string `json:"text"`
	}
	if ok, berr := bind(c, &body); !ok {
		return berr
	}
	text := strings.TrimSpace(body.Text)
	switch {
	case body.Key == "":
		return badRequest(c, "key is required")
	case text == "":
		return badRequest(c, "text is required")
	case utf8.RuneCountInString(text) > memory.MaxTaughtChars:
		return badRequest(c, "text is longer than 2,000 characters")
	}
	q, err := a.Service.GetQuestion(ctx(c), id, body.Key)
	if err != nil {
		return questionError(c, err)
	}
	question := q.Query
	if r := []rune(question); len(r) > 300 {
		question = string(r[:300])
	}
	md, err := json.Marshal(map[string]any{"source": "meko", "kind": "taught", "question": question})
	if err != nil {
		return err
	}
	args := map[string]any{"text": text, "datapack_id": id, "agent_id": memory.WebUIAgent,
		"conversation_id": restConversation, "metadata": string(md)}
	var reply string
	switch {
	case cloud:
		if reply, err = a.Cloud.Tool(ctx(c), "memory_add", args); err != nil {
			return cloudError(c, err)
		}
	case a.Tool == nil:
		return envelope.Fail(c, http.StatusNotFound, "NOT_FOUND", "This Meko serves no tools here.")
	default:
		if reply, err = a.Tool(ctx(c), a.Credential(c), "memory_add", args); err != nil {
			return badRequest(c, err.Error())
		}
	}
	var added struct {
		Results []struct {
			ID string `json:"id"`
		} `json:"results"`
		Error  string `json:"error"`
		Detail string `json:"detail"`
	}
	if err = json.Unmarshal([]byte(reply), &added); err != nil || added.Error != "" {
		return toolReply(c, "memory_add", reply, cloud)
	}
	m := store.QuestionMark{State: memory.QuestionTaught, Text: text}
	if len(added.Results) > 0 {
		m.Memory = added.Results[0].ID
	}
	if err = a.Service.MarkQuestion(ctx(c), id, body.Key, m); err != nil {
		return questionError(c, err)
	}
	if q, err = a.Service.GetQuestion(ctx(c), id, body.Key); err != nil {
		return questionError(c, err)
	}
	return envelope.OK(c, questionRow(q))
}

// forgetAsks is DELETE /datapacks/:id/questions/asks?conversation_id=&before= (RFC 3339): one
// conversation's asks before a time are forgotten, such as an agent's searches while it filled a
// new datapack, which aren't questions to teach Meko. It replies {deleted}.
func (a *api) forgetAsks(c echo.Context) error {
	id, _, ok, err := a.questionsDatapack(c)
	if !ok {
		return err
	}
	conv := strings.TrimSpace(c.QueryParam("conversation_id"))
	if conv == "" {
		return badRequest(c, "conversation_id is required")
	}
	before := time.Now()
	if v := c.QueryParam("before"); v != "" {
		if before, err = time.Parse(time.RFC3339Nano, v); err != nil {
			return badRequest(c, "before must be a time (RFC 3339)")
		}
	}
	n, err := a.Service.ForgetAsks(ctx(c), id, conv, before)
	if err != nil {
		return serviceError(c, err)
	}
	return envelope.OK(c, map[string]any{"deleted": n})
}

func questionError(c echo.Context, err error) error {
	if errors.Is(err, memory.ErrNoQuestion) {
		return notFound(c, "question not found")
	}
	return serviceError(c, err)
}
