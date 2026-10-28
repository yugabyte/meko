package memory

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/yugabyte/meko/backend/internal/store"
)

// The memory extractor pulls memories out of conversation turns through the user's own agent CLI
// (Claude Code today), run headless with no tools, on a fixed interval: each run sends every
// conversation's waiting turns in one call. It's off until the CLI's admin sets it up.

// Harnesses the extractor can run, by name, and the program each one runs.
var harnessPrograms = map[string]string{"claude": "claude"}

// Extractor defaults and limits.
const (
	DefaultExtractInterval = 5 * time.Minute
	MinExtractInterval     = time.Minute
	defaultClaudeModel     = "haiku"
	extractTimeout         = 5 * time.Minute
	extractConversations   = 50   // conversations a run takes
	extractTurns           = 30   // turns of one conversation a call takes
	extractExisting        = 20   // existing memories a call is shown
	extractDuplicate       = 0.9  // a new fact this close to an existing memory is dropped
	extractQueryChars      = 2000 // of the turns' text, to find the existing memories
)

// Settings keys.
const (
	keyExtractHarness   = "memory_extractor.harness"
	keyExtractCommand   = "memory_extractor.command"
	keyExtractModel     = "memory_extractor.model"
	keyExtractInterval  = "memory_extractor.interval_seconds"
	keyExtractLastRun   = "memory_extractor.last_run_at"
	keyExtractLastError = "memory_extractor.last_error"
	keyExtractTotal     = "memory_extractor.extracted"
)

// ErrExtractorOff is a run asked for while no harness is set up.
var ErrExtractorOff = errors.New("the memory extractor is off: set it up with meko memory-extractor use claude")

// ExtractorSettings is how the extractor runs: Harness "" is off.
type ExtractorSettings struct {
	Harness  string        `json:"harness"`
	Command  string        `json:"command"` // the harness's program, an absolute path
	Model    string        `json:"model"`
	Interval time.Duration `json:"-"`
}

// ExtractorStatus is the extractor's settings and how its runs have gone.
type ExtractorStatus struct {
	ExtractorSettings
	Enabled         bool   `json:"enabled"`
	IntervalSeconds int    `json:"interval_seconds"`
	Queued          int    `json:"queued"`
	LastRunAt       string `json:"last_run_at,omitempty"`
	LastError       string `json:"last_error,omitempty"`
	Extracted       int    `json:"extracted"`
}

// ExtractRun is what one run did.
type ExtractRun struct {
	Conversations int `json:"conversations"`
	Turns         int `json:"turns"`
	Memories      int `json:"memories"`
}

// ExtractedFact is one memory a harness returned, with the number of the new turn it came from.
type ExtractedFact struct {
	Text string `json:"text"`
	Turn int    `json:"turn"`
}

// Harness runs one extraction call.
type Harness interface {
	Extract(ctx context.Context, system, input string) ([]ExtractedFact, error)
}

// extractSchema is the reply every call must have.
const extractSchema = `{"type":"object","properties":{"memory":{"type":"array","items":{"type":"object",` +
	`"properties":{"text":{"type":"string"},"turn":{"type":"integer"}},"required":["text","turn"]}}},` +
	`"required":["memory"]}`

// claudeHarness is Claude Code run headless (claude -p): no tools, no MCP servers, no slash commands,
// no session saved, and only the local settings of an empty folder, so the user's own hooks (Meko's
// capture among them) don't run inside it. --bare would skip those too, but also the saved login.
type claudeHarness struct{ command, model string }

func (h claudeHarness) Extract(ctx context.Context, system, input string) ([]ExtractedFact, error) {
	dir, err := os.MkdirTemp("", "meko-extract-")
	if err != nil {
		return nil, err
	}
	defer func() { _ = os.RemoveAll(dir) }()
	ctx, cancel := context.WithTimeout(ctx, extractTimeout)
	defer cancel()
	//nolint:gosec // G204: command is an absolute path the admin set up, checked to be the harness's program
	cmd := exec.CommandContext(ctx, h.command, "-p", "--setting-sources", "local", "--tools", "",
		"--strict-mcp-config", "--no-session-persistence", "--disable-slash-commands", "--model", h.model,
		"--system-prompt", system, "--output-format", "json", "--json-schema", extractSchema)
	cmd.Dir = dir
	cmd.Stdin = strings.NewReader(input)
	var out, errOut bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errOut
	runErr := cmd.Run()
	var reply struct {
		IsError    bool   `json:"is_error"`
		Result     string `json:"result"`
		Structured *struct {
			Memory []ExtractedFact `json:"memory"`
		} `json:"structured_output"`
	}
	if err := json.Unmarshal(out.Bytes(), &reply); err != nil {
		if runErr != nil {
			return nil, fmt.Errorf("claude: %v: %s", runErr, clip(errOut.String()+out.String(), 300))
		}
		return nil, fmt.Errorf("claude replied with something other than JSON: %s", clip(out.String(), 300))
	}
	if reply.IsError || reply.Structured == nil {
		return nil, fmt.Errorf("claude: %s", clip(reply.Result, 300))
	}
	return reply.Structured.Memory, nil
}

