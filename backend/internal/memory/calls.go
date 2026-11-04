package memory

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/yugabyte/meko/backend/internal/store"
)

// CallRetention is how long the call log keeps a call.
const CallRetention = 90 * 24 * time.Hour

// CallContentsRetention is how long a call's contents are kept, while Settings' "Keep call contents"
// is on: long enough to look into a recent session, short enough that the replies (some of them
// large) don't pile up.
const CallContentsRetention = 7 * 24 * time.Hour

// CallContentsMax is how much of a call's arguments, and of its reply, is kept.
const CallContentsMax = 1 << 20

// keyKeepCallContents is the settings key of "Keep call contents": "1" on, anything else off.
const keyKeepCallContents = "call_contents.keep"

// pruneEvery is how often RunCallPruner prunes after its first run.
var pruneEvery = 24 * time.Hour

// RecordCall adds a call to the call log, and what it sent (its raw arguments) and got back (its
// reply's text) while Settings' "Keep call contents" is on. The call already happened, so a failure
// to record it is logged, not returned.
func (s *Service) RecordCall(ctx context.Context, c store.Call, sent json.RawMessage, reply string) {
	// The caller's request may be over (its context canceled) by the time it's recorded.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	var contents *store.CallContents
	if s.keepCallContents(ctx) {
		contents = callContents(sent, reply)
	}
	if err := s.st.AddCall(ctx, c, contents); err != nil && s.log != nil {
		s.log.Warn("couldn't record a call", "tool", c.Tool, "error", err)
	}
}

// keepCallContents says whether "Keep call contents" is on; off when it can't be read.
func (s *Service) keepCallContents(ctx context.Context) bool {
	v, _, err := s.st.Setting(ctx, keyKeepCallContents)
	return err == nil && v == "1"
}

// callContents is what's kept of a call: its arguments with any *_base64 one (a file's or an
// artifact's bytes) replaced by its size, and both sides cut at CallContentsMax.
func callContents(sent json.RawMessage, reply string) *store.CallContents {
	args := string(sent)
	var a map[string]any
	if json.Unmarshal(sent, &a) == nil {
		changed := false
		for k, v := range a {
			if b64, ok := v.(string); ok && strings.HasSuffix(k, "_base64") {
				a[k] = fmt.Sprintf("<%d bytes>", len(b64)*3/4)
				changed = true
			}
		}
		var b strings.Builder
		enc := json.NewEncoder(&b)
		enc.SetEscapeHTML(false) // "<12 bytes>" reads as it is
		if changed && enc.Encode(a) == nil {
			args = strings.TrimSuffix(b.String(), "\n")
		}
	}
	if args == "" {
		args = "{}"
	}
	cc := &store.CallContents{}
	var cutArgs, cutReply bool
	cc.Args, cutArgs = cut(args, CallContentsMax)
	cc.Reply, cutReply = cut(reply, CallContentsMax)
	cc.Cut = cutArgs || cutReply
	return cc
}

// cut is s cut to at most n bytes, at a character's boundary, and whether it was cut.
func cut(s string, n int) (string, bool) {
	if len(s) <= n {
		return s, false
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n], true
}

// CallContentsState is "Keep call contents": whether it's on, how long contents are kept, and how
// many calls' contents are kept now and their size.
type CallContentsState struct {
	Keep          bool  `json:"keep"`
	RetentionDays int   `json:"retention_days"`
	Calls         int64 `json:"calls"`
	Bytes         int64 `json:"bytes"`
}

// CallContentsState reads "Keep call contents".
func (s *Service) CallContentsState(ctx context.Context) (CallContentsState, error) {
	n, bytes, err := s.st.CallContentsSize(ctx)
	if err != nil {
		return CallContentsState{}, err
	}
	return CallContentsState{Keep: s.keepCallContents(ctx), RetentionDays: int(CallContentsRetention / (24 * time.Hour)),
		Calls: n, Bytes: bytes}, nil
}

// SetKeepCallContents turns "Keep call contents" on or off. Turning it off keeps what's kept until
// it expires or is cleared.
func (s *Service) SetKeepCallContents(ctx context.Context, keep bool) error {
	v := "0"
	if keep {
		v = "1"
	}
	return s.st.SetSetting(ctx, keyKeepCallContents, v)
}

// ClearCallContents deletes every call's kept contents; the calls stay in the call log.
func (s *Service) ClearCallContents(ctx context.Context) error {
	_, err := s.st.ClearCallContents(ctx)
	return err
}

// ListCalls is a page of the call log, newest first (see store.ListCalls).
func (s *Service) ListCalls(ctx context.Context, f store.CallFilter, limit int) ([]store.Call, bool, error) {
	return s.st.ListCalls(ctx, f, limit)
}

// Call is one call of the call log and its contents, when they're kept; ErrNotFound when there's none.
func (s *Service) Call(ctx context.Context, id int64) (store.Call, *store.CallContents, error) {
	c, cc, ok, err := s.st.CallByID(ctx, id)
	if err == nil && !ok {
		err = ErrNotFound
	}
	return c, cc, err
}

// RunCallPruner deletes calls older than CallRetention now and then every day, until ctx ends, and
// Context Gaps' asks with them, and calls' contents older than CallContentsRetention.
func (s *Service) RunCallPruner(ctx context.Context) {
	t := time.NewTicker(pruneEvery)
	defer t.Stop()
	for {
		before := time.Now().Add(-CallRetention)
		n, err := s.st.PruneCalls(ctx, before)
		switch {
		case err != nil && ctx.Err() == nil && s.log != nil:
			s.log.Warn("couldn't prune the call log", "error", err)
		case n > 0 && s.log != nil:
			s.log.Info("pruned the call log", "calls", n)
		}
		n, err = s.st.PruneCallContents(ctx, time.Now().Add(-CallContentsRetention))
		switch {
		case err != nil && ctx.Err() == nil && s.log != nil:
			s.log.Warn("couldn't prune the calls' contents", "error", err)
		case n > 0 && s.log != nil:
			s.log.Info("pruned the calls' contents", "calls", n)
		}
		n, err = s.st.PruneAsks(ctx, before)
		switch {
		case err != nil && ctx.Err() == nil && s.log != nil:
			s.log.Warn("couldn't prune Context Gaps", "error", err)
		case n > 0 && s.log != nil:
			s.log.Info("pruned Context Gaps", "asks", n)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}
