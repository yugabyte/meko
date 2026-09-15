// Package history records a workload's operations to a JSONL file and checks the record offline.
package history

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"sync"
)

const (
	KindTransfer = "transfer"
	KindSkew     = "skew"
	KindRead     = "read"

	StateOK      = "ok"
	StateFail    = "fail"
	StateUnknown = "unknown"
)

type Account struct {
	ID   int   `json:"id"`
	Pair int   `json:"pair"`
	Bal  int64 `json:"bal"`
	Ver  int64 `json:"ver"`
}

// Op.State is what the CLIENT knows: StateUnknown is a COMMIT that went unanswered.
type Op struct {
	Seq      int64     `json:"seq"`
	Session  string    `json:"session"`
	Kind     string    `json:"kind"`
	State    string    `json:"state"`
	InvokeNS int64     `json:"invoke_ns"`
	DoneNS   int64     `json:"done_ns"`
	From     int       `json:"from,omitempty"`
	To       int       `json:"to,omitempty"`
	Amount   int64     `json:"amount,omitempty"`
	LSN      string    `json:"lsn,omitempty"`
	Accounts []Account `json:"accounts,omitempty"`
	Err      string    `json:"err,omitempty"`
}

type Recorder struct {
	mu         sync.Mutex
	path       string
	f          *os.File
	w          *bufio.Writer
	flushEvery int
	sinceFlush int
	seq        int64
}

func NewRecorder(path string, flushEvery int) (*Recorder, error) {
	f, err := os.Create(path)
	if err != nil {
		return nil, fmt.Errorf("create history file %s: %w", path, err)
	}
	if flushEvery <= 0 {
		flushEvery = 1
	}
	return &Recorder{path: path, f: f, w: bufio.NewWriterSize(f, 64*1024), flushEvery: flushEvery}, nil
}

func (r *Recorder) Append(op Op) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.w == nil {
		return fmt.Errorf("history %s is closed", r.path)
	}
	r.seq++
	op.Seq = r.seq
	line, err := json.Marshal(op)
	if err != nil {
		return fmt.Errorf("encode op %d: %w", op.Seq, err)
	}
	if _, err := r.w.Write(append(line, '\n')); err != nil {
		return fmt.Errorf("write op %d: %w", op.Seq, err)
	}
	r.sinceFlush++
	if r.sinceFlush >= r.flushEvery {
		r.sinceFlush = 0
		return r.w.Flush()
	}
	return nil
}

func (r *Recorder) Count() int64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.seq
}

func (r *Recorder) Path() string { return r.path }

func (r *Recorder) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.w == nil {
		return nil
	}
	err := r.w.Flush()
	r.w = nil
	if cerr := r.f.Close(); err == nil {
		err = cerr
	}
	return err
}

func Scan(path string, fn func(Op) error) error {
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("open history %s: %w", path, err)
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var op Op
		if err := json.Unmarshal(line, &op); err != nil {
			return fmt.Errorf("decode history %s: %w", path, err)
		}
		if err := fn(op); err != nil {
			return err
		}
	}
	return sc.Err()
}
