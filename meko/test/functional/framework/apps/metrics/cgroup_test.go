package metrics

import (
	"context"
	"strings"
	"testing"
	"time"

	execpkg "github.com/yugabyte/meko/meko/test/functional/framework/exec"
)

// fakeRunner returns scripted output per call and records the commands it ran.
type fakeRunner struct {
	out  []execpkg.Result
	err  []error
	cmds []string
	n    int
}

func (f *fakeRunner) Run(_ context.Context, cmd string) (execpkg.Result, error) {
	f.cmds = append(f.cmds, cmd)
	i := f.n
	f.n++
	var res execpkg.Result
	if i < len(f.out) {
		res = f.out[i]
	}
	var err error
	if i < len(f.err) {
		err = f.err[i]
	}
	return res, err
}

func TestCgroupCmdIsOneRoundTripPerLayout(t *testing.T) {
	unknown := cgroupCmd(layoutUnknown)
	for _, want := range []string{"cpu.stat", "memory.current", "cpuacct.usage", "memory.usage_in_bytes"} {
		if !strings.Contains(unknown, want) {
			t.Errorf("unknown-layout cmd missing %s: %s", want, unknown)
		}
	}
	if !strings.Contains(cgroupCmd(layoutV2), "memory.current") || strings.Contains(cgroupCmd(layoutV2), "cpuacct") {
		t.Errorf("v2 cmd = %s", cgroupCmd(layoutV2))
	}
	if !strings.Contains(cgroupCmd(layoutV1), "cpuacct.usage") || strings.Contains(cgroupCmd(layoutV1), "cpu.stat") {
		t.Errorf("v1 cmd = %s", cgroupCmd(layoutV1))
	}
}

func TestParseCgroupV2(t *testing.T) {
	out := "V2\nusage_usec 2500000\nuser_usec 1000000\nsystem_usec 1500000\n@@\n1073741824\n"
	got := parseCgroup(out)
	if got.layout != layoutV2 {
		t.Errorf("layout = %d, want v2", got.layout)
	}
	if !got.haveCPU || got.cpuSec != 2.5 {
		t.Errorf("cpuSec = %v (have %v), want 2.5", got.cpuSec, got.haveCPU)
	}
	if !got.haveMem || got.memBytes != 1073741824 {
		t.Errorf("memBytes = %v, want 1073741824", got.memBytes)
	}
}

func TestParseCgroupV1(t *testing.T) {
	got := parseCgroup("V1\n3000000000\n@@\n524288000\n")
	if got.layout != layoutV1 {
		t.Errorf("layout = %d, want v1", got.layout)
	}
	if !got.haveCPU || got.cpuSec != 3 {
		t.Errorf("cpuSec = %v, want 3 (nanoseconds → seconds)", got.cpuSec)
	}
	if !got.haveMem || got.memBytes != 524288000 {
		t.Errorf("memBytes = %v", got.memBytes)
	}
}

// A pod whose probe returns nothing usable contributes no readings, and the
// sampler must not treat that as an error.
func TestParseCgroupTolerantOfJunk(t *testing.T) {
	got := parseCgroup("cat: /sys/fs/cgroup/cpu.stat: No such file or directory\n")
	if got.haveCPU || got.haveMem || got.layout != layoutUnknown {
		t.Errorf("junk parsed into %+v", got)
	}
}

// CPU is a counter: the first sample only seeds the baseline (memory alone), the
// second reports cores used over the elapsed wall-clock.
func TestSampleCgroupCPUIsDeltaOverWallClock(t *testing.T) {
	ex := &fakeRunner{out: []execpkg.Result{
		{Stdout: "V2\nusage_usec 1000000\n@@\n1048576\n"},
		{Stdout: "V2\nusage_usec 31000000\n@@\n2097152\n"},
	}}
	st := newPodState()
	t0 := time.Unix(1730000000, 0)
	quiet := func(string, ...any) {}

	first := sampleCgroup(context.Background(), ex, st, t0, quiet)
	if len(first) != 1 || first[0].name != "Memory" || first[0].value != 1 {
		t.Fatalf("first sample = %+v, want Memory only at 1 MiB", first)
	}
	if st.cgroup != layoutV2 {
		t.Errorf("layout not remembered: %d", st.cgroup)
	}

	second := sampleCgroup(context.Background(), ex, st, t0.Add(15*time.Second), quiet)
	var cpu *reading
	for i := range second {
		if second[i].name == "CPU" {
			cpu = &second[i]
		}
	}
	if cpu == nil {
		t.Fatalf("second sample carried no CPU reading: %+v", second)
	}
	// 30 CPU-seconds consumed over 15 wall seconds = 2 cores.
	if cpu.value != 2 || cpu.unit != "cores" || cpu.round != 2 {
		t.Errorf("CPU reading = %+v, want 2 cores round 2", *cpu)
	}
	// The remembered layout means the second exec reads the files directly.
	if strings.Contains(ex.cmds[1], "if [ -f") {
		t.Errorf("second exec re-detected the layout: %s", ex.cmds[1])
	}
}

// A probe that fails is logged and skipped, never surfaced to the test.
func TestSampleCgroupSkipsFailedProbe(t *testing.T) {
	ex := &fakeRunner{out: []execpkg.Result{{ExitCode: 1, Stderr: "container not found"}}}
	var logged int
	got := sampleCgroup(context.Background(), ex, newPodState(), time.Now(), func(string, ...any) { logged++ })
	if got != nil {
		t.Errorf("readings = %+v, want none", got)
	}
	if logged != 1 {
		t.Errorf("logged %d times, want 1", logged)
	}
}
