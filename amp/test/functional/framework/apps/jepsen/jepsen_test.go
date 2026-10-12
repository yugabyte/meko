package jepsen_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yugabyte/amp/test/functional/framework/apps/jepsen"
	"github.com/yugabyte/amp/test/functional/framework/exec"
)

// scriptExec is a scripted fake RemoteExecutor: records every command and
// answers by substring match (in registration order), the convention the other
// apps/* fakes in this module use.
type scriptExec struct {
	mu       sync.Mutex
	commands []string
	rules    []rule
	fallback exec.Result
}

type rule struct {
	match string
	res   exec.Result
}

func (s *scriptExec) on(match string, res exec.Result) *scriptExec {
	s.rules = append(s.rules, rule{match, res})
	return s
}

func (s *scriptExec) Run(_ context.Context, cmd string) (exec.Result, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.commands = append(s.commands, cmd)
	for _, r := range s.rules {
		if strings.Contains(cmd, r.match) {
			return r.res, nil
		}
	}
	return s.fallback, nil
}

func (s *scriptExec) Copy(context.Context, string, string) error { return nil }

func (s *scriptExec) seen(sub string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, c := range s.commands {
		if strings.Contains(c, sub) {
			return true
		}
	}
	return false
}

// memSink collects artifacts in memory.
type memSink struct {
	mu    sync.Mutex
	files map[string]string
}

func newMemSink() *memSink { return &memSink{files: map[string]string{}} }

func (m *memSink) Add(name, _ string, content []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.files[name] = string(content)
	return nil
}

// --- lifecycle ---

// The run marker is an environment entry and pgrep -f reads only argv, so the
// lifecycle has to find the run's processes through /proc/<pid>/environ.
func TestStopFindsTheRunByItsEnvironment(t *testing.T) {
	ex := &scriptExec{}
	spec := jepsen.RunSpec{Workload: jepsen.SZAppend, Nodes: []string{"n"}, Marker: "m1"}
	app := jepsen.New(ex, spec, jepsen.WithLogf(func(string, ...any) {}))
	if err := app.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(jepsen.BuildCommand(spec), "AMPQA_JEPSEN_MARKER='m1' nohup lein") {
		t.Fatalf("launch does not set the marker the stop looks for:\n%s", jepsen.BuildCommand(spec))
	}
	if !ex.seen("'AMPQA_JEPSEN_MARKER=m1' /proc/[0-9]*/environ") || !ex.seen("kill -KILL") {
		t.Errorf("stop does not kill by the environment entry: %q", ex.commands)
	}
	if ex.seen("pgrep") || ex.seen("pkill") {
		t.Errorf("stop uses pgrep/pkill, which never see an environment entry: %q", ex.commands)
	}
}

// A capture cap that cuts a tail short keeps its first bytes, so the header must
// not claim to hold the last ones.
func TestCollectMarksACaptureCapCut(t *testing.T) {
	ex := (&scriptExec{}).
		on("wc -c", exec.Result{Stdout: "500"}).
		on("tail -c", exec.Result{Stdout: "abc", Truncated: true})
	app := jepsen.New(ex, jepsen.RunSpec{Workload: jepsen.SZAppend, Nodes: []string{"n"}},
		jepsen.WithLogf(func(string, ...any) {}))
	sink := newMemSink()
	if err := app.CollectMainLogOnly(context.Background(), sink); err != nil {
		t.Fatal(err)
	}
	if got := sink.files["jepsen/ysql__sz_ol_append/launch.log"]; !strings.Contains(got, "cut by the exec capture cap") {
		t.Errorf("launch.log = %q, want the capture-cap header", got)
	}
}

// --- catalog ---

func TestYSQLWorkloadsExcludeYCQL(t *testing.T) {
	ysql := jepsen.YSQL()
	if len(ysql) != 17 {
		t.Fatalf("YSQL() = %d workloads, want 17", len(ysql))
	}
	for _, w := range ysql {
		if strings.HasPrefix(string(w), "ycql/") {
			t.Errorf("YSQL() returned a YCQL workload: %s", w)
		}
	}
}

func TestIsolationFiltersPartitionTheYSQLSet(t *testing.T) {
	sz, si, rc := jepsen.Serializable(), jepsen.SnapshotIsolation(), jepsen.ReadCommitted()
	if got := len(sz) + len(si) + len(rc); got != len(jepsen.YSQL()) {
		t.Errorf("sz+si+rc = %d, want the whole YSQL set (%d)", got, len(jepsen.YSQL()))
	}
	for _, w := range sz {
		if !strings.Contains(string(w), "sz.") {
			t.Errorf("Serializable() returned %s", w)
		}
	}
}

