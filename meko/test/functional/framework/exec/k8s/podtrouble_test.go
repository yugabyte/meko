package k8s

import "strings"
import "testing"

func TestPickTroubleLineFindsSchedulingFailure(t *testing.T) {
	out := `Name:  yb-ep-8be65ab0
Status:  Pending
Events:
  Type     Reason            Age   From               Message
  Warning  FailedScheduling  57s   default-scheduler  0/33 nodes are available: 25 Insufficient cpu, 32 Insufficient ephemeral-storage, 6 Insufficient memory.
`
	got := pickTroubleLine(out)
	if !strings.Contains(got, "Insufficient ephemeral-storage") {
		t.Fatalf("want the scheduling message, got %q", got)
	}
}

func TestPickTroubleLinePrefersTheLastOne(t *testing.T) {
	out := "Warning FailedScheduling 5m 0/10 nodes: Insufficient cpu\nWarning FailedScheduling 1m 0/34 nodes: untolerated taint(s)\n"
	if got := pickTroubleLine(out); !strings.Contains(got, "untolerated taint") {
		t.Fatalf("want the most recent line, got %q", got)
	}
}

func TestPickTroubleLineEmptyWhenHealthy(t *testing.T) {
	if got := pickTroubleLine("Name: ok\nStatus: Running\nEvents: <none>\n"); got != "" {
		t.Fatalf("want no verdict for a healthy pod, got %q", got)
	}
}

func TestPickTroubleLineTruncates(t *testing.T) {
	long := "Warning FailedScheduling " + strings.Repeat("x", 900)
	got := pickTroubleLine(long)
	if len(got) > 404 || !strings.HasSuffix(got, "…") {
		t.Fatalf("want a truncated line, got %d chars ending %q", len(got), got[max(0, len(got)-3):])
	}
}
