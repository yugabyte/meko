package perf

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/yugabyte/amp/test/functional/framework/apps/pg"
	"github.com/yugabyte/amp/test/functional/framework/core"
	"github.com/yugabyte/amp/test/functional/framework/releases"
)

const (
	errClassExpected  = "expected"
	errClassTransport = "transport"
	errClassTimeout   = "timeout"
	errClassRelay     = "relay"
	errClassOther     = "other"
)

// A refused dial cannot be told apart from the harness's own port-forward gap, which kubectl tears down and re-establishes whenever the compute pod is replaced, so it is counted separately and first_success_after carries the outage.
var relayGapMarkers = []string{
	"connection refused",
	"dial error",
	"failed to connect to",
	"no such host",
}

type probeEvent struct {
	At       time.Time
	Client   string
	Stmt     string
	Err      error
	Class    string
	SQLState string
}

func (e probeEvent) ok() bool { return e.Err == nil }

func (e probeEvent) describe() string {
	state := e.SQLState
	if state == "" {
		state = "no SQLSTATE"
	}
	return fmt.Sprintf("%s %q at %s: %s (%s): %v",
		e.Client, e.Stmt, e.At.Format(time.RFC3339Nano), e.Class, state, e.Err)
}

var cwTransportMarkers = []string{
	"unexpected eof",
	"connection reset by peer",
	"broken pipe",
	"conn closed",
	"closed pool",
	"use of closed network connection",
	"server closed the connection unexpectedly",
}

func cwWrapTransport(err error) error {
	if err == nil {
		return nil
	}
	var te *pg.TransportError
	if errors.As(err, &te) {
		return err
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return err
	}
	low := strings.ToLower(err.Error())
	if strings.Contains(low, "connection refused") {
		return err
	}
	if hasAny(low, cwTransportMarkers) {
		return &pg.TransportError{Err: err}
	}
	return err
}

func classifyError(err error) (class, sqlstate string) {
	if err == nil {
		return "", ""
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		if c := sqlstateClass(pgErr.Code); c == "57" || c == "08" {
			return errClassExpected, pgErr.Code
		}
		return errClassOther, pgErr.Code
	}
	if core.IsConnDrop(err) {
		return errClassTransport, ""
	}
	if hasAny(strings.ToLower(err.Error()), relayGapMarkers) {
		return errClassRelay, ""
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return errClassTimeout, ""
	}
	return errClassOther, ""
}

func sqlstateClass(code string) string {
	if len(code) < 2 {
		return ""
	}
	return code[:2]
}

type pgxDriver struct {
	pool        *pgxpool.Pool
	period      time.Duration
	stmtTimeout time.Duration
	table       string
	stmtName    string
	stmtSQL     string
	logf        func(string, ...any)

	mu     sync.Mutex
	events []probeEvent

	pinMu   sync.Mutex
	pin     *pgxpool.Conn
	pinDead bool
}

const (
	clientPooled = "pgx_pool"
	clientPinned = "pgx_pinned"
)

func newPGXDriver(pool *pgxpool.Pool, table string, period, stmtTimeout time.Duration, logf func(string, ...any)) *pgxDriver {
	return &pgxDriver{
		pool: pool, period: period, stmtTimeout: stmtTimeout, table: table,
		stmtName: "cw_probe_stmt",
		stmtSQL:  fmt.Sprintf("INSERT INTO %s (client, at) VALUES ('pinned', now())", table),
		logf:     logf,
	}
}

func (d *pgxDriver) preparePinned(ctx context.Context) error {
	conn, err := d.pool.Acquire(ctx)
	if err != nil {
		return fmt.Errorf("pin a connection: %w", cwWrapTransport(err))
	}
	if _, err := conn.Conn().Prepare(ctx, d.stmtName, d.stmtSQL); err != nil {
		conn.Release()
		return fmt.Errorf("prepare %s: %w", d.stmtName, cwWrapTransport(err))
	}
	d.pinMu.Lock()
	old := d.pin
	d.pin, d.pinDead = conn, false
	d.pinMu.Unlock()
	if old != nil {
		old.Release()
	}
	return nil
}

func (d *pgxDriver) releasePinned() {
	d.pinMu.Lock()
	conn := d.pin
	d.pin = nil
	d.pinMu.Unlock()
	if conn != nil {
		conn.Release()
	}
}

