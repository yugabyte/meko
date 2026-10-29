package cli

import (
	"bytes"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/yugabyte/meko/backend/internal/cloudlink"
)

// The datapacks AgentK makes: long names, and descriptions that end in a repo's URL.
const (
	canned1, canned1Project = "adlc/proj/canned-agents/canned-agents", `AgentK project "canned-agents" · repo`
	canned2, canned2Project = "adlc/proj/canned-agents/canned-agents2", `AgentK project "canned-agents2" · repo`
	cannedRepo              = "https://github.com/yugabyte/canned-agents"
)

var cannedAgents = []nameDesc{
	{canned1, canned1Project + " " + cannedRepo},
	{canned2, canned2Project + " " + cannedRepo},
	{"default", "Created by Meko on first start."},
	{"team-runbooks", ""},
}

// cannedOneLine is cannedAgents with nothing wrapped.
const cannedOneLine = canned1 + "    " + canned1Project + " " + cannedRepo + "\n" +
	canned2 + "   " + canned2Project + " " + cannedRepo + "\n" +
	"default                                  Created by Meko on first start.\n" +
	"team-runbooks\n"

// nameDescTable fits names and descriptions to the terminal: descriptions wrap in their column,
// and names and words are never broken.
func TestNameDescTable(t *testing.T) {
	cases := []struct {
		name  string
		rows  []nameDesc
		width int
		want  string
	}{
		{"wide enough, one line each", cannedAgents, 140, cannedOneLine},
		{"descriptions wrap in their column", cannedAgents, 100, "" +
			`adlc/proj/canned-agents/canned-agents    AgentK project "canned-agents" · repo` + "\n" +
			"                                         https://github.com/yugabyte/canned-agents\n" +
			`adlc/proj/canned-agents/canned-agents2   AgentK project "canned-agents2" · repo` + "\n" +
			"                                         https://github.com/yugabyte/canned-agents\n" +
			"default                                  Created by Meko on first start.\n" +
			"team-runbooks\n"},
		// The URL is 41 wide: beside the names, descriptions would get 39, so the names get lines
		// of their own.
		{"descriptions get room for their widest word", cannedAgents, 80, "" +
			canned1 + "\n" +
			"                " + canned1Project + "\n" +
			"                " + cannedRepo + "\n" +
			canned2 + "\n" +
			"                " + canned2Project + "\n" +
			"                " + cannedRepo + "\n" +
			"default         Created by Meko on first start.\n" +
			"team-runbooks\n"},
		{"a name wider than the column gets a line of its own", []nameDesc{
			{"adlc/proj/canned-agents/canned-agents2", "mine"},
			{"adlc/proj/some-very-long-project-name/and-an-even-longer-datapack-name",
				`AgentK project "long-project" · repo https://github.com/yugabyte/long-repo`},
			{"default", "Created by Meko on first start."},
		}, 80, "" +
			"adlc/proj/canned-agents/canned-agents2   mine\n" +
			"adlc/proj/some-very-long-project-name/and-an-even-longer-datapack-name\n" +
			`                                         AgentK project "long-project" · repo` + "\n" +
			"                                         https://github.com/yugabyte/long-repo\n" +
			"default                                  Created by Meko on first start.\n"},
		{"whitespace collapses", []nameDesc{{"notes", "two\n  lines\tand  spaces "}}, 80,
			"notes   two lines and spaces\n"},
		{"a word wider than the terminal runs past it", []nameDesc{{"a", strings.Repeat("x", 50)}}, 40,
			"a\n" + strings.Repeat("x", 50) + "\n"},
		{"a word too wide for the column ends at the edge", []nameDesc{{"a", strings.Repeat("x", 38)}}, 40,
			"a\n  " + strings.Repeat("x", 38) + "\n"},
		{"not a terminal: nothing wraps", cannedAgents, 0, cannedOneLine},
		{"no rows", nil, 80, ""},
		// 文档 takes four cells, and the emoji two: the descriptions still line up.
		{"wide characters take two cells", []nameDesc{{"文档", "docs"}, {"notes", "📝 mine"}, {"x", "あい うえ"}}, 80,
			"文档    docs\n" +
				"notes   📝 mine\n" +
				"x       あい うえ\n"},
	}
	for _, c := range cases {
		got := nameDescTable(c.rows, c.width)
		if got != c.want {
			t.Errorf("%s:\n got:\n%s\nwant:\n%s", c.name, got, c.want)
		}
		widest := 0 // a word wider than the terminal runs past it
		for _, r := range c.rows {
			for _, w := range strings.Fields(r.description) {
				widest = max(widest, cells(w))
			}
		}
		for _, line := range strings.Split(strings.TrimSuffix(got, "\n"), "\n") {
			if strings.HasSuffix(line, " ") {
				t.Errorf("%s: a line ends in a space: %q", c.name, line)
			}
			if n := cells(line); c.width > 0 && n > max(c.width, widest) {
				t.Errorf("%s: a line is %d wide, past %d: %q", c.name, n, c.width, line)
			}
		}
	}
}

func TestWrapWords(t *testing.T) {
	cases := []struct {
		s     string
		width int
		want  []string
	}{
		{"", 10, nil},
		{"one two three", 7, []string{"one two", "three"}},
		{"one two three", 13, []string{"one two three"}},
		{"a https://example.com/long b", 6, []string{"a", "https://example.com/long", "b"}},
	}
	for _, c := range cases {
		if got := wrapWords(c.s, c.width); !reflect.DeepEqual(got, c.want) {
			t.Errorf("wrapWords(%q, %d) = %q, want %q", c.s, c.width, got, c.want)
		}
	}
}

// A writer that isn't a terminal, such as a pipe, the tests' buffer, or /dev/null (a character
// device, but not a terminal), gets no wrapping.
func TestTermWidthOffATerminal(t *testing.T) {
	if w := termWidth(&bytes.Buffer{}); w != 0 {
		t.Fatalf("termWidth of a buffer = %d", w)
	}
	null, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = null.Close() }()
	if w := termWidth(null); w != 0 {
		t.Fatalf("termWidth of %s = %d", os.DevNull, w)
	}
}

func TestCells(t *testing.T) {
	for s, want := range map[string]int{"abc": 3, "·": 1, "文档": 4, "📝": 2, "e\u0301": 1, "👩\u200d💻": 4} {
		if got := cells(s); got != want {
			t.Errorf("cells(%q) = %d, want %d", s, got, want)
		}
	}
}

// meko-cli datapacks sorts this Meko's datapacks and Meko Cloud's together by name, ignoring case,
// with this Meko's first on a tie, as --datapack picks it.
func TestDatapackList(t *testing.T) {
	local := []nameDesc{{"notes", "here"}, {"beta", ""}}
	cloud := []cloudlink.Datapack{{DatapackName: "Zeta"}, {DatapackName: "notes", Description: "Cloud's"},
		{DatapackName: "Alpha"}}
	want := []nameDesc{{"Alpha", ""}, {"beta", ""}, {"notes", "here"}, {"notes", "Cloud's"}, {"Zeta", ""}}
	if got := datapackList(local, cloud); !reflect.DeepEqual(got, want) {
		t.Fatalf("datapackList = %q, want %q", got, want)
	}
}
