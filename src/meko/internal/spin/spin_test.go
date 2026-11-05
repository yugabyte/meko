package spin

import (
	"bytes"
	"io"
	"strings"
	"sync"
	"testing"
	"time"
)

// Off a terminal a step with a row says what it's doing as a plain line when it starts, then Done
// and Fail print its row; a Wait leaves nothing.
func TestStepWithoutATerminal(t *testing.T) {
	var b bytes.Buffer
	s := Start(&b, "Release", "installing meko 1.2.4")
	if want := "Installing meko 1.2.4\n"; b.String() != want {
		t.Fatalf("Start printed %q, want %q", b.String(), want)
	}
	s.Done("meko 1.2.4 installed")
	if want := "Installing meko 1.2.4\n  ✓ Release         meko 1.2.4 installed\n"; b.String() != want {
		t.Errorf("Done printed %q, want %q", b.String(), want)
	}

	b.Reset()
	Start(&b, "Release", "installing meko 1.2.4").Fail()
	if want := "Installing meko 1.2.4\n  ✗ Release         installing meko 1.2.4\n"; b.String() != want {
		t.Errorf("Fail printed %q, want %q", b.String(), want)
	}

	b.Reset()
	Wait(&b, "Looking for releases").Clear()
	if b.Len() != 0 {
		t.Errorf("Wait left %q", b.String())
	}
}

// On a terminal the row shows the rotating dots, each frame drawn over the last, until the step
// ends: then the line is erased and the row printed in its place.
func TestStepAnimates(t *testing.T) {
	forceAnimation(t)
	var b syncBuffer
	s := Start(&b, "Release", "installing meko 1.2.4")
	waitFor(t, &b, Frames[1])
	s.Done("meko 1.2.4 installed")
	out := b.String()
	if !strings.HasPrefix(out, "\r  "+Frames[0]+" Release         installing meko 1.2.4\033[K") {
		t.Errorf("first frame: %q", out)
	}
	if !strings.HasSuffix(out, "\r\033[K  ✓ Release         meko 1.2.4 installed\n") {
		t.Errorf("end: %q", out)
	}

	b.Reset()
	w := Wait(&b, "Looking for releases")
	waitFor(t, &b, Frames[1])
	w.Clear()
	out = b.String()
	if !strings.HasPrefix(out, "\r  "+Frames[0]+" Looking for releases…\033[K") ||
		!strings.HasSuffix(out, "\033[K\r\033[K") {
		t.Errorf("Wait drew %q", out)
	}
}

// The dots are cyan where rows are colored, as gh's are, and plain where they aren't.
func TestFrameColor(t *testing.T) {
	if got, want := frame(0, true), "\033[36m⣾\033[0m"; got != want {
		t.Errorf("frame(0, true) = %q, want %q", got, want)
	}
	if got := frame(len(Frames)+1, false); got != Frames[1] {
		t.Errorf("frame(%d, false) = %q, want %q", len(Frames)+1, got, Frames[1])
	}
}

// Only a step's first ending counts.
func TestStepEndsOnce(t *testing.T) {
	forceAnimation(t)
	var b syncBuffer
	s := Start(&b, "Release", "installing")
	s.Done("installed")
	s.Fail()
	s.Clear()
	if out := b.String(); strings.Count(out, "✓") != 1 || strings.Contains(out, "✗") ||
		!strings.HasSuffix(out, "\r\033[K  ✓ Release         installed\n") {
		t.Errorf("printed %q", out)
	}
}

func forceAnimation(t *testing.T) {
	t.Helper()
	old, oldInterval := animates, Interval
	animates = func(io.Writer) bool { return true }
	Interval = time.Millisecond
	t.Cleanup(func() { animates, Interval = old, oldInterval })
}

// waitFor waits until b holds s.
func waitFor(t *testing.T, b *syncBuffer, s string) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); !strings.Contains(b.String(), s); {
		if time.Now().After(deadline) {
			t.Fatalf("no %q in %q", s, b.String())
		}
		time.Sleep(time.Millisecond)
	}
}

// syncBuffer is a bytes.Buffer the test reads while the step's goroutine writes to it.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

func (s *syncBuffer) Reset() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.b.Reset()
}
