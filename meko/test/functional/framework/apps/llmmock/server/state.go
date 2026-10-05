package server

import (
	"slices"
	"strings"
	"sync"
	"time"
)

const maxCalls = 5000

type fixture struct {
	ID       string    `json:"id"`
	Text     string    `json:"text"`
	Vector   []float64 `json:"vector"`
	Contains bool      `json:"contains,omitempty"`
}

type toolCall struct {
	Name      string `json:"name"`
	Arguments any    `json:"arguments,omitempty"`
}

type script struct {
	ID        string     `json:"id"`
	Route     string     `json:"route,omitempty"`
	Match     string     `json:"match,omitempty"`
	System    string     `json:"system,omitempty"`
	Reply     string     `json:"reply,omitempty"`
	ToolCalls []toolCall `json:"tool_calls,omitempty"`
	Times     int        `json:"times,omitempty"`
	Remaining *int       `json:"remaining"`
}

type fault struct {
	ID         string `json:"id"`
	Route      string `json:"route,omitempty"`
	Match      string `json:"match,omitempty"`
	Kind       string `json:"kind"`
	Status     int    `json:"status,omitempty"`
	RetryAfter int    `json:"retry_after,omitempty"`
	DelayMS    int    `json:"delay_ms,omitempty"`
	HangMS     int    `json:"hang_ms,omitempty"`
	Times      int    `json:"times,omitempty"`
	Remaining  *int   `json:"remaining"`
}

type message struct {
	Role string `json:"role"`
	Text string `json:"text"`
}

type call struct {
	Seq        int       `json:"seq"`
	Time       float64   `json:"time"`
	Status     int       `json:"status"`
	Fault      string    `json:"fault"`
	Answer     string    `json:"answer"`
	Route      string    `json:"route"`
	API        string    `json:"api"`
	Model      string    `json:"model"`
	Path       string    `json:"path"`
	Stream     bool      `json:"stream"`
	Text       string    `json:"text"`
	Labels     []string  `json:"labels"`
	Dims       int       `json:"dims,omitempty"`
	DurationMS int       `json:"duration_ms,omitempty"`
	Inputs     []string  `json:"inputs,omitempty"`
	Messages   []message `json:"messages,omitempty"`
	Tools      []string  `json:"tools,omitempty"`
}

type state struct {
	mu       sync.Mutex
	fixtures []*fixture
	scripts  []*script
	faults   []*fault
	calls    []*call
	seq      int
}

func (st *state) reset() {
	st.mu.Lock()
	defer st.mu.Unlock()
	st.fixtures, st.scripts, st.faults, st.calls, st.seq = nil, nil, nil, nil, 0
}

func (st *state) record(c call) *call {
	st.mu.Lock()
	defer st.mu.Unlock()
	st.seq++
	c.Seq = st.seq
	c.Time = float64(time.Now().UnixNano()) / 1e9
	st.calls = append(st.calls, &c)
	if len(st.calls) > maxCalls {
		st.calls = slices.Clone(st.calls[len(st.calls)-maxCalls:])
	}
	return &c
}

func (st *state) update(c *call, fn func(*call)) {
	st.mu.Lock()
	defer st.mu.Unlock()
	fn(c)
}

func (st *state) fixtureFor(text string) *fixture {
	st.mu.Lock()
	defer st.mu.Unlock()
	for _, fx := range slices.Backward(st.fixtures) {
		if fx.Text == text || (fx.Contains && strings.Contains(text, fx.Text)) {
			return fx
		}
	}
	return nil
}

func takeRemaining(left **int) bool {
	if *left == nil {
		return true
	}
	if **left <= 0 {
		return false
	}
	n := **left - 1
	*left = &n
	return true
}

func (st *state) takeScript(labels []string, prompt, system string, hasTools, lastIsTool bool) *script {
	st.mu.Lock()
	defer st.mu.Unlock()
	for _, sc := range st.scripts {
		if sc.Route != "" && !slices.Contains(labels, sc.Route) {
			continue
		}
		if sc.Match != "" && !strings.Contains(prompt, sc.Match) {
			continue
		}
		if sc.System != "" && !strings.Contains(system, sc.System) {
			continue
		}
		if len(sc.ToolCalls) > 0 && (lastIsTool || !hasTools) {
			continue
		}
		if !takeRemaining(&sc.Remaining) {
			continue
		}
		cp := *sc
		return &cp
	}
	return nil
}

func (st *state) takeFaults(labels []string, text string) []fault {
	st.mu.Lock()
	defer st.mu.Unlock()
	var out []fault
	for _, f := range st.faults {
		if f.Route != "" && !slices.Contains(labels, f.Route) {
			continue
		}
		if f.Match != "" && !strings.Contains(text, f.Match) {
			continue
		}
		if !takeRemaining(&f.Remaining) {
			continue
		}
		out = append(out, *f)
	}
	return out
}

func remaining(times int) *int {
	if times > 0 {
		return &times
	}
	return nil
}