func (d *pgxDriver) run(ctx context.Context) {
	tick := time.NewTicker(d.period)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			d.probePool(ctx)
			d.probePinned(ctx)
		}
	}
}

func (d *pgxDriver) probePool(ctx context.Context) {
	sql := fmt.Sprintf("INSERT INTO %s (client, at) VALUES ('pool', now())", d.table)
	sctx, cancel := context.WithTimeout(ctx, d.stmtTimeout)
	_, err := d.pool.Exec(sctx, sql)
	cancel()
	d.record(ctx, clientPooled, sql, cwWrapTransport(err))
}

// pinMu is held across the whole statement, not just the pointer read: a pgx connection is not safe for concurrent use and checkPrepared touches this same one ("conn busy", run fa24d972).
func (d *pgxDriver) probePinned(ctx context.Context) {
	d.pinMu.Lock()
	defer d.pinMu.Unlock()
	if d.pin == nil || d.pinDead {
		return
	}
	err := d.execPinnedLocked(ctx)
	d.record(ctx, clientPinned, d.stmtName, err)
}

func (d *pgxDriver) execPinnedLocked(ctx context.Context) error {
	sctx, cancel := context.WithTimeout(ctx, d.stmtTimeout)
	defer cancel()
	_, err := d.pin.Exec(sctx, d.stmtName)
	if err != nil {
		d.pinDead = true
	}
	return cwWrapTransport(err)
}

func (d *pgxDriver) record(ctx context.Context, client, stmt string, err error) {
	if err != nil && ctx.Err() != nil && errors.Is(err, context.Canceled) {
		return
	}
	ev := probeEvent{At: time.Now(), Client: client, Stmt: stmt, Err: err}
	if err != nil {
		ev.Class, ev.SQLState = classifyError(err)
	}
	d.mu.Lock()
	d.events = append(d.events, ev)
	d.mu.Unlock()
}

func (d *pgxDriver) snapshot() []probeEvent {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := make([]probeEvent, len(d.events))
	copy(out, d.events)
	sort.Slice(out, func(i, j int) bool { return out[i].At.Before(out[j].At) })
	return out
}

type preparedStatementOutcome string

const (
	preparedSurvived   preparedStatementOutcome = "survived"
	preparedReprepared preparedStatementOutcome = "reprepared_after_26000"
	preparedReconnect  preparedStatementOutcome = "session_replaced"
)

func (d *pgxDriver) checkPrepared(ctx context.Context) (preparedStatementOutcome, error) {
	outcome, err := d.checkPreparedOnSession(ctx)
	if err != nil || outcome != "" {
		return outcome, err
	}
	if err := d.preparePinned(ctx); err != nil {
		return preparedReconnect, fmt.Errorf("the pinned session could not be re-established: %w", err)
	}
	return preparedReconnect, nil
}

func (d *pgxDriver) checkPreparedOnSession(ctx context.Context) (preparedStatementOutcome, error) {
	d.pinMu.Lock()
	defer d.pinMu.Unlock()
	if d.pin == nil || d.pinDead {
		return "", nil
	}
	err := d.execPinnedLocked(ctx)
	if err == nil {
		return preparedSurvived, nil
	}
	class, state := classifyError(err)
	if state == "26000" {
		if _, perr := d.pin.Conn().Prepare(ctx, d.stmtName, d.stmtSQL); perr != nil {
			return preparedReprepared, fmt.Errorf("the server forgot %s (26000) and re-preparing it failed: %w", d.stmtName, cwWrapTransport(perr))
		}
		d.pinDead = false
		return preparedReprepared, nil
	}
	if class == errClassOther {
		return "", fmt.Errorf("the prepared statement failed with an unlicensed error (%s %s): %w", class, state, err)
	}
	return "", nil
}

type idleTx struct {
	conn *pgxpool.Conn
	mark string
}

func beginIdleTx(ctx context.Context, pool *pgxpool.Pool, table, mark string) (*idleTx, error) {
	conn, err := pool.Acquire(ctx)
	if err != nil {
		return nil, fmt.Errorf("acquire a connection for the idle transaction: %w", cwWrapTransport(err))
	}
	if _, err := conn.Exec(ctx, "BEGIN"); err != nil {
		conn.Release()
		return nil, fmt.Errorf("begin the idle transaction: %w", cwWrapTransport(err))
	}
	if _, err := conn.Exec(ctx, fmt.Sprintf("INSERT INTO %s (client, at, mark) VALUES ('idle_tx', now(), $1)", table), mark); err != nil {
		conn.Release()
		return nil, fmt.Errorf("write the idle transaction's row: %w", cwWrapTransport(err))
	}
	return &idleTx{conn: conn, mark: mark}, nil
}

