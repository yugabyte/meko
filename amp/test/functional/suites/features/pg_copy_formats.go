// COPY in both directions, in the formats a user actually types. pg_copy covers
// the pgx CopyFrom fast path (binary, table-to-table); this covers the SQL
// COPY … TO STDOUT / COPY … FROM STDIN protocol, its text and CSV encodings and
// their options — the shape `psql \copy`, pg_dump and every bulk-load script
// use. It also covers the failure a bulk load must have: a malformed row aborts
// the whole COPY and leaves the table exactly as it was.
package features

import (
	"bytes"
	"fmt"
	"strings"

	"github.com/yugabyte/amp/test/functional/framework/apps/pg"
	"github.com/yugabyte/amp/test/functional/framework/core"
)

// copyFmtRows is small on purpose: this test is about encodings and options, and
// every row is compared byte-for-byte against a Go-built expectation.
const copyFmtRows = 300

func init() {
	core.Register(core.Test{
		Name:        "features/pg_copy_formats",
		Suite:       "features",
		Category:    core.Functional,
		Env:         core.EnvSpec{Sharing: core.Shared, Shape: core.SingleContainer},
		Tags:        []string{"regression"},
		OwningTeam:  core.TeamCompute,
		Description: "COPY TO STDOUT and COPY FROM STDIN over text and CSV (header, custom delimiter, NULL marker): each export is compared to Go-built bytes, re-imported into a fresh table and verified, and a malformed CSV row must abort its COPY leaving the table unchanged.",
		Run:         runCopyFormats,
	})
}

// copyRow is one seeded row, including the two values every COPY encoding has
// an opinion about: a NULL, and a string carrying the delimiter and a quote.
type copyRow struct {
	ID    int
	Label string
	Note  *string
}

func runCopyFormats(t *core.T) error {
	env, ok := t.Env.(pgEnv)
	if !ok {
		t.Skip("requires an env that exposes a PGEndpoint (sharedk8s or universe)")
		return nil
	}
	client, err := connect(t, env)
	if err != nil {
		return err
	}
	defer client.Close()

	src := "cpf_src_" + suffix(t)
	dst := "cpf_dst_" + suffix(t)
	defer func() {
		_, _ = client.Exec(t, "DROP TABLE IF EXISTS "+src)
		_, _ = client.Exec(t, "DROP TABLE IF EXISTS "+dst)
	}()

	rows := buildCopyRows(t)

	if err := t.Step("seed_source", func(s *core.StepHandle) error {
		for _, table := range []string{src, dst} {
			if _, err := client.Exec(t, fmt.Sprintf(
				"CREATE TABLE %s (id int PRIMARY KEY, label text NOT NULL, note text)", table)); err != nil {
				return err
			}
		}
		for _, r := range rows {
			if _, err := client.Exec(t,
				fmt.Sprintf("INSERT INTO %s (id, label, note) VALUES ($1, $2, $3)", src),
				r.ID, r.Label, r.Note); err != nil {
				return err
			}
		}
		s.Set("rows", len(rows))
		return nil
	}); err != nil {
		return err
	}

	// Each case exports with COPY … TO, compares the bytes to what Postgres's
	// documented encoding says they must be, then re-imports into an empty table
	// with the matching COPY … FROM and re-reads the rows.
	cases := []struct {
		name    string
		options string
		want    func([]copyRow) string
	}{
		{"text", "", encodeText},
		{"csv", "WITH (FORMAT csv)", func(rs []copyRow) string { return encodeCSV(rs, ',', "", false) }},
		{"csv_header", "WITH (FORMAT csv, HEADER true)", func(rs []copyRow) string { return encodeCSV(rs, ',', "", true) }},
		{"csv_delimiter_pipe", "WITH (FORMAT csv, DELIMITER '|')", func(rs []copyRow) string { return encodeCSV(rs, '|', "", false) }},
		{"csv_null_marker", "WITH (FORMAT csv, NULL 'NADA')", func(rs []copyRow) string { return encodeCSV(rs, ',', "NADA", false) }},
	}

	for _, c := range cases {
		if err := t.Step("roundtrip_"+c.name, func(s *core.StepHandle) error {
			out, err := copyOut(t, client, fmt.Sprintf("COPY (SELECT id, label, note FROM %s ORDER BY id) TO STDOUT %s", src, c.options))
			if err != nil {
				return err
			}
			want := c.want(rows)
			if out != want {
				return fmt.Errorf("COPY TO %s produced %d bytes that differ from the expected encoding\n first difference at: %s",
					c.name, len(out), firstDiff(out, want))
			}
			s.Set("bytes", len(out))

			if _, err := client.Exec(t, "TRUNCATE "+dst); err != nil {
				return err
			}
			if err := copyIn(t, client, fmt.Sprintf("COPY %s (id, label, note) FROM STDIN %s", dst, c.options), out); err != nil {
				return err
			}
			return verifyCopyTable(t, client, dst, rows)
		}); err != nil {
			return err
		}
	}

	return t.Step("malformed_input_aborts", func(s *core.StepHandle) error {
		if _, err := client.Exec(t, "TRUNCATE "+dst); err != nil {
			return err
		}
		good := encodeCSV(rows[:5], ',', "", false)
		// Third column count is wrong on the last line: COPY must reject the whole
		// batch, not the offending line, or a half-loaded table looks successful.
		broken := good + "9999,too,many,columns\n"
		err := copyIn(t, client, fmt.Sprintf("COPY %s (id, label, note) FROM STDIN WITH (FORMAT csv)", dst), broken)
		if err == nil {
			return fmt.Errorf("COPY FROM accepted a row with too many columns")
		}
		left, qerr := client.QueryInt(t, "SELECT count(*) FROM "+dst)
		if qerr != nil {
			return qerr
		}
		if left != 0 {
			return fmt.Errorf("%d row(s) survived a COPY that failed mid-stream: COPY must be all-or-nothing", left)
		}
		s.Set("rejected", err.Error())
		return nil
	})
}

