package repository

import (
	"database/sql"
	"fmt"

	"github.com/lib/pq"

	"meko/api_server/internal/memtable"
)

// DatapackRollupRepo answers "how much is in these datapacks, and who put it
// there?" for a whole set of datapacks in one go.
//
// Every table it reads is multi-tenant and keyed on the datapack id, so
// `= ANY($1) GROUP BY` covers a project of any size with one query per table
// and no sequential scan. That is what keeps the project page a fixed number
// of round trips instead of one per datapack, which is how
// DatapackHandler.Describe does it — see MEKO-653.
//
// The queries are not equally cheap, and EXPLAIN says why:
//
//   - promoted_collection is unpartitioned: one index scan.
//   - knowledge_bases and conversation_history_search are
//     PARTITION BY HASH (tenant_id) and tenant_id IS the datapack, so the
//     planner prunes to one partition per datapack.
//   - The memory table is memtable.Table(). The default,
//     meko_mem0_collection, is PARTITION BY HASH (meko_datapack_id,
//     meko_user_id, meko_agent_id): constraining only the datapack cannot
//     prune, so it index-scans all 128 partitions, and grouping here pays
//     that fan-out once for the project rather than once per datapack.
//     meko_mem0_memories is PARTITION BY HASH (meko_datapack_id), so it
//     prunes to one partition per datapack and grouping only saves round
//     trips.
//
// Reads the DATA database, not meko_system.
type DatapackRollupRepo struct {
	dataDB *sql.DB
}

func NewDatapackRollupRepo(dataDB *sql.DB) *DatapackRollupRepo {
	return &DatapackRollupRepo{dataDB: dataDB}
}

// DatapackRollup is what one datapack holds. Zero values are correct for a
// datapack with no rows in a given table, so a missing map entry and an empty
// rollup mean the same thing.
type DatapackRollup struct {
	MemoryCount int `json:"memory_count"`
	// AwaitingReview counts memories with no promotion decision yet
	// (meko_promotion_user_action IS NULL). A subset of MemoryCount, and the
	// same definition DatapackHandler.Describe uses for learnings_count.
	AwaitingReview int `json:"awaiting_review_count"`
	// PromotedCount is memories published to the project's shared knowledge.
	PromotedCount  int `json:"promoted_count"`
	KnowledgeDocs  int `json:"knowledge_doc_count"`
	KnowledgeChunk int `json:"knowledge_chunk_count"`
}

// ProjectRollups is every datapack's contents plus one project-wide figure
// that cannot be got by summing them.
type ProjectRollups struct {
	ByDatapack map[string]DatapackRollup
	// ActiveAgents is how many distinct agents have actually written into
	// these datapacks.
	//
	// Deliberately NOT read from meko_system.agent_access. That table only
	// ever holds the default 'meko_agent', one row per user, because an agent
	// is linked there by datapack create, share accept and an explicit
	// POST /agents, and never by writing. The agents that do the work
	// ("WorkbenchLM", "claude_code:billing") appear only on the rows they
	// wrote, so counting access rows would just restate the user count.
	//
	// Counted as (user, agent_id) pairs for the same reason
	// ProjectRepo.PeopleWithAccess does: an agent_id is unique only within a
	// user, and in practice everyone's default is the identical string
	// 'meko_agent', so keying on the id alone would collapse two people's
	// agents into one in exactly the shared project where the number matters.
	//
	// It cannot be summed per datapack either: one agent writing in three
	// datapacks is one agent, so the distinct set is unioned here.
	ActiveAgents int
}

