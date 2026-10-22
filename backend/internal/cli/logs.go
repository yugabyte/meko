package cli

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/yugabyte/meko/backend/internal/paths"
)

type logsOptions struct {
	follow                        bool
	lines                         int
	server, memoryEngine, service bool
	json                          bool
}

// logSource is one log file and the label its lines get.
type logSource struct{ label, path string }

// followPoll is how often logs -f looks for new lines.
var followPoll = 500 * time.Millisecond

// runLogs prints the last lines of the server's and the memory engine's logs, merged by time, and with -f
// keeps printing new ones, following a file across its rotation.
func runLogs(l paths.Layout, o logsOptions, stdout io.Writer) int {
	var srcs []logSource
	if o.server {
		srcs = append(srcs, logSource{"meko", l.Log})
	}
	if o.memoryEngine {
		srcs = append(srcs, logSource{"memory", l.MemoryEngineLog})
	}
	if o.service {
		srcs = append(srcs, logSource{"service", l.ServiceLog})
	}
	if len(srcs) == 0 {
		srcs = []logSource{{"meko", l.Log}, {"memory", l.MemoryEngineLog}}
	}
	if o.lines < 0 {
		o.lines = 0
	}
	label := len(srcs) > 1
	var all []logLine
	found := false
	for _, s := range srcs {
		b, err := os.ReadFile(s.path) //nolint:gosec // G304: the data folder's own logs
		if err != nil {
			continue
		}
		found = true
		all = append(all, parseLines(s.label, b)...)
	}
	if !found && !o.follow {
		fmt.Fprintf(stdout, "No logs yet in %s\n", l.LogDir)
		return exitOK
	}
	sort.SliceStable(all, func(i, j int) bool { return all[i].at.Before(all[j].at) })
	if len(all) > o.lines {
		all = all[len(all)-o.lines:]
	}
	for _, ln := range all {
		fmt.Fprintln(stdout, ln.format(label, o.json))
	}
	if !o.follow {
		return exitOK
	}
	newFollower(srcs, label, o.json).run(SignalContext(), stdout)
	return exitOK
}

// logLine is one line of a log, with its time when it has one (a JSON line's "time").
type logLine struct {
	source string
	raw    string
	at     time.Time
	fields map[string]any
}

func parseLines(source string, b []byte) []logLine {
	var out []logLine
	var last time.Time
	sc := bufio.NewScanner(bytes.NewReader(b))
	sc.Buffer(make([]byte, 64*1024), 4*1024*1024)
	for sc.Scan() {
		ln := parseLine(source, sc.Text())
		if ln.at.IsZero() {
			ln.at = last // a plain line sorts after the timed line before it
		}
		last = ln.at
		out = append(out, ln)
	}
	return out
}

func parseLine(source, raw string) logLine {
	ln := logLine{source: source, raw: raw}
	if !strings.HasPrefix(raw, "{") {
		return ln
	}
	var m map[string]any
	if json.Unmarshal([]byte(raw), &m) != nil {
		return ln
	}
	ln.fields = m
	if s, ok := m["time"].(string); ok {
		if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
			ln.at = t
		}
	}
	return ln
}

// format renders a JSON line as "time LEVEL msg key=value ...", in local time.
func (ln logLine) format(label, raw bool) string {
	prefix := ""
	if label {
		prefix = fmt.Sprintf("%-7s| ", ln.source)
	}
	if raw || ln.fields == nil {
		return prefix + ln.raw
	}
	var b strings.Builder
	if !ln.at.IsZero() {
		b.WriteString(ln.at.Local().Format("2006-01-02 15:04:05.000 "))
	}
	level, _ := ln.fields["level"].(string)
	if level == "" {
		level = "INFO"
	}
	fmt.Fprintf(&b, "%-5s %v", level, ln.fields["msg"])
	keys := make([]string, 0, len(ln.fields))
	for k := range ln.fields {
		if k != "time" && k != "level" && k != "msg" {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	for _, k := range keys {
		v, _ := json.Marshal(ln.fields[k])
		s := string(v)
		if str, ok := ln.fields[k].(string); ok && !strings.ContainsAny(str, " \t\"=") && str != "" {
			s = str
		}
		fmt.Fprintf(&b, " %s=%s", k, s)
	}
	return prefix + b.String()
}

// follower prints lines added to log files from when it's made on. A file that shrinks or is
// replaced (rotated) is read again from its start.
type follower struct {
	srcs       []logSource
	label, raw bool
	st         []followState
}

type followState struct {
	ino  uint64
	off  int64
	part string // an unfinished last line, waiting for its end
}

func newFollower(srcs []logSource, label, raw bool) *follower {
	f := &follower{srcs: srcs, label: label, raw: raw, st: make([]followState, len(srcs))}
	for i, s := range srcs {
		if fi, err := os.Stat(s.path); err == nil {
			f.st[i] = followState{ino: inode(fi), off: fi.Size()}
		}
	}
	return f
}

// run prints new lines until ctx ends.
func (f *follower) run(ctx context.Context, stdout io.Writer) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(followPoll):
		}
		for i := range f.srcs {
			f.poll(i, stdout)
		}
	}
}

func (f *follower) poll(i int, stdout io.Writer) {
	s, st := f.srcs[i], &f.st[i]
	fi, err := os.Stat(s.path)
	if err != nil {
		return
	}
	if inode(fi) != st.ino || fi.Size() < st.off {
		*st = followState{ino: inode(fi)}
	}
	if fi.Size() == st.off {
		return
	}
	file, err := os.Open(s.path) //nolint:gosec // G304: the data folder's own logs
	if err != nil {
		return
	}
	b, err := io.ReadAll(io.NewSectionReader(file, st.off, fi.Size()-st.off))
	_ = file.Close()
	if err != nil {
		return
	}
	st.off += int64(len(b))
	lines := strings.Split(st.part+string(b), "\n")
	st.part = lines[len(lines)-1]
	for _, line := range lines[:len(lines)-1] {
		fmt.Fprintln(stdout, parseLine(s.label, line).format(f.label, f.raw))
	}
}