// harness is the harness for settings: a test's, or the named one.
func (s *Service) harness(set ExtractorSettings) Harness {
	if s.testHarness != nil {
		return s.testHarness
	}
	return claudeHarness{command: set.Command, model: set.Model}
}

// ExtractorSettings reads the extractor's settings, defaults filled in.
func (s *Service) ExtractorSettings(ctx context.Context) (ExtractorSettings, error) {
	var set ExtractorSettings
	for key, dst := range map[string]*string{keyExtractHarness: &set.Harness, keyExtractCommand: &set.Command,
		keyExtractModel: &set.Model} {
		v, _, err := s.st.Setting(ctx, key)
		if err != nil {
			return set, err
		}
		*dst = v
	}
	set.Interval = DefaultExtractInterval
	if v, ok, err := s.st.Setting(ctx, keyExtractInterval); err != nil {
		return set, err
	} else if n, perr := strconv.Atoi(v); ok && perr == nil {
		set.Interval = max(time.Duration(n)*time.Second, MinExtractInterval)
	}
	return set, nil
}

// SetExtractor turns the extractor on with a harness, after checking that one call through it works
// (installed, signed in), or off when Harness is "".
func (s *Service) SetExtractor(ctx context.Context, set ExtractorSettings) error {
	if set.Harness == "" {
		for _, k := range []string{keyExtractHarness, keyExtractCommand, keyExtractModel, keyExtractLastError} {
			if err := s.st.DeleteSetting(ctx, k); err != nil {
				return err
			}
		}
		s.kickExtractor()
		return nil
	}
	program, ok := harnessPrograms[set.Harness]
	if !ok {
		return fmt.Errorf("%w: harness must be claude (codex and cursor come later)", ErrBadRequest)
	}
	if s.testHarness == nil {
		if !filepath.IsAbs(set.Command) || filepath.Base(set.Command) != program {
			return fmt.Errorf("%w: command must be the absolute path of %s", ErrBadRequest, program)
		}
		if fi, err := os.Stat(set.Command); err != nil || fi.IsDir() || fi.Mode()&0o111 == 0 {
			return fmt.Errorf("%w: %s isn't a program that can run", ErrBadRequest, set.Command)
		}
	}
	if set.Model == "" {
		set.Model = defaultClaudeModel
	}
	if set.Interval == 0 {
		set.Interval = DefaultExtractInterval
	}
	if set.Interval < MinExtractInterval {
		return fmt.Errorf("%w: the interval must be at least %s", ErrBadRequest, MinExtractInterval)
	}
	if _, err := s.harness(set).Extract(ctx, extractBase, "Today: 2026-01-01\n\nNew turns:\n(none)\n"); err != nil {
		return fmt.Errorf("%w: a test call failed (is %s installed and signed in?): %v", ErrBadRequest, set.Harness, err)
	}
	for k, v := range map[string]string{keyExtractHarness: set.Harness, keyExtractCommand: set.Command,
		keyExtractModel: set.Model, keyExtractInterval: strconv.Itoa(int(set.Interval / time.Second))} {
		if err := s.st.SetSetting(ctx, k, v); err != nil {
			return err
		}
	}
	if err := s.st.DeleteSetting(ctx, keyExtractLastError); err != nil {
		return err
	}
	s.kickExtractor()
	return nil
}