func TestWorkloadFamilies(t *testing.T) {
	cases := []struct {
		w         jepsen.Workload
		bank, set bool
	}{
		{jepsen.SZBank, true, false},
		{jepsen.SIBankContention, true, false},
		{jepsen.SZSet, false, true},
		{jepsen.SZLongFork, false, true},
		{jepsen.SZAppend, false, false},
	}
	for _, c := range cases {
		if got := c.w.IsBank(); got != c.bank {
			t.Errorf("%s IsBank = %v, want %v", c.w, got, c.bank)
		}
		if got := c.w.IsSet(); got != c.set {
			t.Errorf("%s IsSet = %v, want %v", c.w, got, c.set)
		}
	}
}

func TestFileNameIsFilesystemSafe(t *testing.T) {
	if got := jepsen.SZAppend.FileName(); got != "ysql__sz_ol_append" {
		t.Errorf("FileName = %q", got)
	}
}

// --- command rendering ---

func TestBuildCommandDisablesJepsensOwnNemesisAndSSH(t *testing.T) {
	cmd := jepsen.BuildCommand(jepsen.RunSpec{
		Workload:    jepsen.SZAppend,
		Nodes:       []string{"ampqa-jepsen-ep-abc"},
		Concurrency: "30n",
		TimeLimit:   10 * time.Minute,
		RepoDir:     "/opt/ampqa/jepsen",
		NoSSH:       true,
		Marker:      "m1",
	})
	for _, want := range []string{
		"cd '/opt/ampqa/jepsen/yugabyte'",
		"--nodes 'ampqa-jepsen-ep-abc'",
		"--concurrency '30n'",
		"--time-limit 600",
		"--workload 'ysql/sz.ol.append'",
		"--url 'NONE'",
		"--env 'yba'",
		"--no-ssh",
		"--os 'noop'",
		// The load-bearing one: faults come from framework/chaos, so jepsen's
		// own nemesis must never be armed.
		"--nemesis 'none'",
		"AMPQA_JEPSEN_MARKER='m1'",
	} {
		if !strings.Contains(cmd, want) {
			t.Errorf("command missing %q:\n%s", want, cmd)
		}
	}
}

func TestBuildCommandRendersExtraFlagsVerbatim(t *testing.T) {
	cmd := jepsen.BuildCommand(jepsen.RunSpec{
		Workload: jepsen.SIBank, Nodes: []string{"n"}, Extra: []string{"--table-count 4"},
	})
	if !strings.Contains(cmd, "--table-count 4") {
		t.Errorf("extra flag missing:\n%s", cmd)
	}
}

func TestBuildSetupCommandIsIdempotentAndPinnable(t *testing.T) {
	cmd := jepsen.BuildSetupCommand(jepsen.SetupSpec{RepoRef: "deadbeef"})
	for _, want := range []string{
		"if [ ! -d '/opt/ampqa/jepsen/.git' ]; then git clone",
		"git fetch --tags --force origin 'deadbeef'",
		"git checkout --detach FETCH_HEAD",
		"lein deps",
	} {
		if !strings.Contains(cmd, want) {
			t.Errorf("setup command missing %q:\n%s", want, cmd)
		}
	}
}

// --- lifecycle ---

func TestWaitReadyReturnsOnSetupMarker(t *testing.T) {
	ex := (&scriptExec{}).
		on("'Test crashed!'", exec.Result{ExitCode: 1}).
		on("'Setup successful'", exec.Result{ExitCode: 0})
	app := jepsen.New(ex, jepsen.RunSpec{Workload: jepsen.SZAppend, Nodes: []string{"n"}},
		jepsen.WithLogf(func(string, ...any) {}))
	if err := app.WaitReady(context.Background()); err != nil {
		t.Fatalf("WaitReady: %v", err)
	}
}

