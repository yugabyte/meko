package memory

import (
	"fmt"
	"strings"
	"time"

	"github.com/yugabyte/meko/src/meko/internal/store"
)

// The extraction prompt, written for Meko and kept short so each call's fixed cost stays small. It
// matches Meko Cloud's behavior: facts come from the user's messages only (the assistant's replies
// are context), and it only adds.
const extractBase = `You extract long-term memories from a conversation between a user and an AI assistant.

Output JSON only: {"memory": [{"text": "...", "turn": N}]}, where turn is the number of the new turn
the fact comes from. Return {"memory": []} when nothing is worth keeping, which is often.

Rules:
- Extract from the USER's messages in the new turns only. Assistant replies and earlier turns are
  context for understanding them: never extract what only the assistant said, unless the user
  confirms or adopts it ("yes, let's do that").
- Keep durable facts a future conversation would need: preferences, decisions and their reasons,
  plans, people and who owns what, constraints, recurring problems and what fixed them.
- Skip small talk, one-off requests, questions with no fact in them, and anything already in the
  existing memories, even reworded. Only add: never restate, update or contradict an existing memory
  here.
- Each memory is one self-contained sentence in the third person ("The user prefers ..."), with
  names and specifics kept, understandable without the conversation.
- Resolve relative dates ("yesterday", "next Friday") against the turn's own date, not today's.
- Never invent anything: every memory must be stated by the user in a new turn.`

// extractProfiles add a domain's rules to the base prompt.
var extractProfiles = map[string]string{
	store.ExtractionGeneral: "",
	store.ExtractionCoding: `This datapack holds coding conversations. Also:
- Keep: project conventions (package manager, test layout, commit and branch rules), the stack and
  environment (languages, versions, frameworks, how to build, run and deploy), architecture
  decisions and why, ownership and review process, constraints ("migrations are append-only"),
  recurring build or tooling problems and the fix that worked.
- Skip: what a particular function or file contains, line-level details of the current change,
  one-off errors that were just fixed, code the assistant wrote, and pasted files, logs or stack
  traces unless the user states what in them matters.
Example: the user pastes a stack trace and says "this always happens after a node upgrade, rm -rf
node_modules fixes it" -> "Upgrading Node breaks the build until node_modules is reinstalled."
Example: the user pastes a component and asks for a bug fix -> nothing.`,
}

// ExtractionProfiles is the profiles a datapack can pick.
func ExtractionProfiles() []string { return []string{store.ExtractionGeneral, store.ExtractionCoding} }

// extractSystem is the system prompt for a datapack's settings.
func extractSystem(d store.DatapackExtraction) string {
	parts := []string{extractBase}
	if p := extractProfiles[d.Profile]; p != "" {
		parts = append(parts, p)
	}
	if d.Instructions != "" {
		parts = append(parts, "The user's own instructions for this datapack:\n"+d.Instructions)
	}
	return strings.Join(parts, "\n\n")
}

// Sizes that bound a call: a turn's user text, the reply kept as context, and the context turns.
const (
	extractUserChars    = 4000
	extractReplyChars   = 300
	extractContextTurns = 4
)

// extractInput is the user message of a call: today's date, the existing memories to not repeat,
// earlier turns as context, and the numbered new turns.
func extractInput(existing []string, context, turns []store.ExtractionTurn, now time.Time) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Today: %s\n\n", now.UTC().Format("2006-01-02"))
	b.WriteString("Existing memories (do not repeat):\n")
	if len(existing) == 0 {
		b.WriteString("(none)\n")
	}
	for _, m := range existing {
		fmt.Fprintf(&b, "- %s\n", m)
	}
	if len(context) > 0 {
		b.WriteString("\nEarlier turns (context only):\n")
		for _, t := range context {
			fmt.Fprintf(&b, "User: %s\nAssistant: %s\n", clip(t.Input, extractReplyChars), clip(t.Output, extractReplyChars))
		}
	}
	b.WriteString("\nNew turns:\n")
	for i, t := range turns {
		fmt.Fprintf(&b, "[turn %d] (%s)\nUser: %s\nAssistant (context only): %s\n\n", i,
			turnDate(t).Format("2006-01-02"), clip(t.Input, extractUserChars), clip(t.Output, extractReplyChars))
	}
	return b.String()
}

// clip shortens text over n characters to its start and end, saying how much was cut: a large paste
// keeps what usually matters (what it is, and how it ends) without its bulk.
func clip(text string, n int) string {
	r := []rune(strings.TrimSpace(text))
	if len(r) <= n {
		return string(r)
	}
	head, tail := n*3/4, n/4
	return fmt.Sprintf("%s [… %d characters cut …] %s", string(r[:head]), len(r)-head-tail, string(r[len(r)-tail:]))
}

// trivial is a user message with nothing to remember: a few words of go-ahead or thanks.
func trivial(input string) bool {
	return len([]rune(strings.TrimSpace(input))) < 12
}

// turnDate is when a turn was said: its session date when a replayed transcript gives one.
func turnDate(t store.ExtractionTurn) time.Time {
	if !t.SessionDate.IsZero() {
		return t.SessionDate
	}
	return t.CreatedAt.UTC()
}
