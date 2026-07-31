// Package sysbench drives the sysbench binary REMOTELY over an
// exec.RemoteExecutor (the client-pod rule: sysbench never runs on the
// coordinator). It shells `sysbench <script> ... prepare` / `... run` with the
// pgsql driver against the bundled oltp_* lua workloads and parses
// tps/latency out of the captured output — no sysbench SDK exists, this is the
// same shelling-out approach pgbench and dockerhost use for their CLIs.
//
// Unlike pgbench (delivered from the YB release tarball), sysbench is a distro
// package: EnsureInstalled installs it from apt on the (Debian/Ubuntu-family)
// client image, idempotently. The Debian/Ubuntu sysbench build ships the pgsql
// driver and the oltp_* scripts under /usr/share/sysbench.
package sysbench

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

// DefaultScript is the lua workload driven when Options.Script is empty.
const DefaultScript = "oltp_read_write"

// missingBinaryHint names the install route to fix a missing sysbench binary,
// returned verbatim so callers/logs point at the fix instead of a bare exec
// error.
const missingBinaryHint = "sysbench not found on target; run sysbench.EnsureInstalled " +
	"(apt install on the Debian/Ubuntu-family client image) before Prepare/Run"

// installCmd is the idempotent remote install EnsureInstalled runs: a no-op
// when sysbench is already present, else an apt install (the client image is
// Debian/Ubuntu-family; its sysbench package bundles the pgsql driver).
const installCmd = "command -v sysbench >/dev/null 2>&1 || " +
	"(apt-get update -qq && DEBIAN_FRONTEND=noninteractive apt-get install -y -qq sysbench)"

// PrepareOptions parameterizes `sysbench <script> ... prepare` (dataset load).
type PrepareOptions struct {
	Conn      pg.ConnInfo
	Script    string // lua workload; "" → DefaultScript
	Tables    int    // --tables; 0 → sysbench's own default (1)
	TableSize int    // --table-size; 0 → sysbench's own default (10000)
	ExtraArgs []string
}

// RunOptions parameterizes a `sysbench <script> ... run` load run. Tables and
// TableSize must match the prepared dataset — the scripts use them to pick
// tables/rows at run time too.
type RunOptions struct {
	Conn           pg.ConnInfo
	Script         string // lua workload; "" → DefaultScript
	Tables         int
	TableSize      int
	Threads        int           // --threads
	Duration       time.Duration // --time, rounded up to whole seconds
	ReportInterval time.Duration // --report-interval; 0 → no periodic progress
	ExtraArgs      []string
}

// Result is the parsed outcome of a sysbench run. Raw always carries the full
// captured output, even when parsing found nothing (fields stay zero) or the
// run failed, so a caller can attach it for triage regardless.
type Result struct {
	TPS          float64
	LatencyAvgMs float64
	LatencyP95Ms float64
	Raw          string
}

// EnsureInstalled installs sysbench on ex when it is not already present.
func EnsureInstalled(ctx context.Context, ex exec.RemoteExecutor) error {
	res, err := ex.Run(ctx, installCmd)
	if err != nil {
		return fmt.Errorf("install sysbench: %w", err)
	}
	if res.ExitCode != 0 {
		return fmt.Errorf("install sysbench exited %d: %s", res.ExitCode, errText(res))
	}
	return nil
}

// Prepare runs `sysbench <script> --tables --table-size ... prepare` on ex
// against opts.Conn, loading the workload's dataset.
func Prepare(ctx context.Context, ex exec.RemoteExecutor, opts PrepareOptions) error {
	if err := checkAvailable(ctx, ex); err != nil {
		return err
	}
	args := []string{"sysbench", script(opts.Script)}
	args = append(args, connArgs(opts.Conn)...)
	args = append(args, shapeArgs(opts.Tables, opts.TableSize)...)
	args = append(args, opts.ExtraArgs...)
	args = append(args, "prepare")

	res, err := ex.Run(ctx, envPrefix(opts.Conn)+strings.Join(args, " "))
	if err != nil {
		return fmt.Errorf("sysbench prepare: %w", err)
	}
	if res.ExitCode != 0 {
		return fmt.Errorf("sysbench prepare exited %d: %s", res.ExitCode, errText(res))
	}
	return nil
}

