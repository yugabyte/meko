// Package pgbench drives the pgbench binary REMOTELY over an
// exec.RemoteExecutor (the client-pod rule: pgbench never runs on the
// coordinator). It shells `pgbench -i` / `pgbench -c -j -T ...` on the target
// and parses tps/latency out of the captured output — no pgbench SDK exists,
// this is the same shelling-out approach kubectl and ssh use for their
// respective CLIs.
package pgbench

import (
	"context"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/yugabyte/amp/test/functional/framework/apps/pg"
	"github.com/yugabyte/amp/test/functional/framework/exec"
)

// missingBinaryHint names the artifact route to fix a missing pgbench binary,
// returned verbatim so callers/logs point at the fix instead of a bare exec
// error.
const missingBinaryHint = "pgbench not found on target; deploy it via the pgbench artifact " +
	"(artifact.Deploy(ctx, execs, artifacts[\"pgbench\"], opts)) before Init/Run"

// InitOptions parameterizes `pgbench -i`.
type InitOptions struct {
	Conn      pg.ConnInfo
	Scale     int // -s; 0 → pgbench's own default (1)
	ExtraArgs []string
}

// RunOptions parameterizes a `pgbench` load run.
type RunOptions struct {
	Conn      pg.ConnInfo
	Clients   int           // -c
	Jobs      int           // -j
	Duration  time.Duration // -T, rounded up to whole seconds
	ExtraArgs []string
}

// Result is the parsed outcome of a pgbench run. Raw always carries the full
// captured output, even when parsing found nothing (fields stay zero) or the
// run failed, so a caller can attach it for triage regardless.
type Result struct {
	TPS          float64
	LatencyAvgMs float64
	Raw          string
}

// Init runs `pgbench -i -s <scale> ...` on ex against opts.Conn.
func Init(ctx context.Context, ex exec.RemoteExecutor, opts InitOptions) error {
	if err := checkAvailable(ctx, ex); err != nil {
		return err
	}
	args := []string{"pgbench", "-i"}
	if opts.Scale > 0 {
		args = append(args, "-s", strconv.Itoa(opts.Scale))
	}
	args = append(args, opts.ExtraArgs...)
	args = append(args, connArgs(opts.Conn)...)

	res, err := ex.Run(ctx, quietTrace(envPrefix(opts.Conn)+strings.Join(args, " ")))
	if err != nil {
		return fmt.Errorf("pgbench init: %w", err)
	}
	if res.ExitCode != 0 {
		return fmt.Errorf("pgbench init exited %d: %s", res.ExitCode, strings.TrimSpace(res.Stderr))
	}
	return nil
}

// Run runs `pgbench -c <clients> -j <jobs> -T <seconds> ...` on ex and parses
// tps/latency out of the output.
func Run(ctx context.Context, ex exec.RemoteExecutor, opts RunOptions) (Result, error) {
	if err := checkAvailable(ctx, ex); err != nil {
		return Result{}, err
	}
	args := []string{"pgbench"}
	if opts.Clients > 0 {
		args = append(args, "-c", strconv.Itoa(opts.Clients))
	}
	if opts.Jobs > 0 {
		args = append(args, "-j", strconv.Itoa(opts.Jobs))
	}
	if opts.Duration > 0 {
		args = append(args, "-T", strconv.Itoa(int(opts.Duration.Round(time.Second)/time.Second)))
	}
	args = append(args, opts.ExtraArgs...)
	args = append(args, connArgs(opts.Conn)...)

	res, err := ex.Run(ctx, quietTrace(envPrefix(opts.Conn)+strings.Join(args, " ")))
	if err != nil {
		return Result{}, fmt.Errorf("pgbench run: %w", err)
	}
	out := res.Stdout + res.Stderr
	if res.ExitCode != 0 {
		return Result{Raw: out}, fmt.Errorf("pgbench run exited %d: %s", res.ExitCode, strings.TrimSpace(res.Stderr))
	}
	parsed := parseOutput(out)
	// No tps line means the SUMMARY never reached the capture — a broken exec
	// stream or an output flood evicting the tail. Returning it as a zero
	// poisoned a whole multitenant gate ("tenant 4 (pgbench) produced no
	// throughput") when the tenant had in fact been running fine; a missing
	// measurement is an error, not a measurement of zero.
	if !strings.Contains(out, "tps") {
		return Result{Raw: out}, fmt.Errorf("pgbench exited 0 but its summary is not in the captured output (%d bytes captured; the stream broke or the capture cap evicted it)", len(out))
	}
	return parsed, nil
}

