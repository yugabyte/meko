package history

import (
	"fmt"
	"sort"
	"strings"
)

const (
	InvTotal     = "total_conservation"
	InvNegative  = "no_negative_balance"
	InvMonotonic = "monotonic_reads"
	InvReplay    = "acked_transfers_reflected"
	InvWriteSkew = "no_write_skew"
)

type CheckConfig struct {
	Initial      int64
	Total        int64
	Serializable bool
}

type Violation struct {
	Invariant string
	Seq       int64
	Detail    string
}

func (v Violation) String() string {
	return fmt.Sprintf("%s (op %d): %s", v.Invariant, v.Seq, v.Detail)
}

type Report struct {
	Ops        int64
	Reads      int64
	Committed  int64
	Failed     int64
	Unknown    int64
	Violations []Violation
}

func (r Report) Summary() string {
	parts := make([]string, 0, len(r.Violations))
	for _, v := range r.Violations {
		parts = append(parts, v.String())
	}
	return strings.Join(parts, "; ")
}

func Check(path string, cfg CheckConfig) (Report, error) {
	var rep Report
	acked := map[int]int64{}
	unknownUp := map[int]int64{}
	unknownDown := map[int]int64{}
	lastVer := map[string]map[int]int64{}
	var final []Account
	var finalSeq int64
	seen := map[string]bool{}

	add := func(inv string, seq int64, format string, args ...any) {
		if seen[inv] {
			return
		}
		seen[inv] = true
		rep.Violations = append(rep.Violations, Violation{Invariant: inv, Seq: seq, Detail: fmt.Sprintf(format, args...)})
	}

	err := Scan(path, func(op Op) error {
		rep.Ops++
		if op.Kind == KindRead {
			if op.State != StateOK {
				return nil
			}
			rep.Reads++
			checkSnapshot(op, cfg, add)
			checkMonotonic(op, lastVer, add)
			final, finalSeq = op.Accounts, op.Seq
			return nil
		}
		switch op.State {
		case StateOK:
			rep.Committed++
			acked[op.From] -= op.Amount
			acked[op.To] += op.Amount
		case StateUnknown:
			rep.Unknown++
			unknownDown[op.From] += op.Amount
			unknownUp[op.To] += op.Amount
		default:
			rep.Failed++
		}
		return nil
	})
	if err != nil {
		return rep, err
	}
	if final == nil {
		return rep, fmt.Errorf("history %s holds no successful snapshot read, so nothing could be checked against one", path)
	}
	checkReplay(final, finalSeq, cfg, acked, unknownUp, unknownDown, add)
	return rep, nil
}

type addFunc func(inv string, seq int64, format string, args ...any)

func checkSnapshot(op Op, cfg CheckConfig, add addFunc) {
	var total int64
	pairSum := map[int]int64{}
	for _, a := range op.Accounts {
		total += a.Bal
		if a.Pair >= 0 {
			pairSum[a.Pair] += a.Bal
			continue
		}
		if a.Bal < 0 {
			add(InvNegative, op.Seq, "session %s read account %d at balance %d: a guarded transfer may never take an account below zero",
				op.Session, a.ID, a.Bal)
		}
	}
	if total != cfg.Total {
		add(InvTotal, op.Seq, "session %s read a snapshot summing to %d, the bank was opened with %d: %d has been created or destroyed",
			op.Session, total, cfg.Total, total-cfg.Total)
	}
	if !cfg.Serializable {
		return
	}
	for _, pair := range sortedKeys(pairSum) {
		if pairSum[pair] < 0 {
			add(InvWriteSkew, op.Seq, "session %s read pair %d summing to %d: every drain of that pair was guarded on the pair sum, so under SERIALIZABLE one of two concurrent drains had to abort",
				op.Session, pair, pairSum[pair])
		}
	}
}

func checkMonotonic(op Op, lastVer map[string]map[int]int64, add addFunc) {
	prev, ok := lastVer[op.Session]
	if !ok {
		prev = map[int]int64{}
		lastVer[op.Session] = prev
	}
	for _, a := range op.Accounts {
		if was, seenBefore := prev[a.ID]; seenBefore && a.Ver < was {
			add(InvMonotonic, op.Seq, "session %s read account %d at version %d after having read version %d: the snapshot went backwards in time",
				op.Session, a.ID, a.Ver, was)
		}
		if a.Ver > prev[a.ID] {
			prev[a.ID] = a.Ver
		}
	}
}

func checkReplay(final []Account, seq int64, cfg CheckConfig, acked, up, down map[int]int64, add addFunc) {
	for _, a := range final {
		want := cfg.Initial + acked[a.ID]
		lo, hi := want-down[a.ID], want+up[a.ID]
		if a.Bal >= lo && a.Bal <= hi {
			continue
		}
		add(InvReplay, seq, "account %d holds %d, the acknowledged transfers add up to %d (allowing [%d, %d] for %d in flight when the window closed)",
			a.ID, a.Bal, want, lo, hi, down[a.ID]+up[a.ID])
	}
}

func sortedKeys(m map[int]int64) []int {
	out := make([]int, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Ints(out)
	return out
}