// Rollups returns what each datapack holds and how many agents wrote it.
// FOUR queries whatever the number of datapacks.
//
// Failures are returned rather than swallowed: reporting zeros as if they
// were counts is worse than letting the caller decide to degrade.
func (r *DatapackRollupRepo) Rollups(datapackIDs []string) (ProjectRollups, error) {
	out := ProjectRollups{ByDatapack: make(map[string]DatapackRollup, len(datapackIDs))}
	if len(datapackIDs) == 0 {
		return out, nil
	}
	ids := pq.Array(datapackIDs)
	agents := map[string]bool{}

	// Memories. Grouped by (datapack, user, agent) rather than datapack alone:
	// the counts still add up per datapack in Go, and the same single scan
	// yields the agent set, so attribution costs nothing on top of the
	// 128-partition fan-out this query already pays. Asking separately would
	// pay it twice. All three are partition-key columns, so the extra grouping
	// is free.
	if err := r.eachMemoryGroup(ids, func(id, userID, agent string, total, awaiting int) {
		row := out.ByDatapack[id]
		row.MemoryCount += total
		row.AwaitingReview += awaiting
		out.ByDatapack[id] = row
		if agent != "" {
			agents[agentKey(userID, agent)] = true
		}
	}); err != nil {
		return out, fmt.Errorf("rollups: memories: %w", err)
	}

	if err := r.scanInto(out.ByDatapack, ids,
		`SELECT meko_datapack_id::text, COUNT(*), 0
		 FROM   datapacks.promoted_collection
		 WHERE  meko_datapack_id = ANY($1)
		 GROUP  BY meko_datapack_id`,
		func(row *DatapackRollup, a, _ int) { row.PromotedCount = a },
	); err != nil {
		return out, fmt.Errorf("rollups: promoted: %w", err)
	}

	// Files and chunks off the same scan. Uploaded files and promoted memories
	// share this table and are told apart by metadata_filters->>'source'.
	//
	// The predicate is "not 'memory'", NOT "= 'doc'" as
	// KnowledgeBaseRepo.Counts writes it. Checked against real rows: 'memory'
	// is written, 'doc' is written nowhere and source is simply NULL on
	// uploads, so `= 'doc'` reports zero files for a datapack that plainly has
	// some. Excluding the one marker that IS written counts uploads whether or
	// not they are tagged, and keeps working if tagging starts.
	if err := r.scanInto(out.ByDatapack, ids,
		`SELECT tenant_id::text,
		        COUNT(DISTINCT document_id)
		          FILTER (WHERE metadata_filters->>'source' IS DISTINCT FROM 'memory'),
		        COUNT(*)
		 FROM   datapacks.knowledge_bases
		 WHERE  tenant_id = ANY($1)
		 GROUP  BY tenant_id`,
		func(row *DatapackRollup, a, b int) { row.KnowledgeDocs, row.KnowledgeChunk = a, b },
	); err != nil {
		return out, fmt.Errorf("rollups: knowledge: %w", err)
	}

	// Conversations contribute agents but no counts. An agent can hold a
	// hundred conversations and have extracted no memories yet, which is the
	// common case early on, so leaving this out would miss the busiest agent
	// in a project entirely.
	if err := r.eachConversationAgent(ids, func(userID, agent string) {
		agents[agentKey(userID, agent)] = true
	}); err != nil {
		return out, fmt.Errorf("rollups: conversation agents: %w", err)
	}

	out.ActiveAgents = len(agents)
	return out, nil
}

// agentKey identifies one agent instance. See ProjectRollups.ActiveAgents for
// why the agent_id on its own is not enough.
func agentKey(userID, agentID string) string {
	return userID + "\x00" + agentID
}

func (r *DatapackRollupRepo) eachMemoryGroup(
	ids any,
	fn func(datapackID, userID, agentID string, total, awaiting int),
) error {
	rows, err := r.dataDB.Query(
		`SELECT meko_datapack_id::text,
		        COALESCE(meko_user_id::text, ''),
		        COALESCE(meko_agent_id, ''),
		        COUNT(*),
		        COUNT(*) FILTER (WHERE meko_promotion_user_action IS NULL)
		 FROM   `+memtable.Table()+`
		 WHERE  meko_datapack_id = ANY($1)
		 GROUP  BY meko_datapack_id, meko_user_id, meko_agent_id`,
		ids,
	)
	if err != nil {
		return err
	}
	defer rows.Close()

	for rows.Next() {
		var id, userID, agent string
		var total, awaiting int
		if sErr := rows.Scan(&id, &userID, &agent, &total, &awaiting); sErr != nil {
			return sErr
		}
		fn(id, userID, agent, total, awaiting)
	}
	return rows.Err()
}

func (r *DatapackRollupRepo) eachConversationAgent(ids any, fn func(userID, agentID string)) error {
	rows, err := r.dataDB.Query(
		`SELECT DISTINCT COALESCE(user_id::text, ''), agent_id
		 FROM   datapacks.conversation_history_search
		 WHERE  tenant_id = ANY($1)`,
		ids,
	)
	if err != nil {
		return err
	}
	defer rows.Close()

	for rows.Next() {
		var userID string
		var agent sql.NullString
		if sErr := rows.Scan(&userID, &agent); sErr != nil {
			return sErr
		}
		if agent.Valid && agent.String != "" {
			fn(userID, agent.String)
		}
	}
	return rows.Err()
}

// scanInto runs a (id, int, int) query and folds each row into the map via
// assign. Two of the rollup queries differ only in their SQL and which fields
// they fill, so the scan loop lives here once.
func (r *DatapackRollupRepo) scanInto(
	out map[string]DatapackRollup,
	ids any,
	query string,
	assign func(row *DatapackRollup, a, b int),
) error {
	rows, err := r.dataDB.Query(query, ids)
	if err != nil {
		return err
	}
	defer rows.Close()

	for rows.Next() {
		var id string
		var a, b int
		if sErr := rows.Scan(&id, &a, &b); sErr != nil {
			return sErr
		}
		row := out[id]
		assign(&row, a, b)
		out[id] = row
	}
	return rows.Err()
}