// A crash during setup must fail fast rather than burn the whole setup timeout,
// because the run it would gate has already ended.
func TestWaitReadyFailsFastOnCrash(t *testing.T) {
	ex := (&scriptExec{}).
		on("'Test crashed!'", exec.Result{ExitCode: 0}).
		on("'Setup successful'", exec.Result{ExitCode: 1})
	app := jepsen.New(ex, jepsen.RunSpec{Workload: jepsen.SZAppend, Nodes: []string{"n"}},
		jepsen.WithLogf(func(string, ...any) {}),
		jepsen.WithSetupTimeout(time.Hour))
	err := app.WaitReady(context.Background())
	if err == nil || !strings.Contains(err.Error(), "crashed") {
		t.Fatalf("WaitReady error = %v, want a crash report", err)
	}
}

// The verdict is a positive pass, and only once "Analysis complete" is there.
// A log that says neither is silent on the question, and silence must not read
// as a pass.
func TestValidRequiresAnAnalysis(t *testing.T) {
	ex := (&scriptExec{fallback: exec.Result{ExitCode: 1}})
	app := jepsen.New(ex, jepsen.RunSpec{Workload: jepsen.SZAppend, Nodes: []string{"n"}},
		jepsen.WithLogf(func(string, ...any) {}))
	if _, err := app.Valid(context.Background()); err == nil {
		t.Fatal("Valid on a log with no analysis returned no error")
	}
}

// jepsen's verdict is THREE-WAY. A checker that errors out prints "no anomalies
// found" and :valid? :unknown — the history is not accused of anything, but the
// analysis did not finish. Reading only for "Analysis invalid" reports that as a
// pass, which is how a suite silently stops testing anything. Observed for real:
// without gnuplot installed, every workload returned :unknown while every
// consistency checker passed.
func TestVerdictDistinguishesUnknownFromValid(t *testing.T) {
	for _, c := range []struct {
		name    string
		markers map[string]bool
		want    jepsen.Verdict
	}{
		{"clean pass", map[string]bool{jepsen.MarkerValid: true}, jepsen.VerdictValid},
		{"violation", map[string]bool{jepsen.MarkerInvalid: true}, jepsen.VerdictInvalid},
		{"analysis errored", map[string]bool{jepsen.MarkerUnknown: true}, jepsen.VerdictUnknown},
		// An invalid run is the finding and wins over anything else present.
		{"invalid wins", map[string]bool{jepsen.MarkerInvalid: true, jepsen.MarkerUnknown: true}, jepsen.VerdictInvalid},
	} {
		t.Run(c.name, func(t *testing.T) {
			ex := &scriptExec{fallback: exec.Result{ExitCode: 1}}
			ex.on("'"+jepsen.MarkerAnalysisDone+"'", exec.Result{ExitCode: 0})
			for m := range c.markers {
				ex.on("'"+m+"'", exec.Result{ExitCode: 0})
			}
			app := jepsen.New(ex, jepsen.RunSpec{Workload: jepsen.SZAppend, Nodes: []string{"n"}},
				jepsen.WithLogf(func(string, ...any) {}))
			got, err := app.Verdict(context.Background())
			if err != nil {
				t.Fatalf("Verdict: %v", err)
			}
			if got != c.want {
				t.Errorf("Verdict = %q, want %q", got, c.want)
			}
			valid, _ := app.Valid(context.Background())
			if valid != (c.want == jepsen.VerdictValid) {
				t.Errorf("Valid = %v for verdict %q", valid, got)
			}
		})
	}
}

// The plotting checkers shell out to gnuplot and graphviz; without them the
// whole analysis degrades to :unknown.
func TestSetupCommandInstallsThePlottingTools(t *testing.T) {
	cmd := jepsen.BuildSetupCommand(jepsen.SetupSpec{})
	for _, want := range []string{"gnuplot", "graphviz"} {
		if !strings.Contains(cmd, want) {
			t.Errorf("setup command does not install %s:\n%s", want, cmd)
		}
	}
}

func TestValidReadsTheVerdict(t *testing.T) {
	for _, c := range []struct {
		name    string
		invalid bool
		want    bool
	}{
		{"passing run", false, true},
		{"violation", true, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			invalidExit, validExit := 1, 0
			if c.invalid {
				invalidExit, validExit = 0, 1
			}
			ex := (&scriptExec{fallback: exec.Result{ExitCode: 1}}).
				on("'Analysis complete'", exec.Result{ExitCode: 0}).
				on("'Analysis invalid'", exec.Result{ExitCode: invalidExit}).
				on("'Everything looks good'", exec.Result{ExitCode: validExit})
			app := jepsen.New(ex, jepsen.RunSpec{Workload: jepsen.SZAppend, Nodes: []string{"n"}},
				jepsen.WithLogf(func(string, ...any) {}))
			got, err := app.Valid(context.Background())
			if err != nil {
				t.Fatalf("Valid: %v", err)
			}
			if got != c.want {
				t.Errorf("Valid = %v, want %v", got, c.want)
			}
		})
	}
}