// ExtractorStatus is the extractor's settings and its runs so far.
func (s *Service) ExtractorStatus(ctx context.Context) (ExtractorStatus, error) {
	set, err := s.ExtractorSettings(ctx)
	if err != nil {
		return ExtractorStatus{}, err
	}
	out := ExtractorStatus{ExtractorSettings: set, Enabled: set.Harness != "",
		IntervalSeconds: int(set.Interval / time.Second)}
	if out.Queued, err = s.st.QueuedCount(ctx); err != nil {
		return out, err
	}
	out.LastRunAt, _, _ = s.st.Setting(ctx, keyExtractLastRun)
	out.LastError, _, _ = s.st.Setting(ctx, keyExtractLastError)
	total, _, _ := s.st.Setting(ctx, keyExtractTotal)
	out.Extracted, _ = strconv.Atoi(total)
	return out, nil
}

// queueExtraction says whether a new turn in a datapack is queued for extraction: the extractor is
// on and the datapack hasn't opted out.
func (s *Service) queueExtraction(ctx context.Context, datapack string) (bool, error) {
	if datapack == "" {
		return false, nil
	}
	harness, _, err := s.st.Setting(ctx, keyExtractHarness)
	if err != nil || harness == "" {
		return false, err
	}
	d, err := s.st.GetDatapackExtraction(ctx, datapack)
	return !d.OptOut, err
}

func (s *Service) kickExtractor() {
	select {
	case s.extractKick <- struct{}{}:
	default:
	}
}

// RunExtractor runs the extractor every interval, and at once when its settings change, until ctx
// ends. serve runs it.
func (s *Service) RunExtractor(ctx context.Context) {
	for {
		set, err := s.ExtractorSettings(ctx)
		if err != nil {
			set.Interval = DefaultExtractInterval
		}
		select {
		case <-ctx.Done():
			return
		case <-s.extractKick:
		case <-time.After(set.Interval):
		}
		if _, err := s.ExtractPending(ctx); err != nil && !errors.Is(err, ErrExtractorOff) && ctx.Err() == nil {
			s.log.Warn("memory extraction failed; the turns stay queued", "error", err)
		}
	}
}

// ExtractPending extracts every waiting turn now, a call per conversation. A failed call leaves its
// turns queued, records why, and ends the run: the next one tries again.
func (s *Service) ExtractPending(ctx context.Context) (ExtractRun, error) {
	s.extractMu.Lock()
	defer s.extractMu.Unlock()
	var run ExtractRun
	set, err := s.ExtractorSettings(ctx)
	if err != nil {
		return run, err
	}
	if set.Harness == "" {
		return run, ErrExtractorOff
	}
	convs, err := s.st.QueuedConversations(ctx, extractConversations)
	if err != nil {
		return run, err
	}
	for _, c := range convs {
		n, turns, err := s.extractConversation(ctx, s.harness(set), c)
		run.Memories += n
		if turns > 0 {
			run.Conversations++
			run.Turns += turns
		}
		if err != nil {
			_ = s.st.SetSetting(ctx, keyExtractLastError, err.Error())
			s.recordRun(ctx, run.Memories)
			return run, err
		}
	}
	_ = s.st.DeleteSetting(ctx, keyExtractLastError)
	s.recordRun(ctx, run.Memories)
	return run, nil
}

// recordRun notes when the extractor last ran and adds what it saved to the total.
func (s *Service) recordRun(ctx context.Context, saved int) {
	_ = s.st.SetSetting(ctx, keyExtractLastRun, time.Now().UTC().Format(time.RFC3339))
	if saved > 0 {
		total, _, _ := s.st.Setting(ctx, keyExtractTotal)
		n, _ := strconv.Atoi(total)
		_ = s.st.SetSetting(ctx, keyExtractTotal, strconv.Itoa(n+saved))
	}
}

