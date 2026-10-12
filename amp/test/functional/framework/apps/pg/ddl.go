package pg

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// The DDL gate. A fault injected while a workload is still building its schema
// fails the run for a table that never got created, not for anything the
// guarantee under test was asked about — and the failure looks identical to a
// real one, which is what makes it expensive.
//
// AwaitNoDDL is a point-in-time check. It passes before a workload starts its
// setup and in any gap between its statements, so a caller must first wait for
// the workload to report its setup complete, and only then call it.
//
// Scope note: this reports DDL running on the endpoint it is asked about. It is
// not a cluster-wide barrier, because there is nothing to ask: an amp branch has
// one writer, so the only session that can be mid-DDL on this branch is one this
// endpoint is serving.

// ddlVerbs are the statement heads that start a catalog change. Matched as the
// FIRST word of a normalized query, never as a substring — an INSERT whose
// payload contains the word "CREATE" is not DDL, and a gate that says it is
// never opens.
var ddlVerbs = []string{
	"CREATE", "ALTER", "DROP", "TRUNCATE", "REINDEX", "REFRESH", "COMMENT", "GRANT", "REVOKE",
}

// DefaultDDLPollInterval is how often AwaitNoDDL re-reads pg_stat_activity.
const DefaultDDLPollInterval = 2 * time.Second

// ActiveDDL returns the DDL statements running right now on the endpoint,
// excluding this connection's own query. Empty means no DDL is in flight.
//
// Only 'active' backends count: an 'idle in transaction' session holding a
// completed DDL is not still doing it, and waiting for it to disconnect would
// hang the gate on a connection pool that is simply warm.
func (c *Client) ActiveDDL(ctx context.Context) ([]string, error) {
	const q = `SELECT pid, query FROM pg_stat_activity
	           WHERE state = 'active' AND pid <> pg_backend_pid() AND query <> ''`
	var found []string
	err := c.Query(ctx, func(rows pgx.Rows) error {
		var pid int
		var query string
		if err := rows.Scan(&pid, &query); err != nil {
			return err
		}
		if IsDDL(query) {
			found = append(found, fmt.Sprintf("pid %d: %s", pid, firstLine(query)))
		}
		return nil
	}, q)
	if err != nil {
		return nil, fmt.Errorf("read pg_stat_activity: %w", err)
	}
	return found, nil
}

// IsDDL reports whether query's first keyword starts a catalog change.
// Exported so a caller can classify a statement it captured elsewhere.
func IsDDL(query string) bool {
	head := strings.ToUpper(firstWord(query))
	if head == "" {
		return false
	}
	for _, v := range ddlVerbs {
		if head == v {
			return true
		}
	}
	return false
}

// AwaitNoDDL blocks until no DDL is running on the endpoint, then returns. It
// does not know whether more DDL is coming; see the package note above.
// A timeout returns the statements still in flight, so the caller reports what
// it was waiting for rather than a bare deadline.
func (c *Client) AwaitNoDDL(ctx context.Context, timeout, poll time.Duration) error {
	if timeout <= 0 {
		timeout = 5 * time.Minute
	}
	if poll <= 0 {
		poll = DefaultDDLPollInterval
	}
	wctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	var last []string
	for {
		ddl, err := c.ActiveDDL(wctx)
		if err == nil {
			if len(ddl) == 0 {
				return nil
			}
			last = ddl
		}
		select {
		case <-wctx.Done():
			if len(last) == 0 {
				return fmt.Errorf("could not confirm DDL had finished within %s: %w", timeout, wctx.Err())
			}
			return fmt.Errorf("DDL still running after %s: %s", timeout, strings.Join(last, "; "))
		case <-time.After(poll):
		}
	}
}

// firstWord returns the first whitespace-delimited token, skipping leading
// whitespace, SQL line comments and a leading open paren.
func firstWord(s string) string {
	for _, line := range strings.Split(s, "\n") {
		l := strings.TrimSpace(line)
		if l == "" || strings.HasPrefix(l, "--") {
			continue
		}
		l = strings.TrimLeft(l, "(")
		if i := strings.IndexAny(l, " \t("); i >= 0 {
			return l[:i]
		}
		return l
	}
	return ""
}

// firstLine trims a query to one readable line for an error message.
func firstLine(s string) string {
	s = strings.TrimSpace(strings.ReplaceAll(s, "\n", " "))
	if len(s) > 160 {
		return s[:160] + "…"
	}
	return s
}
