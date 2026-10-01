package sqlancer

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yugabyte/amp/test/functional/framework/exec"
)

// scripted answers each command by the first matching substring rule.
type scripted struct {
	mu    sync.Mutex
	rules []rule
	cmds  []string
}

type rule struct {
	contains string
	res      exec.Result
}

func (s *scripted) Run(_ context.Context, cmd string) (exec.Result, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cmds = append(s.cmds, cmd)
	for _, r := range s.rules {
		if strings.Contains(cmd, r.contains) {
			return r.res, nil
		}
	}
	return exec.Result{}, nil
}

func (s *scripted) Copy(context.Context, string, string) error { return nil }

func testSpec() RunSpec {
	return RunSpec{
		Oracle:  "NOREC",
		Target:  Target{Host: "yb-ep-1", Port: 55433, User: "cloud_admin", Password: "pw"},
		Jar:     testJar,
		WorkDir: "/opt/ampqa/sqlancer-runs/norec",
		Threads: 4,
		Timeout: 10 * time.Minute,
		MaxHeap: "4g",
	}
}

func TestBuildRunCommandTargetsTheAmpProvider(t *testing.T) {
	cmd := BuildRunCommand(testSpec())
	for _, want := range []string{
		"--host 'yb-ep-1' --port 55433",
		"--num-threads 4",
		"--timeout-seconds 600",
		"--database-prefix 'sqlancer_norec_'",
		"--exit-code-error 3",
		" amp --oracle 'NOREC'",
	} {
		if !strings.Contains(cmd, want) {
			t.Errorf("run command lacks %q:\n%s", want, cmd)
		}
	}
	// Global options must precede the provider name, or SQLancer reads them as
	// provider options and rejects them.
	if strings.Index(cmd, "--exit-code-error") > strings.Index(cmd, " amp ") {
		t.Errorf("a global option follows the provider name:\n%s", cmd)
	}
}

func TestBuildInstallCommandPinsTheReleaseItDownloads(t *testing.T) {
	cmd := BuildInstallCommand(Config{ReleaseRepo: DefaultReleaseRepo, ReleaseTag: DefaultReleaseTag,
		ReleaseAsset: DefaultReleaseAsset, InstallDir: DefaultInstallDir})
	for _, want := range []string{
		"https://api.github.com/repos/yugabyte/sqlancer/git/ref/tags/sqlancer_2.0.0-yb",
		"-o '/opt/ampqa/sqlancer/sqlancer-2.0.0-yb.tar.gz' " +
			"'https://github.com/yugabyte/sqlancer/releases/download/sqlancer_2.0.0-yb/sqlancer-2.0.0-yb.tar.gz'",
		`[ "$before" = "$after" ]`,
		"exit 65",
		"sha256sum '/opt/ampqa/sqlancer/sqlancer-2.0.0-yb.tar.gz'",
	} {
		if !strings.Contains(cmd, want) {
			t.Errorf("install command lacks %q:\n%s", want, cmd)
		}
	}
	// The tag is read on both sides of the download, or a moved tag goes unseen.
	if strings.Count(cmd, "git/ref/tags/") != 2 {
		t.Errorf("tag commit must be read before and after the download:\n%s", cmd)
	}
	if strings.Index(cmd, "after=") < strings.Index(cmd, "curl -fsSL --retry 3") {
		t.Errorf("tag re-read precedes the download:\n%s", cmd)
	}
}

// A thread's -cur.log is the statement log of work in progress, present on
// every run; only the other logs are reproducers.
func TestFindingLogsSkipsInProgressLogs(t *testing.T) {
	got := FindingLogs("sqlancer_norec_1-cur.log\nsqlancer_norec_0.log\n\nsqlancer_norec_0-cur.log\nREADME\n")
	if len(got) != 1 || got[0] != "sqlancer_norec_0.log" {
		t.Errorf("FindingLogs = %v, want [sqlancer_norec_0.log]", got)
	}
}