// Run runs `sysbench <script> --threads --time ... run` on ex and parses
// tps/latency out of the output.
func Run(ctx context.Context, ex exec.RemoteExecutor, opts RunOptions) (Result, error) {
	if err := checkAvailable(ctx, ex); err != nil {
		return Result{}, err
	}
	args := []string{"sysbench", script(opts.Script)}
	args = append(args, connArgs(opts.Conn)...)
	args = append(args, shapeArgs(opts.Tables, opts.TableSize)...)
	if opts.Threads > 0 {
		args = append(args, "--threads="+strconv.Itoa(opts.Threads))
	}
	if opts.Duration > 0 {
		args = append(args, "--time="+strconv.Itoa(seconds(opts.Duration)))
	}
	if opts.ReportInterval > 0 {
		args = append(args, "--report-interval="+strconv.Itoa(seconds(opts.ReportInterval)))
	}
	args = append(args, opts.ExtraArgs...)
	args = append(args, "run")

	res, err := ex.Run(ctx, envPrefix(opts.Conn)+strings.Join(args, " "))
	if err != nil {
		return Result{}, fmt.Errorf("sysbench run: %w", err)
	}
	out := res.Stdout + res.Stderr
	if res.ExitCode != 0 {
		return Result{Raw: out}, fmt.Errorf("sysbench run exited %d: %s", res.ExitCode, errText(res))
	}
	return parseOutput(out), nil
}

// checkAvailable detects a missing sysbench binary on the target and returns
// an error naming the install route, rather than a bare "not found" exec
// failure downstream.
func checkAvailable(ctx context.Context, ex exec.RemoteExecutor) error {
	res, err := ex.Run(ctx, "command -v sysbench")
	if err != nil {
		return fmt.Errorf("check sysbench availability: %w", err)
	}
	if res.ExitCode != 0 {
		return fmt.Errorf("%s", missingBinaryHint)
	}
	return nil
}

func script(s string) string {
	if s == "" {
		return DefaultScript
	}
	return shellQuote(s)
}

// connArgs renders the pgsql driver's connection flags; the password travels
// via envPrefix (sysbench's pgsql driver links libpq, which falls back to
// PGPASSWORD when no explicit password is given). Host/user/database values are
// shell-quoted (arbitrary strings); the port is a plain decimal (always safe).
func connArgs(ci pg.ConnInfo) []string {
	args := []string{"--db-driver=pgsql"}
	if ci.Host != "" {
		args = append(args, "--pgsql-host="+shellQuote(ci.Host))
	}
	if ci.Port != 0 {
		args = append(args, "--pgsql-port="+strconv.Itoa(ci.Port))
	}
	if ci.User != "" {
		args = append(args, "--pgsql-user="+shellQuote(ci.User))
	}
	if ci.Database != "" {
		args = append(args, "--pgsql-db="+shellQuote(ci.Database))
	}
	return args
}

// shapeArgs renders the dataset-shape flags shared by prepare and run.
func shapeArgs(tables, tableSize int) []string {
	var args []string
	if tables > 0 {
		args = append(args, "--tables="+strconv.Itoa(tables))
	}
	if tableSize > 0 {
		args = append(args, "--table-size="+strconv.Itoa(tableSize))
	}
	return args
}

// envPrefix renders a leading `PGPASSWORD=... ` shell assignment when a
// password is set, so it never appears as a plain sysbench argument (visible
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

// errText is the diagnostic tail for a non-zero exit: stderr, falling back to
// stdout (some sysbench failures print only there), then a fixed marker.
func errText(res exec.Result) string {
	if s := strings.TrimSpace(res.Stderr); s != "" {
		return s
	}
	if s := strings.TrimSpace(res.Stdout); s != "" {
		return s
	}
	return "no output"
}

func seconds(d time.Duration) int {
	if d <= 0 {
		return 0
	}
	// Ceiling, not Round: a sub-second positive duration must never become
	// --time=0 (sysbench treats 0 as "run forever").
	return int((d + time.Second - 1) / time.Second)
}

// tpsRe / latencyAvgRe / latencyP95Re match sysbench's summary block:
//
//	transactions:  24118  (401.93 per sec.)
//	Latency (ms):
//	     avg:                19.90
//	     95th percentile:    38.25
var (
	tpsRe        = regexp.MustCompile(`(?m)^\s*transactions:\s+\d+\s+\(([0-9.]+) per sec\.\)`)
	latencyAvgRe = regexp.MustCompile(`(?m)^\s*avg:\s+([0-9.]+)`)
	latencyP95Re = regexp.MustCompile(`(?m)^\s*95th percentile:\s+([0-9.]+)`)
)

// parseOutput extracts tps, average latency, and the 95th-percentile latency
// from raw sysbench output. Missing fields are left zero rather than erroring —
// Raw always carries the full text for a caller/attachment to fall back on.
func parseOutput(raw string) Result {
	r := Result{Raw: raw}
	if m := tpsRe.FindStringSubmatch(raw); m != nil {
		r.TPS, _ = strconv.ParseFloat(m[1], 64)
	}
	if m := latencyAvgRe.FindStringSubmatch(raw); m != nil {
		r.LatencyAvgMs, _ = strconv.ParseFloat(m[1], 64)
	}
	if m := latencyP95Re.FindStringSubmatch(raw); m != nil {
		r.LatencyP95Ms, _ = strconv.ParseFloat(m[1], 64)
	}
	return r
}
