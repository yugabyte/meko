// Package admin holds the additive, admin-only counters that back the
// meko_admin metrics dashboard. They live in their own repo/package so
// the shared user-facing repositories stay untouched. AdminRepo spans
// the control plane (systemDB), the data plane (dataDB), and the
// optional Langfuse DB.
package admin

import (
	"database/sql"
	"strconv"
	"time"
)

// AdminRepo aggregates admin-dashboard counters across the three data
// stores. systemDB serves meko_system.* registries; dataDB serves the
// datapacks.* / dist_rag.* counters; langfuseDB is optional and may be
// nil (Langfuse-derived metrics are then skipped).
type AdminRepo struct {
	systemDB   *sql.DB
	dataDB     *sql.DB
	langfuseDB *sql.DB
}

// NewAdminRepo wires the three DB handles the admin metrics need.
func NewAdminRepo(systemDB, dataDB, langfuseDB *sql.DB) *AdminRepo {
	return &AdminRepo{systemDB: systemDB, dataDB: dataDB, langfuseDB: langfuseDB}
}

// LangfuseConfigured reports whether a Langfuse DB handle is present.
func (r *AdminRepo) LangfuseConfigured() bool {
	return r != nil && r.langfuseDB != nil
}

// rangePredicate builds an optional inclusive-lower / exclusive-upper time
// filter for admin-metrics counts. `col` is the timestamp column, `next` the
// next positional placeholder index. Returns the SQL fragment (empty when both
// bounds are nil) and the args to append, so the no-range path is unchanged.
func rangePredicate(col string, start, endExclusive *time.Time, next int) (string, []any) {
	var sql string
	var args []any
	if start != nil {
		sql += " AND " + col + " >= $" + strconv.Itoa(next)
		args = append(args, *start)
		next++
	}
	if endExclusive != nil {
		sql += " AND " + col + " < $" + strconv.Itoa(next)
		args = append(args, *endExclusive)
	}
	return sql, args
}