func TestStartRejectsAnEmptyTarget(t *testing.T) {
	app := jepsen.New(&scriptExec{}, jepsen.RunSpec{Workload: jepsen.SZAppend})
	if err := app.Start(context.Background()); err == nil {
		t.Fatal("Start with no nodes returned no error")
	}
}

// --- collection ---

func TestCollectMarksATruncatedFile(t *testing.T) {
	ex := (&scriptExec{}).
		on("wc -c", exec.Result{ExitCode: 0, Stdout: "5000\n"}).
		on("find .", exec.Result{ExitCode: 0, Stdout: ""}).
		on("tail -c", exec.Result{ExitCode: 0, Stdout: "tail-bytes"})
	app := jepsen.New(ex, jepsen.RunSpec{Workload: jepsen.SZAppend, Nodes: []string{"n"}},
		jepsen.WithLogf(func(string, ...any) {}),
		jepsen.WithCollectMaxBytes(100))
	sink := newMemSink()
	if err := app.Collect(context.Background(), sink); err != nil {
		t.Fatalf("Collect: %v", err)
	}
	body, ok := sink.files["jepsen/ysql__sz_ol_append/launch.log"]
	if !ok {
		t.Fatalf("launch log not collected, got %v", sink.files)
	}
	if !strings.Contains(body, "truncated — last 100 of 5000 bytes") {
		t.Errorf("truncation not marked: %q", body)
	}
}

// CleanStore must never run before the store is collected; this asserts the
// command it issues, so the ordering in the suite stays reviewable.
func TestCleanStoreRemovesOnlyTheStore(t *testing.T) {
	ex := &scriptExec{}
	app := jepsen.New(ex, jepsen.RunSpec{Workload: jepsen.SZAppend, Nodes: []string{"n"},
		RepoDir: "/opt/ampqa/jepsen"})
	if err := app.CleanStore(context.Background()); err != nil {
		t.Fatalf("CleanStore: %v", err)
	}
	if !ex.seen("rm -rf '/opt/ampqa/jepsen/yugabyte/store'") {
		t.Errorf("CleanStore issued %v", ex.commands)
	}
}

// --- database setup ---

type fakeDB struct {
	mu    sync.Mutex
	stmts []string
	fail  map[string]int // statement substring → remaining failures
}

func (f *fakeDB) Exec(_ context.Context, sql string, _ ...any) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.stmts = append(f.stmts, sql)
	for sub, n := range f.fail {
		if n > 0 && strings.Contains(sql, sub) {
			f.fail[sub]--
			return 0, errors.New("database \"jepsen\" is being accessed by other users")
		}
	}
	return 0, nil
}

// The stress driver's sequence also runs `grant yb_extension to jepsen`. That
// role exists on YugabyteDB and NOT on an amp compute, so the grant would abort
// the whole setup — its absence here is deliberate and is what this asserts.
func TestSetupDatabaseSkipsTheYugabyteOnlyGrant(t *testing.T) {
	db := &fakeDB{}
	if err := jepsen.SetupDatabase(context.Background(), db, time.Minute); err != nil {
		t.Fatalf("SetupDatabase: %v", err)
	}
	joined := strings.Join(db.stmts, "; ")
	if strings.Contains(joined, "yb_extension") {
		t.Errorf("setup granted yb_extension, which an amp compute does not define: %s", joined)
	}
	for _, want := range []string{"CREATE USER jepsen CREATEDB", "CREATE DATABASE jepsen"} {
		if !strings.Contains(joined, want) {
			t.Errorf("setup missing %q: %s", want, joined)
		}
	}
}

