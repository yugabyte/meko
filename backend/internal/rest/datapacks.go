package rest

import (
	"fmt"
	"sort"
	"strings"

	"github.com/labstack/echo/v4"

	"github.com/yugabyte/meko/backend/internal/envelope"
	"github.com/yugabyte/meko/backend/internal/memory"
	"github.com/yugabyte/meko/backend/internal/wire"
)

func (a *api) authConfig(c echo.Context) error {
	return envelope.OK(c, map[string]any{"provider": "none", "password_login": false, "signup": false,
		"social_providers": []string{}})
}

// fullName is the local user's name for attribution.
func fullName(p memory.Profile) string { return strings.TrimSpace(p.FirstName + " " + p.LastName) }

func (a *api) me(c echo.Context) error {
	p, err := a.Service.Profile(ctx(c))
	if err != nil {
		return err
	}
	return envelope.OK(c, map[string]any{"account_id": a.Service.UserID(), "email": p.Email,
		"first_name": p.FirstName, "last_name": p.LastName, "email_verified": true,
		"has_completed_onboarding": p.OnboardingDone, "has_email_password_login": false, "user_type": "pro"})
}

func (a *api) setOnboarding(c echo.Context) error {
	var body struct {
		Done *bool `json:"has_completed_onboarding"`
	}
	if ok, err := bind(c, &body); !ok {
		return err
	}
	if body.Done == nil {
		return badRequest(c, "has_completed_onboarding is required")
	}
	if err := a.Service.SetOnboarding(ctx(c), *body.Done); err != nil {
		return err
	}
	return envelope.OK(c, map[string]any{"message": "updated", "has_completed_onboarding": *body.Done})
}

func (a *api) setProfile(c echo.Context) error {
	var body struct {
		First string `json:"first_name"`
		Last  string `json:"last_name"`
	}
	if ok, err := bind(c, &body); !ok {
		return err
	}
	first, last := strings.TrimSpace(body.First), strings.TrimSpace(body.Last)
	if first == "" || last == "" || len(first) > 100 || len(last) > 100 || strings.ContainsAny(first+last, "<>") {
		return badRequest(c, "first_name and last_name are 1 to 100 characters, with no HTML")
	}
	if err := a.Service.SetName(ctx(c), first, last); err != nil {
		return err
	}
	return envelope.OK(c, map[string]string{"message": "profile updated"})
}

func (a *api) usage(c echo.Context) error {
	datapacks, conversations, err := a.Service.CountAll(ctx(c))
	if err != nil {
		return err
	}
	st, err := a.Service.CallStats(ctx(c), "")
	if err != nil {
		return err
	}
	return envelope.OK(c, map[string]int{"datapack_count": datapacks, "datapack_creations": datapacks,
		"kb_storage_file_size": 0, "num_conversations": conversations, "token_burn": st.InputTokens + st.OutputTokens,
		"num_memory_searches": st.Requests, "num_kb_searches": 0, "num_memory_adds": st.MemoryAdds})
}

// datapackRow is a datapack as GET /datapacks lists it: Cloud's row, its counts left at 0 (only
// describe counts), and the owner, the local user.
type datapackRow struct {
	wire.Datapack
	OwnerName                string `json:"datapack_owner_name,omitempty"`
	OwnerEmail               string `json:"datapack_owner_email_id,omitempty"`
	MemoryCount              int    `json:"memory_count"`
	KnowledgeCount           int    `json:"knowledge_count"`
	LearningsCount           int    `json:"learnings_count"`
	CollectiveMemoryCount    int    `json:"collective_memory_count"`
	ConversationSearchOptOut bool   `json:"conversation_search_opt_out"`
}

func (a *api) listDatapacks(c echo.Context) error {
	ds, err := a.Service.Datapacks(ctx(c))
	if err != nil {
		return err
	}
	p, err := a.Service.Profile(ctx(c))
	if err != nil {
		return err
	}
	sort.SliceStable(ds, func(i, j int) bool { return ds[i].CreatedAt.After(ds[j].CreatedAt) })
	out := make([]datapackRow, len(ds))
	for i, d := range ds {
		out[i] = datapackRow{Datapack: wire.DatapackRow(d, a.Service.UserID()), OwnerName: fullName(p),
			OwnerEmail: p.Email, ConversationSearchOptOut: d.ConversationSearchOptOut}
	}
	return envelope.OK(c, out)
}

func (a *api) createDatapack(c echo.Context) error {
	var body struct {
		Name        string `json:"name"`
		Description string `json:"description"`
	}
	if ok, err := bind(c, &body); !ok {
		return err
	}
	d, err := a.Service.CreateDatapack(ctx(c), body.Name, body.Description)
	if err != nil {
		return serviceError(c, err)
	}
	return created(c, map[string]any{"datapack_id": d.ID, "agent_id": memory.DefaultAgentID, "name": d.Name,
		"mcp_user_name": "", "mcp_server": map[string]string{"url": "", "api_key": ""},
		"db_access": map[string]string{"user_name": "", "password": "", "conn_str": ""}})
}

func (a *api) describeDatapack(c echo.Context) error {
	id, ok, err := a.pathDatapack(c)
	if !ok {
		return err
	}
	d, err := a.Service.Datapack(ctx(c), id)
	if err != nil {
		return serviceError(c, err)
	}
	counts, err := a.Service.CountDatapack(ctx(c), id)
	if err != nil {
		return serviceError(c, err)
	}
	p, err := a.Service.Profile(ctx(c))
	if err != nil {
		return err
	}
	return envelope.OK(c, struct {
		wire.DatapackDescription
		OwnerName  string `json:"datapack_owner_name,omitempty"`
		OwnerEmail string `json:"datapack_owner_email_id,omitempty"`
	}{wire.Describe(d, a.Service.UserID(), counts), fullName(p), p.Email})
}