// buildCopyRows seeds rows including the values every encoding treats specially:
// a NULL, an embedded comma and pipe, a double quote, and a backslash.
func buildCopyRows(t *core.T) []copyRow {
	r := t.Seed.Fork("copyfmt").Rand()
	out := make([]copyRow, 0, copyFmtRows)
	for id := 0; id < copyFmtRows; id++ {
		label := fmt.Sprintf("label-%d-%x", id, r.Uint64()>>40)
		switch id {
		case 0:
			label = `comma,and|pipe`
		case 1:
			label = `quote"inside`
		case 2:
			label = `back\slash`
		case 3:
			label = "tab\tinside"
		}
		var note *string
		if id%3 != 0 {
			n := fmt.Sprintf("note-%d", id)
			note = &n
		}
		out = append(out, copyRow{ID: id, Label: label, Note: note})
	}
	return out
}

// encodeText renders rows the way COPY's default text format does: tab
// separated, \N for NULL, and backslash escapes for tab, newline and backslash.
func encodeText(rows []copyRow) string {
	var b strings.Builder
	esc := func(s string) string {
		s = strings.ReplaceAll(s, `\`, `\\`)
		s = strings.ReplaceAll(s, "\t", `\t`)
		s = strings.ReplaceAll(s, "\n", `\n`)
		s = strings.ReplaceAll(s, "\r", `\r`)
		return s
	}
	for _, r := range rows {
		note := `\N`
		if r.Note != nil {
			note = esc(*r.Note)
		}
		fmt.Fprintf(&b, "%d\t%s\t%s\n", r.ID, esc(r.Label), note)
	}
	return b.String()
}

// encodeCSV renders rows the way COPY's CSV format does: fields quoted only when
// they must be (delimiter, quote, newline or carriage return present), quotes
// doubled inside, NULL written as nullMarker (empty = unquoted nothing).
func encodeCSV(rows []copyRow, delim rune, nullMarker string, header bool) string {
	var b strings.Builder
	quote := func(s string) string {
		if !strings.ContainsRune(s, delim) && !strings.ContainsAny(s, "\"\n\r") {
			return s
		}
		return `"` + strings.ReplaceAll(s, `"`, `""`) + `"`
	}
	if header {
		fmt.Fprintf(&b, "id%clabel%cnote\n", delim, delim)
	}
	for _, r := range rows {
		// A NULL is the marker itself, never quoted — quoting would make it the
		// literal string instead. The default marker is the empty field.
		note := nullMarker
		if r.Note != nil {
			note = quote(*r.Note)
		}
		fmt.Fprintf(&b, "%d%c%s%c%s\n", r.ID, delim, quote(r.Label), delim, note)
	}
	return b.String()
}

// copyOut runs a COPY … TO STDOUT and returns the stream. It needs the raw
// connection: COPY is a protocol mode, not a query with rows.
func copyOut(t *core.T, client *pg.Client, sql string) (string, error) {
	conn, err := client.Pool().Acquire(t)
	if err != nil {
		return "", err
	}
	defer conn.Release()
	var buf bytes.Buffer
	if _, err := conn.Conn().PgConn().CopyTo(t, &buf, sql); err != nil {
		return "", fmt.Errorf("copy to stdout (%s): %w", truncateSQL(sql), err)
	}
	return buf.String(), nil
}

// copyIn feeds a COPY … FROM STDIN from memory.
func copyIn(t *core.T, client *pg.Client, sql, data string) error {
	conn, err := client.Pool().Acquire(t)
	if err != nil {
		return err
	}
	defer conn.Release()
	if _, err := conn.Conn().PgConn().CopyFrom(t, strings.NewReader(data), sql); err != nil {
		return fmt.Errorf("copy from stdin (%s): %w", truncateSQL(sql), err)
	}
	return nil
}

// verifyCopyTable reads every row back and compares it to the seed, so a lost
// NULL or a mangled quote fails with the row that broke.
func verifyCopyTable(t *core.T, client *pg.Client, table string, want []copyRow) error {
	count, err := client.QueryInt(t, "SELECT count(*) FROM "+table)
	if err != nil {
		return err
	}
	if int(count) != len(want) {
		return fmt.Errorf("%s holds %d rows after the re-import, want %d", table, count, len(want))
	}
	for _, r := range want {
		label, err := client.QueryString(t, fmt.Sprintf("SELECT label FROM %s WHERE id = $1", table), r.ID)
		if err != nil {
			return err
		}
		if label != r.Label {
			return fmt.Errorf("row %d came back with label %q, want %q", r.ID, label, r.Label)
		}
		nulls, err := client.QueryInt(t, fmt.Sprintf("SELECT count(*) FROM %s WHERE id = $1 AND note IS NULL", table), r.ID)
		if err != nil {
			return err
		}
		if (r.Note == nil) != (nulls == 1) {
			return fmt.Errorf("row %d note nullness did not survive the round trip (want NULL: %v)", r.ID, r.Note == nil)
		}
		if r.Note != nil {
			note, err := client.QueryString(t, fmt.Sprintf("SELECT note FROM %s WHERE id = $1", table), r.ID)
			if err != nil {
				return err
			}
			if note != *r.Note {
				return fmt.Errorf("row %d came back with note %q, want %q", r.ID, note, *r.Note)
			}
		}
	}
	return nil
}

// firstDiff names where two encodings diverge, so a failure points at the byte
// instead of dumping two streams.
func firstDiff(got, want string) string {
	n := min(len(got), len(want))
	for i := 0; i < n; i++ {
		if got[i] != want[i] {
			return fmt.Sprintf("offset %d: got %q, want %q", i, snippet(got, i), snippet(want, i))
		}
	}
	if len(got) != len(want) {
		return fmt.Sprintf("offset %d: lengths differ (got %d bytes, want %d)", n, len(got), len(want))
	}
	return "no difference"
}

func snippet(s string, at int) string {
	end := min(at+24, len(s))
	return s[at:end]
}

func truncateSQL(sql string) string {
	if len(sql) <= 80 {
		return sql
	}
	return sql[:80] + "…"
}