// An amp endpoint requires password authentication, and jepsen hardcodes the
// password as the literal "jepsen". A role created without one is accepted by
// CREATE USER and then fails every connection — which jepsen surfaces as a
// connection timeout, so the endpoint reads as unreachable rather than
// unauthenticated. This is the single line that keeps that from recurring.
func TestSetupDatabaseGivesTheRoleThePasswordJepsenDials(t *testing.T) {
	db := &fakeDB{}
	if err := jepsen.SetupDatabase(context.Background(), db, time.Minute); err != nil {
		t.Fatalf("SetupDatabase: %v", err)
	}
	joined := strings.Join(db.stmts, "; ")
	if !strings.Contains(joined, "CREATE USER jepsen CREATEDB PASSWORD '"+jepsen.RolePassword+"'") {
		t.Errorf("the role is created without the password jepsen dials with: %s", joined)
	}
	if jepsen.RolePassword != "jepsen" {
		t.Errorf("RolePassword = %q; jepsen hardcodes \"jepsen\" and cannot be told otherwise", jepsen.RolePassword)
	}
}

func TestResetDatabaseRetriesAHeldDatabase(t *testing.T) {
	db := &fakeDB{fail: map[string]int{"DROP DATABASE": 2}}
	if err := jepsen.ResetDatabase(context.Background(), db, 30*time.Second); err != nil {
		t.Fatalf("ResetDatabase: %v", err)
	}
	if len(db.stmts) < 4 {
		t.Errorf("expected retries, got %v", db.stmts)
	}
}

// --- config ---

type mapGetter map[string]string

func (m mapGetter) GetString(k, def string) string {
	if v, ok := m[k]; ok {
		return v
	}
	return def
}

func (m mapGetter) GetInt(k string, def int) int { return def }

func (m mapGetter) GetMapSlice(string, []map[string]any) []map[string]any { return nil }

func (m mapGetter) GetDuration(k string, def time.Duration) time.Duration {
	if v, ok := m[k]; ok {
		d, err := time.ParseDuration(v)
		if err == nil {
			return d
		}
	}
	return def
}

func TestFromConfigDefaults(t *testing.T) {
	cfg := jepsen.FromConfig(mapGetter{})
	if cfg.RepoRef != jepsen.DefaultRepoRef || cfg.Concurrency != jepsen.DefaultConcurrency {
		t.Errorf("defaults not applied: %+v", cfg)
	}
}

// Shape carries the per-family overrides the stress suite needs: a set or
// long-fork history OOMs the analyser at full concurrency, a multi-key-acid
// one is never analysed in time, and bank's ten accounts are pure contention
// past a handful of threads.
func TestShapeOverridesTheHeavyFamilies(t *testing.T) {
	cfg := jepsen.FromConfig(mapGetter{"apps.jepsen.time_to_run": "10m"})
	if c, d := cfg.Shape(jepsen.SZSet); c != "10n" || d != 5*time.Minute {
		t.Errorf("set shape = %s/%s, want 10n/5m", c, d)
	}
	if c, d := cfg.Shape(jepsen.SZMultiKeyACID); c != "10n" || d != 5*time.Minute {
		t.Errorf("multi-key-acid shape = %s/%s, want 10n/5m", c, d)
	}
	if c, d := cfg.Shape(jepsen.SZBank); c != "5n" || d != 10*time.Minute {
		t.Errorf("bank shape = %s/%s, want 5n/10m", c, d)
	}
	if c, d := cfg.Shape(jepsen.SZAppend); c != "80n" || d != 10*time.Minute {
		t.Errorf("append shape = %s/%s, want 80n/10m", c, d)
	}
}

// The verdict line can lag "Analysis complete"; results.edn decides then.
func TestVerdictFallsBackToResultsEdn(t *testing.T) {
	for _, c := range []struct {
		tail string
		want jepsen.Verdict
	}{
		{"   :also-not #{:strong-serializable}},\n :valid? false}\n", jepsen.VerdictInvalid},
		{" :workload {:valid? true},\n :valid? true}", jepsen.VerdictValid},
		{" :valid? :unknown}\n", jepsen.VerdictUnknown},
	} {
		ex := &scriptExec{fallback: exec.Result{ExitCode: 1}}
		ex.on("'"+jepsen.MarkerAnalysisDone+"'", exec.Result{ExitCode: 0})
		ex.on("results.edn", exec.Result{ExitCode: 0, Stdout: c.tail})
		app := jepsen.New(ex, jepsen.RunSpec{Workload: jepsen.SZAppend, Nodes: []string{"n"}},
			jepsen.WithLogf(func(string, ...any) {}))
		got, err := app.Verdict(context.Background())
		if err != nil || got != c.want {
			t.Errorf("tail %q: Verdict = %q, %v; want %q", c.tail, got, err, c.want)
		}
	}
}