type idleTxOutcome struct {
	Committed bool
	Class     string
	SQLState  string
	Err       error
}

func (o idleTxOutcome) answered() bool {
	return !o.Committed && o.SQLState != ""
}

func (x *idleTx) commit(ctx context.Context, timeout time.Duration) idleTxOutcome {
	defer x.conn.Release()
	sctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	if _, err := x.conn.Exec(sctx, "COMMIT"); err != nil {
		err = cwWrapTransport(err)
		class, state := classifyError(err)
		return idleTxOutcome{Class: class, SQLState: state, Err: err}
	}
	return idleTxOutcome{Committed: true}
}

// A COMMIT whose reply never arrived leaves the outcome genuinely unknown, so only a refusal the server actually answered with contradicts the row being there.
func verifyIdleTxRow(ctx context.Context, conn *pg.Client, table string, out idleTxOutcome, mark string) error {
	n, err := conn.QueryInt(ctx, fmt.Sprintf("SELECT count(*) FROM %s WHERE mark = $1", table), mark)
	if err != nil {
		return fmt.Errorf("read back the idle transaction's row: %w", err)
	}
	switch {
	case out.Committed && n != 1:
		return fmt.Errorf("the idle transaction committed but its row %q is not there (%d rows): a commit was acknowledged and lost", mark, n)
	case out.answered() && n != 0:
		return fmt.Errorf("the idle transaction's commit was refused (%s %s: %v) but its row %q is there (%d rows): a partial commit",
			out.Class, out.SQLState, out.Err, mark, n)
	}
	return nil
}

var javaExceptionRe = regexp.MustCompile(`(?i)(SQLException|SQLState|PSQLException|Connection refused|Connection reset|EOF|terminating connection|server closed the connection)`)

var javaSQLStateRe = regexp.MustCompile(`(?i)sqlstate[:= ]+\(?([0-9a-z]{5})\)?`)

var javaTransportMarkers = []string{
	"eof",
	"connection reset",
	"broken pipe",
	"server closed the connection",
	"connection refused",
	"socket closed",
}

type javaError struct {
	At       time.Time
	Line     string
	Class    string
	SQLState string
}

func parseJavaErrors(log string, offset time.Duration) []javaError {
	var out []javaError
	for _, line := range strings.Split(log, "\n") {
		if !javaExceptionRe.MatchString(line) {
			continue
		}
		e := javaError{Line: strings.TrimSpace(line), Class: errClassOther}
		if m := javaSQLStateRe.FindStringSubmatch(line); m != nil {
			e.SQLState = strings.ToUpper(m[1])
			if c := sqlstateClass(e.SQLState); c == "57" || c == "08" {
				e.Class = errClassExpected
			}
		} else if hasAny(strings.ToLower(line), javaTransportMarkers) {
			e.Class = errClassTransport
		}
		if stamp, ok := sampleAppLineTime(line); ok {
			e.At = stamp.Add(offset)
		}
		out = append(out, e)
	}
	return out
}

func hasAny(s string, markers []string) bool {
	for _, m := range markers {
		if strings.Contains(s, m) {
			return true
		}
	}
	return false
}

// The last stamped line, not the first: it is within a progress interval of the read, while the first predates the readiness wait by however long the JVM took.
func javaLogAnchor(log string) (time.Time, bool) {
	lines := strings.Split(log, "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if stamp, ok := sampleAppLineTime(lines[i]); ok {
			return stamp, true
		}
	}
	return time.Time{}, false
}

func cwPreviousRelease(list []releases.Release, tag string) (releases.Release, error) {
	version, _, isRelease := releases.ParseTag(tag)
	for _, r := range list {
		if r.Tag == tag {
			continue
		}
		if isRelease && releases.Compare(r.Version, version) >= 0 {
			continue
		}
		return r, nil
	}
	return releases.Release{}, fmt.Errorf("the report portal lists no release older than %s to roll onto", tag)
}