// extractConversation extracts one conversation's waiting turns in one call and says how many
// memories it saved and how many turns it took. Turns with nothing to remember, of a datapack that
// opted out, or in no datapack leave the queue without a call.
func (s *Service) extractConversation(ctx context.Context, h Harness, conv int64) (int, int, error) {
	queued, err := s.st.QueuedTurns(ctx, conv, extractTurns)
	if err != nil || len(queued) == 0 {
		return 0, 0, err
	}
	all := make([]int64, len(queued))
	var turns []store.ExtractionTurn
	for i, t := range queued {
		all[i] = t.Rowid
		if !trivial(t.Input) {
			turns = append(turns, t)
		}
	}
	dp := queued[0].DatapackID
	var d store.DatapackExtraction
	if dp != "" {
		if d, err = s.st.GetDatapackExtraction(ctx, dp); err != nil {
			return 0, 0, err
		}
	}
	if dp == "" || d.OptOut || len(turns) == 0 {
		return 0, len(queued), s.st.DoneExtracting(ctx, all)
	}
	fail := func(err error) (int, int, error) {
		_ = s.st.FailedExtracting(ctx, all, err.Error())
		return 0, 0, err
	}
	earlier, err := s.st.TurnsBefore(ctx, conv, queued[0].Rowid, extractContextTurns)
	if err != nil {
		return fail(err)
	}
	texts := make([]string, len(turns))
	for i, t := range turns {
		texts[i] = t.Input
	}
	found, err := s.SearchMemories(ctx, Search{DatapackID: dp, Query: clip(strings.Join(texts, "\n"), extractQueryChars),
		Limit: extractExisting})
	if err != nil {
		return fail(err)
	}
	existing := make([]string, len(found))
	for i, m := range found {
		existing[i] = m.Memory
	}
	facts, err := h.Extract(ctx, extractSystem(d), extractInput(existing, earlier, turns, time.Now()))
	if err != nil {
		return fail(err)
	}
	saved, err := s.saveExtracted(ctx, dp, turns, facts)
	if err != nil {
		return fail(err)
	}
	return saved, len(queued), s.st.DoneExtracting(ctx, all)
}

// saveExtracted adds the facts a call returned as memories of their turns' conversation, each naming
// its turn, and drops any too close to a memory already there.
func (s *Service) saveExtracted(ctx context.Context, dp string, turns []store.ExtractionTurn,
	facts []ExtractedFact) (int, error) {
	saved := 0
	seen := map[string]bool{}
	for _, f := range facts {
		text := strings.TrimSpace(f.Text)
		key := strings.ToLower(text)
		if text == "" || f.Turn < 0 || f.Turn >= len(turns) || seen[key] {
			continue
		}
		seen[key] = true
		near, err := s.SearchMemories(ctx, Search{DatapackID: dp, Query: text, Limit: 1})
		if err != nil {
			return saved, err
		}
		if len(near) > 0 && near[0].RawSimilarityScore != nil && *near[0].RawSimilarityScore >= extractDuplicate {
			continue
		}
		t := turns[f.Turn]
		if _, err = s.AddMemory(ctx, AddMemory{DatapackID: dp, AgentID: t.AgentID, ConversationID: t.ConversationID,
			Text: text, Metadata: map[string]any{MetaSource: SourceExtract, MetaMessageID: t.MessageID}}); err != nil {
			return saved, err
		}
		saved++
	}
	return saved, nil
}

// Metadata an extracted memory carries: where it came from, and the turn it came from.
const (
	MetaSource    = "meko_source"
	MetaMessageID = "meko_message_id"
	SourceExtract = "extract"
)

// DatapackExtraction is a datapack's extraction settings.
func (s *Service) DatapackExtraction(ctx context.Context, datapack string) (store.DatapackExtraction, error) {
	d, err := s.st.GetDatapackExtraction(ctx, datapack)
	if errors.Is(err, store.ErrNoDatapack) {
		return d, ErrNoDatapack
	}
	return d, err
}

// DatapackExtractionChange changes a datapack's extraction settings: nil fields stay as they are.
type DatapackExtractionChange struct {
	OptOut       *bool   `json:"opt_out"`
	Profile      *string `json:"profile"`
	Instructions *string `json:"instructions"`
}

// SetDatapackExtraction applies a change and returns the settings it leaves.
func (s *Service) SetDatapackExtraction(ctx context.Context, datapack string, c DatapackExtractionChange) (
	store.DatapackExtraction, error) {
	d, err := s.DatapackExtraction(ctx, datapack)
	if err != nil {
		return d, err
	}
	if c.OptOut != nil {
		d.OptOut = *c.OptOut
	}
	if c.Profile != nil {
		if _, ok := extractProfiles[*c.Profile]; !ok {
			return d, fmt.Errorf("%w: profile must be one of %s", ErrBadRequest, strings.Join(ExtractionProfiles(), ", "))
		}
		d.Profile = *c.Profile
	}
	if c.Instructions != nil {
		if len(*c.Instructions) > 2000 {
			return d, fmt.Errorf("%w: instructions are at most 2000 characters", ErrBadRequest)
		}
		d.Instructions = *c.Instructions
	}
	err = s.st.SetDatapackExtraction(ctx, datapack, d)
	if errors.Is(err, store.ErrNoDatapack) {
		err = ErrNoDatapack
	}
	return d, err
}