// quietTrace keeps pgbench's summary safe from ysql_bench's per-statement
// client tracing, which it writes to stderr unasked ("ysql_bench: client 3
// sending BEGIN;", four lines per transaction) — megabytes a minute at a few
// hundred tps.
//
// A denylist alone was not enough: the trace tears into prefixless fragments
// that no line-anchored filter can drop, and 300 progress lines then compete
// with megabytes of debris for the capture. A 5-minute run charted 24 seconds.
// So the progress lines are extracted explicitly and emitted first.
//
// The streams are captured SEPARATELY. Merging them (`2>&1`) let 8 client
// threads tear each other's lines mid-write ("ysql_bench: ysql_bench: client…",
// prefixless fragments), which defeated any line-anchored filter, flooded the
// 10MiB capture cap, and evicted the stdout summary — the tps line — with it.
// Kept apart, stdout holds the clean summary in full; stderr is filtered for
// the trace prefix and then hard-bounded, so no flood shape (torn lines
// included) can ever crowd the summary out again. The -P progress lines ride
// stderr and survive the filter; a torn one is a lost point, not a lost run.
//
// Both go to files first so the filter cannot swallow pgbench's exit code, and
// real errors on stderr still come back inside the bounded tail.
func quietTrace(cmd string) string {
	return "out=$(mktemp); errf=$(mktemp); " + cmd + " >\"$out\" 2>\"$errf\"; rc=$?; " +
		"cat \"$out\"; " +
		// Progress lines FIRST and unbounded: they are the time series, they are
		// small (one per -P second), and they must not compete with the trace for
		// room. The rest of stderr follows as a bounded tail so real errors still
		// arrive without a flood being able to push the series out.
		"{ grep -aE '^progress: ' \"$errf\"; " +
		"grep -avE '^progress: |^ysql_bench: client [0-9]+ ' \"$errf\" | tail -c 65536; } >&2; " +
		"rm -f \"$out\" \"$errf\"; exit $rc"
}

// checkAvailable detects a missing pgbench binary on the target and returns
// an error naming the artifact-deploy route, rather than a bare "not found"
// exec failure downstream.
func checkAvailable(ctx context.Context, ex exec.RemoteExecutor) error {
	ok, err := exec.HasCommand(ctx, ex, "pgbench")
	if err != nil {
		return fmt.Errorf("check pgbench availability: %w", err)
	}
	if !ok {
		return fmt.Errorf("%s", missingBinaryHint)
	}
	return nil
}

// connArgs renders the -h/-p/-U/-d flags; the password travels via envPrefix.
// Host/user/database values are shell-quoted (arbitrary strings); the port is
// a plain decimal (always safe).
func connArgs(ci pg.ConnInfo) []string {
	var args []string
	if ci.Host != "" {
		args = append(args, "-h", shellQuote(ci.Host))
	}
	if ci.Port != 0 {
		args = append(args, "-p", strconv.Itoa(ci.Port))
	}
	if ci.User != "" {
		args = append(args, "-U", shellQuote(ci.User))
	}
	if ci.Database != "" {
		args = append(args, "-d", shellQuote(ci.Database))
	}
	return args
}

// envPrefix renders a leading `PGPASSWORD=... ` shell assignment when a
// password is set, so it never appears as a plain pgbench argument (visible
// in `ps`).
func envPrefix(ci pg.ConnInfo) string {
	if ci.Password == "" {
		return ""
	}
	return "PGPASSWORD=" + shellQuote(ci.Password) + " "
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// tpsExcludingRe / tpsIncludingRe / latencyRe are tolerant of both the
// two-line ("including"/"excluding" connection establishing) and single-line
// modern pgbench tps formats, and of the "latency average = N ms" line.
var (
	tpsExcludingRe = regexp.MustCompile(`tps = ([0-9.]+).*\(excluding`)
	tpsAnyRe       = regexp.MustCompile(`tps = ([0-9.]+)`)
	latencyRe      = regexp.MustCompile(`latency average = ([0-9.]+) ms`)
)

// parseOutput extracts tps (preferring the "excluding connection establishing"
// figure when both are present) and average latency from raw pgbench output.
// Missing fields are left zero rather than erroring — Raw always carries the
// full text for a caller/attachment to fall back on.
func parseOutput(raw string) Result {
	r := Result{Raw: raw}
	if m := tpsExcludingRe.FindStringSubmatch(raw); m != nil {
		r.TPS, _ = strconv.ParseFloat(m[1], 64)
	} else if m := tpsAnyRe.FindStringSubmatch(raw); m != nil {
		r.TPS, _ = strconv.ParseFloat(m[1], 64)
	}
	if m := latencyRe.FindStringSubmatch(raw); m != nil {
		r.LatencyAvgMs, _ = strconv.ParseFloat(m[1], 64)
	}
	return r
}