func (a *api) updateDatapack(c echo.Context) error {
	id, ok, err := a.pathDatapack(c)
	if !ok {
		return err
	}
	var body struct {
		Name        *string `json:"name"`
		Description *string `json:"description"`
	}
	if ok, berr := bind(c, &body); !ok {
		return berr
	}
	if _, err = a.Service.UpdateDatapack(ctx(c), id, body.Name, body.Description, nil); err != nil {
		return serviceError(c, err)
	}
	return envelope.OK(c, map[string]string{"message": "datapack updated"})
}

func (a *api) deleteDatapack(c echo.Context) error {
	id, ok, err := a.pathDatapack(c)
	if !ok {
		return err
	}
	if err = a.Service.DeleteDatapack(ctx(c), id); err != nil {
		return serviceError(c, err)
	}
	return envelope.OK(c, map[string]string{"message": "datapack deleted"})
}

// provisionStatus is what the Connect page shows: the MCP URL and the key to send, the client
// token (per-client keys are #195).
func (a *api) provisionStatus(c echo.Context) error {
	id, ok, err := a.pathDatapack(c)
	if !ok {
		return err
	}
	d, err := a.Service.Datapack(ctx(c), id)
	if err != nil {
		return serviceError(c, err)
	}
	return envelope.OK(c, map[string]string{"datapack_id": d.ID, "datapack_name": d.Name,
		"mcp_server_url": fmt.Sprintf("http://127.0.0.1:%d/mcp", a.Port), "mcp_api_key": a.ClientToken,
		"db_user_name": "", "db_password": "", "db_conn_str": "", "deploy_env": "local"})
}

func (a *api) agents(c echo.Context) error {
	id, ok, err := a.pathDatapack(c)
	if !ok {
		return err
	}
	seen, err := a.Service.DatapackAgents(ctx(c), id)
	if err != nil {
		return serviceError(c, err)
	}
	out := make([]map[string]string, len(seen))
	for i, s := range seen {
		out[i] = map[string]string{"agent_id": s.AgentID, "user_id": a.Service.UserID(),
			"created_at": wire.Time(s.First), "updated_at": wire.Time(s.LastSeen)}
	}
	return envelope.OK(c, out)
}

// metrics is the datapack detail page's dashboard: its searches from the call log, and its sizes.
func (a *api) metrics(c echo.Context) error {
	id, ok, err := a.pathDatapack(c)
	if !ok {
		return err
	}
	st, err := a.Service.CallStats(ctx(c), id)
	if err != nil {
		return err
	}
	z, err := a.Service.SizeDatapack(ctx(c), id)
	if err != nil {
		return serviceError(c, err)
	}
	ifRaw := st.TokensSent + st.TokensSaved
	pct := 0.0
	if ifRaw > 0 {
		pct = 100 * float64(st.TokensSaved) / float64(ifRaw)
	}
	days := make([]map[string]any, len(st.Daily))
	for i, d := range st.Daily {
		days[i] = map[string]any{"date": d.Date, "hybrid": d.Hybrid, "kb": d.KB, "memory": d.Memory}
	}
	out := map[string]any{"datapack_id": id, "total_requests": st.Requests, "tokens_saved_total": st.TokensSaved,
		"tokens_if_raw_total": ifRaw, "savings_pct": pct, "score_high_total": st.ScoreHigh, "score_mid_total": st.Mid,
		"score_low_total": st.Low, "agent_count": st.Agents, "human_count": 1, "daily_activity": days,
		"conversation_count": z.Conversations, "memory_extraction_count": 0, "extraction_rate": 0.0,
		"kb_chunk_count": 0, "kb_file_count": 0, "memory_chunk_count": z.Memories,
		"conversation_chunk_count": z.ConversationChunks, "kb_bytes": 0, "memory_bytes": z.MemoryBytes,
		"conversation_bytes": z.TurnBytes}
	if st.LastAccessed != nil {
		out["last_accessed_at"] = wire.Time(*st.LastAccessed)
	}
	return envelope.OK(c, out)
}

func (a *api) tokenSavings(c echo.Context) error {
	id, ok, err := a.pathDatapack(c)
	if !ok {
		return err
	}
	st, err := a.Service.CallStats(ctx(c), id)
	if err != nil {
		return err
	}
	return envelope.OK(c, map[string]any{"total_requests": st.Requests, "total_tokens_saved": st.TokensSaved,
		"tokens_saved_by_source": st.SavedBySource, "by_datapack": []map[string]any{{"datapack_id": id,
			"total_requests": st.Requests, "total_tokens_saved": st.TokensSaved}}})
}

func (a *api) countConversations(c echo.Context) error {
	id, ok, err := a.pathDatapack(c)
	if !ok {
		return err
	}
	d, err := a.Service.Datapack(ctx(c), id)
	if err != nil {
		return serviceError(c, err)
	}
	n, err := a.Service.CountConversations(ctx(c), id)
	if err != nil {
		return serviceError(c, err)
	}
	return envelope.OK(c, map[string]any{"datapack_id": id, "datapack_name": d.Name, "count": n})
}

func (a *api) setOptOut(c echo.Context) error {
	id, ok, err := a.pathDatapack(c)
	if !ok {
		return err
	}
	var body struct {
		OptedOut *bool `json:"opted_out"`
	}
	if ok, berr := bind(c, &body); !ok {
		return berr
	}
	if body.OptedOut == nil {
		return badRequest(c, "opted_out is required")
	}
	if err = a.Service.SetConversationSearchOptOut(ctx(c), id, *body.OptedOut); err != nil {
		return serviceError(c, err)
	}
	return envelope.OK(c, map[string]bool{"conversation_search_opt_out": *body.OptedOut})
}
