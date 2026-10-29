package rest_test

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/yugabyte/meko/backend/internal/memory"
	"github.com/yugabyte/meko/backend/internal/testenv"
)

// fakeAnswerer answers with reply, recording each call's input.
type fakeAnswerer struct {
	mu     sync.Mutex
	inputs []string
	reply  memory.SearchAnswer
	err    error
}

func (a *fakeAnswerer) Answer(_ context.Context, _, input string) (memory.SearchAnswer, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.inputs = append(a.inputs, input)
	return a.reply, a.err
}

// Search answers: off until the admin sets them up; then a question and its search's passages go to
// the harness numbered, at most 10, and only citations of passages it was given come back. A failed
// call says so without the text.
func TestSearchAnswers(t *testing.T) {
	a := &fakeAnswerer{}
	e := testenv.Start(t, testenv.Options{Answerer: a})
	const settings = "/settings/answers"
	path := "/datapacks/" + e.DatapackID + "/answer"
	if st := obj(okData(t, e, "GET", settings, nil, 200)); st["enabled"] != false {
		t.Fatalf("on by default: %v", st)
	}
	ask := map[string]any{"query": "when do releases ship?",
		"passages": []map[string]string{{"source": "memory", "id": "m1", "text": "releases ship on Tuesdays"}}}
	wantErr(t, e, "POST", path, ask, 409, "ANSWERS_OFF")

	wantErr(t, e, "PUT", settings, map[string]any{"harness": "claude"}, 403, "FORBIDDEN")
	if r := asAdmin(t, e, settings, map[string]any{"harness": "codex"}); r.Status != 400 {
		t.Errorf("an unknown harness: %+v", r)
	}
	r := asAdmin(t, e, settings, map[string]any{"harness": "claude"})
	if r.Status != 200 || obj(r.Data)["enabled"] != true || obj(r.Data)["model"] != "haiku" || len(a.inputs) != 1 {
		t.Fatalf("set up: %+v, %d test calls", r, len(a.inputs))
	}

	many := make([]map[string]string, 0, 12)
	for i := range 12 {
		many = append(many, map[string]string{"source": "knowledge_base", "id": fmt.Sprint(i),
			"text": fmt.Sprintf("passage %d", i)})
	}
	a.reply = memory.SearchAnswer{Answer: "On Tuesdays [1].", Cited: []int{1, 1, 11, 0}}
	got := obj(okData(t, e, "POST", path, map[string]any{"query": "when?", "passages": many}, 200))
	if got["answer"] != "On Tuesdays [1]." || fmt.Sprint(got["cited"]) != "[1]" || got["model"] != "haiku" {
		t.Errorf("answer %v", got)
	}
	in := a.inputs[len(a.inputs)-1]
	if !strings.Contains(in, "Question: when?") || !strings.Contains(in, "[10] (knowledge_base) passage 9") ||
		strings.Contains(in, "passage 10") {
		t.Errorf("input:\n%s", in)
	}

	wantErr(t, e, "POST", path, map[string]any{"query": " "}, 400, "VALIDATION_ERROR")
	wantErr(t, e, "POST", "/datapacks/00000000-0000-0000-0000-000000000000/answer", ask, 404, "NOT_FOUND")
	a.err = fmt.Errorf("%w: claude: Not logged in", memory.ErrAnswer)
	wantErr(t, e, "POST", path, ask, 502, "ANSWER_FAILED")

	if r := asAdmin(t, e, settings, map[string]any{"harness": ""}); r.Status != 200 ||
		obj(r.Data)["enabled"] != false {
		t.Errorf("off: %+v", r)
	}
}
