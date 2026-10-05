package mockpod

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	execpkg "github.com/yugabyte/meko/meko/test/functional/framework/exec"
	"github.com/yugabyte/meko/meko/test/functional/framework/exec/k8s"
)

func TestResolvePrefersAPrebuiltBinaryAndDigestsIt(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mekoqa")
	if err := os.WriteFile(path, []byte("binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	b, err := Resolve(context.Background(), Config{Binary: path, Arch: "arm64"})
	if err != nil {
		t.Fatal(err)
	}
	if b.Path != path || len(b.SHA) != 64 || len(b.Label()) != 16 {
		t.Fatalf("resolved %+v", b)
	}
	if _, err := Resolve(context.Background(), Config{Binary: path + ".missing"}); err == nil {
		t.Fatal("a missing prebuilt binary was accepted")
	}
	if _, err := Resolve(context.Background(), Config{Arch: "riscv"}); err == nil {
		t.Fatal("an unknown arch was accepted")
	}
}

func TestStaticELFRejectsAnythingButAStaticLinuxBinary(t *testing.T) {
	text := filepath.Join(t.TempDir(), "script")
	if err := os.WriteFile(text, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if staticELF(text) {
		t.Error("a shell script counted as a static binary")
	}
}

func TestCommandWaitsForTheBinaryThenRunsIt(t *testing.T) {
	cmd := Command("mock", "llm", "--port", "8080")
	if cmd[0] != "sh" || cmd[1] != "-c" || !strings.Contains(cmd[2], ReadyFile) || !strings.Contains(cmd[2], `exec "$@"`) {
		t.Fatalf("command %q", cmd)
	}
	if got := strings.Join(cmd[3:], " "); got != "sh /opt/mekoqa/mekoqa mock llm --port 8080" {
		t.Fatalf("argv %q", got)
	}
}

type script struct {
	argvs   []string
	ready   bool
	podList string
}

func (s *script) run(_ context.Context, _ io.Reader, argv []string) (execpkg.Result, error) {
	line := strings.Join(argv, " ")
	s.argvs = append(s.argvs, line)
	switch {
	case strings.Contains(line, " get pod "):
		return execpkg.Result{Stdout: s.podList}, nil
	case strings.Contains(line, "test -f"):
		if s.ready {
			return execpkg.Result{}, nil
		}
		return execpkg.Result{ExitCode: 1}, nil
	}
	return execpkg.Result{}, nil
}

func installed(s *script) bool {
	for _, a := range s.argvs {
		if strings.Contains(a, " cp ") {
			return true
		}
	}
	return false
}

func TestInstallCopiesIntoThePodOfTheCurrentBinaryOnce(t *testing.T) {
	s := &script{podList: "llmmock-abc"}
	kube := k8s.New(k8s.Config{Namespace: "ns"}).WithRunner(s.run)
	b := Binary{Path: "/x/mekoqa", SHA: "0123456789abcdef0123"}
	if err := Install(context.Background(), kube, "llmmock", b, "", time.Second); err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(s.argvs, "\n")
	for _, want := range []string{"app=llmmock,mekoqa/bin=0123456789abcdef", "cp /x/mekoqa ns/llmmock-abc:/opt/mekoqa/mekoqa", "chmod 755 /opt/mekoqa/mekoqa && touch /opt/mekoqa/.ready"} {
		if !strings.Contains(joined, want) {
			t.Errorf("no command with %q in\n%s", want, joined)
		}
	}

	s2 := &script{podList: "llmmock-abc", ready: true}
	kube = k8s.New(k8s.Config{Namespace: "ns"}).WithRunner(s2.run)
	if err := Install(context.Background(), kube, "llmmock", b, "", time.Second); err != nil {
		t.Fatal(err)
	}
	if installed(s2) {
		t.Error("a pod that already has the binary got it again")
	}
}

func TestInstallFailsWhenNoPodComesUp(t *testing.T) {
	s := &script{}
	kube := k8s.New(k8s.Config{Namespace: "ns"}).WithRunner(s.run)
	if err := Install(context.Background(), kube, "llmmock", Binary{SHA: "ab"}, "", 10*time.Millisecond); err == nil {
		t.Fatal("no error without a pod")
	}
}

func TestInstallSelectsThePodOfTheCurrentConfig(t *testing.T) {
	s := &script{podList: "cognito-mock-new"}
	kube := k8s.New(k8s.Config{Namespace: "ns"}).WithRunner(s.run)
	if err := Install(context.Background(), kube, "cognito-mock", Binary{Path: "/x/mekoqa", SHA: "ab"}, "c0ffee", time.Second); err != nil {
		t.Fatal(err)
	}
	if joined := strings.Join(s.argvs, "\n"); !strings.Contains(joined, "app=cognito-mock,mekoqa/bin=ab,mekoqa/config=c0ffee") {
		t.Errorf("the pod was not selected by its config:\n%s", joined)
	}
}