func TestRunClassifiesByReproducerLogs(t *testing.T) {
	cases := []struct {
		name    string
		exit    string
		ls      string
		want    Verdict
		wantErr bool
	}{
		{"clean", "0", "sqlancer_norec_0-cur.log", VerdictClean, false},
		{"finding", "3", "sqlancer_norec_0.log\nsqlancer_norec_0-cur.log", VerdictFindings, false},
		// Non-zero with no reproducer never got going: a harness problem.
		{"no start", "3", "", "", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ex := &scripted{rules: []rule{
				{"nohup", exec.Result{Stdout: "123\n"}},
				{"cat '/opt/ampqa/sqlancer-runs/norec/sqlancer.exit'", exec.Result{Stdout: c.exit + "\n"}},
				{"ls -1", exec.Result{Stdout: c.ls}},
			}}
			a := New(ex, Config{PollInterval: time.Millisecond}, nil)
			res, err := a.Run(context.Background(), testSpec())
			if (err != nil) != c.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, c.wantErr)
			}
			if !c.wantErr && res.Verdict != c.want {
				t.Errorf("verdict = %s, want %s", res.Verdict, c.want)
			}
		})
	}
}

const (
	testCommit = "9ff8d1b0b2c3d4e5f60718293a4b5c6d7e8f9012"
	testSum    = "a11f38c602a620d7422cedbd55cb276fc3851bee9f3a439c1b46c392ef1e5eae"
	testJar    = "/opt/ampqa/sqlancer/sqlancer-2.0.0-yb.jar"
)

func installApp(exit, out string) *App {
	ex := &scripted{rules: []rule{
		{"nohup", exec.Result{Stdout: "7\n"}},
		{"cat '/opt/ampqa/sqlancer-install.exit'", exec.Result{Stdout: exit + "\n"}},
		{"tail -c", exec.Result{Stdout: out}},
	}}
	return New(ex, Config{ReleaseTag: DefaultReleaseTag, InstallDir: DefaultInstallDir,
		InstallTimeout: time.Minute, PollInterval: time.Millisecond}, nil)
}

func TestInstallReturnsCommitSumAndJar(t *testing.T) {
	a := installApp("0", testCommit+"\n"+testSum+"\n"+testJar+"\n")
	got, err := a.Install(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if want := (Installed{Commit: testCommit, SHA256: testSum, Jar: testJar}); got != want {
		t.Errorf("Install = %+v, want %+v", got, want)
	}
}

func TestInstallRejectsAMovedTag(t *testing.T) {
	a := installApp("65", "release tag moved during download: aaa -> bbb\n")
	_, err := a.Install(context.Background())
	if err == nil || !strings.Contains(err.Error(), "moved during the download") {
		t.Errorf("err = %v, want a moved-tag error", err)
	}
}

// A download that printed an error page instead of a commit must not pass as a
// build that was tested.
func TestInstallRejectsOutputThatIsNotAPinnedBuild(t *testing.T) {
	a := installApp("0", "<html>\nnot found\n"+testJar+"\n")
	if _, err := a.Install(context.Background()); err == nil {
		t.Error("Install accepted output with no commit or sha256")
	}
}

// A run whose process vanished before writing its exit code still has the
// reproducers its threads wrote, and those are a finding, not an env failure.
func TestRunAbandonedWithReproducersIsAFinding(t *testing.T) {
	ex := &scripted{rules: []rule{
		{"nohup", exec.Result{Stdout: "123\n"}},
		{"kill -0", exec.Result{Stdout: "GONE\n"}},
		{"ls -1", exec.Result{Stdout: "sqlancer_norec_0.log\nsqlancer_norec_1-cur.log\n"}},
	}}
	a := New(ex, Config{PollInterval: time.Millisecond}, nil)
	res, err := a.Run(context.Background(), testSpec())
	if err != nil {
		t.Fatalf("err = %v, want a finding", err)
	}
	if res.Verdict != VerdictFindings || len(res.Findings) != 1 || res.RunError == "" {
		t.Errorf("result = %+v, want 1 finding with a RunError", res)
	}
}

// With nothing on disk, the same abandoned run is a harness error.
func TestRunAbandonedWithoutReproducersIsAnError(t *testing.T) {
	ex := &scripted{rules: []rule{
		{"nohup", exec.Result{Stdout: "123\n"}},
		{"kill -0", exec.Result{Stdout: "GONE\n"}},
	}}
	a := New(ex, Config{PollInterval: time.Millisecond}, nil)
	if _, err := a.Run(context.Background(), testSpec()); err == nil {
		t.Fatal("err = nil, want the abandoned run reported")
	}
}
