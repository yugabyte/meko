package functional

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/yugabyte/meko/meko/test/functional/framework/apps/clientpod"
	"github.com/yugabyte/meko/meko/test/functional/framework/apps/mekostack"
	"github.com/yugabyte/meko/meko/test/functional/framework/core"
)

func init() {
	core.Register(core.Test{
		Name:       "functional/hook_capture",
		Suite:      "functional",
		Category:   core.Functional,
		Tags:       tags(),
		OwningTeam: core.TeamMCPServer,
		Env:        core.EnvSpec{Sharing: core.Shared, Shape: core.SingleStack},
		Timeout:    15 * time.Minute,
		Params:     []string{"functional.hook_capture"},
		Description: "The published Claude Code hooks (meko-skills plugin, capture.js) run in a pod beside the stack with a PAT and a pinned datapack, the way a user's " +
			"machine runs them. A session's transcript is captured into one conversation with every exchange in order, a fact from a user turn becomes a searchable memory, " +
			"exchanges captured while the MCP server is unreachable stay queued and are delivered by the next drain without duplicates, and a drain with nothing new adds nothing.",
		Run: hookCapture,
	})
}

func hookCapture(t *core.T) error {
	ref := t.Options.GetString("functional.hook_capture.ref", "main")
	image := t.Options.GetString("functional.hook_capture.image", "mirror.gcr.io/library/node:20-alpine")
	accts, done := accountsFor(t, 1)
	defer done()
	owner, err := newUser(t, accts[0])
	defer owner.close()
	if err != nil {
		return err
	}
	mk := marker(t)
	dp, err := ownedDatapack(t, owner, "hooks-"+t.ResourceSuffix())
	defer deleteDatapack(owner, dp.ID)
	if err != nil {
		return err
	}
	env := clusterEnv(t)
	kube := env.Kube()
	agent := "claude_code:mekoqa-" + t.ResourceSuffix()[:8]
	pod := "mekoqa-hooks-" + t.ResourceSuffix()[:10]
	_, mcpURL := mekostack.ClusterURLs(env.Stack().Namespace())

	tmp, err := os.MkdirTemp("", "mekoqa-hooks-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)
	cp := clientpod.NewK8s(kube)
	if err := t.Step("start_the_client", func(h *core.StepHandle) error {
		h.Set("ref", ref)
		if err := cp.Start(t, clientpod.Spec{Name: pod, Image: image, Command: []string{"sleep", "3600"}, CPU: "500m", Memory: "256Mi"}); err != nil {
			return err
		}
		dir := filepath.Join(tmp, "skills")
		if err := fetchHooks(t, ref, dir); err != nil {
			return err
		}
		exec := kube.ForPod(pod, "")
		if _, err := exec.Run(t, "mkdir -p /hooks /work /tmp/wm"); err != nil {
			return err
		}
		if err := exec.Copy(t, filepath.Join(dir, "lib"), "/hooks/lib"); err != nil {
			return fmt.Errorf("copy the hooks into %s: %w", pod, err)
		}
		pin, _ := json.Marshal(map[string]string{"datapack_id": dp.ID, "datapack_name": dp.Name, "selected_at": time.Now().UTC().Format(time.RFC3339)})
		slug := strings.Trim(strings.Map(func(r rune) rune {
			if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '.' || r == '_' || r == '-' {
				return r
			}
			return '_'
		}, agent), "_")
		_, err := exec.Run(t, fmt.Sprintf("printf '%%s' '%s' > /tmp/wm/pin-%s.json", string(pin), slug))
		return err
	}); err != nil {
		return err
	}
	defer func() {
		ctx, cancel := cleanupCtx()
		defer cancel()
		_ = cp.Stop(ctx, resultSink{t})
	}()

	session := "mekoqa-session-" + t.ResourceSuffix()
	transcript := "/work/" + session + ".jsonl"
	turns := []string{
		fmt.Sprintf("First %s: please remember the build runs on runner pine.", mk),
		fmt.Sprintf("Second %s: my favourite editor theme is called driftwood.", mk),
		fmt.Sprintf("Third %s: the staging database is called heron.", mk),
	}
	later := []string{
		fmt.Sprintf("Fourth %s: the release notes live in the wiki under lantern.", mk),
		fmt.Sprintf("Fifth %s: the pager rotates every Monday.", mk),
	}
	write := func(all []string) error {
		var b strings.Builder
		for i, in := range all {
			for _, e := range []map[string]any{
				{"type": "user", "sessionId": session, "uuid": fmt.Sprintf("u%d", i), "timestamp": time.Now().UTC().Format(time.RFC3339),
					"message": map[string]any{"role": "user", "content": in}},
				{"type": "assistant", "sessionId": session, "uuid": fmt.Sprintf("a%d", i), "timestamp": time.Now().UTC().Format(time.RFC3339),
					"message": map[string]any{"role": "assistant", "content": []map[string]string{{"type": "text", "text": fmt.Sprintf("Noted %d.", i)}}}},
			} {
				line, _ := json.Marshal(e)
				b.Write(line)
				b.WriteByte('\n')
			}
		}
		local := filepath.Join(tmp, session+".jsonl")
		if err := os.WriteFile(local, []byte(b.String()), 0o644); err != nil {
			return err
		}
		return kube.ForPod(pod, "").Copy(t, local, transcript)
	}
	run := func(op string, url string, extra map[string]any) (string, error) {
		in := map[string]any{"session_id": session, "transcript_path": transcript, "cwd": "/work", "hook_event_name": op}
		for k, v := range extra {
			in[k] = v
		}
		body, _ := json.Marshal(in)
		cmd := fmt.Sprintf("cd /hooks/lib && printf '%%s' '%s' | env HOME=/tmp MEKO_WATERMARK_DIR=/tmp/wm MEKO_HOOK_CLIENT=claude_code MEKO_AGENT_ID='%s' MEKO_MCP_URL='%s' MEKO_API_KEY='%s' MEKO_API_TIMEOUT=10 node capture.js %s 2>&1",
			string(body), agent, url, owner.pat.Value, op)
		res, err := kube.ForPod(pod, "").Run(t, cmd)
		out := strings.TrimSpace(res.Stdout + res.Stderr)
		return out, err
	}
	conversation := func() (convBody, error) {
		var list struct {
			Conversations []struct {
				ID string `json:"id"`
			} `json:"conversations"`
		}
		if err := owner.callJSON("conversation_list", map[string]any{"datapack_id": dp.ID, "agent_id": agent}, &list); err != nil {
			return convBody{}, err
		}
		if len(list.Conversations) != 1 {
			return convBody{}, fmt.Errorf("agent %s has %d conversations in %s, want one per session", agent, len(list.Conversations), dp.ID)
		}
		var c convBody
		err := owner.callJSON("conversation_get", map[string]any{"datapack_id": dp.ID, "conversation_id": list.Conversations[0].ID, "agent_id": agent, "include_messages": true}, &c)
		return c, err
	}
	inOrder := func(want []string) error {
		var c convBody
		var err error
		werr := eventually(t, fmt.Sprintf("the captured conversation to hold %d exchanges", len(want)), visibleWithin(t), func() (bool, error) {
			c, err = conversation()
			return err == nil && len(c.Messages) >= len(want), nil
		})
		if werr != nil {
			return fmt.Errorf("%w (last: %d messages, %v)", werr, len(c.Messages), err)
		}
		if len(c.Messages) != len(want) {
			return fmt.Errorf("the conversation holds %d messages, want exactly %d: a drain sent an exchange twice or lost one", len(c.Messages), len(want))
		}
		for i, m := range c.Messages {
			if !strings.Contains(fmt.Sprint(m.Input), want[i]) {
				return fmt.Errorf("message %d is %v, want %q in order", i, m.Input, want[i])
			}
		}
		return nil
	}

	if err := t.Step("session_is_captured", func(h *core.StepHandle) error {
		if err := write(turns); err != nil {
			return err
		}
		out, err := run("session-start", mcpURL, nil)
		h.Set("session_start", truncate(out, 300))
		if err != nil {
			return fmt.Errorf("session-start: %w: %s", err, truncate(out, 300))
		}
		out, err = run("drain", mcpURL, map[string]any{"lifecycle": "closing"})
		h.Set("drain", truncate(out, 300))
		if err != nil {
			return fmt.Errorf("drain: %w: %s", err, truncate(out, 300))
		}
		return inOrder(turns)
	}); err != nil {
		return err
	}

	var errs []error
	step := func(name string, fn func() error) {
		if err := t.Step(name, func(*core.StepHandle) error { return fn() }); err != nil {
			errs = append(errs, err)
		}
	}
	step("a_user_fact_becomes_a_memory", func() error {
		conv, err := owner.conversation(dp.ID, "hooks check "+mk)
		if err != nil {
			return err
		}
		var last string
		err = eventually(t, "memory_search to find the captured fact", 2*visibleWithin(t), func() (bool, error) {
			text, terr := owner.call("memory_search", map[string]any{"datapack_id": dp.ID, "conversation_id": conv, "agent_id": agent, "query": "favourite editor theme driftwood " + mk})
			if terr != nil {
				return false, terr
			}
			last = text
			return strings.Contains(text, "driftwood"), nil
		})
		if err == nil {
			return nil
		}
		all, terr := owner.call("memory_get_all", map[string]any{"datapack_id": dp.ID, "conversation_id": conv, "agent_id": agent})
		if terr != nil {
			return fmt.Errorf("%w; memory_get_all failed too: %v; last search answer: %s", err, terr, truncate(last, 600))
		}
		return fmt.Errorf("%w; a stored memory names the fact: %v; last search answer: %s", err, strings.Contains(all, "driftwood"), truncate(last, 600))
	})
	step("a_new_session_preloads_the_memories", func() error {
		next := session + "-next"
		in, _ := json.Marshal(map[string]any{"session_id": next, "transcript_path": "/work/" + next + ".jsonl", "cwd": "/work", "hook_event_name": "SessionStart"})
		cmd := fmt.Sprintf("cd /hooks/lib && touch /work/%s.jsonl && printf '%%s' '%s' | env HOME=/tmp MEKO_WATERMARK_DIR=/tmp/wm MEKO_HOOK_CLIENT=claude_code MEKO_AGENT_ID='%s' MEKO_MCP_URL='%s' MEKO_API_KEY='%s' node capture.js session-start 2>&1",
			next, string(in), agent, mcpURL, owner.pat.Value)
		res, err := kube.ForPod(pod, "").Run(t, cmd)
		out := res.Stdout + res.Stderr
		if err != nil {
			return fmt.Errorf("session-start: %w: %s", err, truncate(out, 300))
		}
		m := preloadedRe.FindStringSubmatch(out)
		if m == nil || m[1] == "0" {
			return fmt.Errorf("a new session in a datapack holding this agent's memories preloaded none: %s", truncate(out, 400))
		}
		return nil
	})
	step("an_outage_queues_then_delivers_once", func() error {
		all := append(append([]string{}, turns...), later...)
		if err := write(all); err != nil {
			return err
		}
		out, _ := run("drain", "http://127.0.0.1:9/mcp", map[string]any{"lifecycle": "active"})
		if c, err := conversation(); err == nil && len(c.Messages) != len(turns) {
			return fmt.Errorf("a drain with the MCP server unreachable changed the conversation to %d messages (%s)", len(c.Messages), truncate(out, 200))
		}
		var last string
		err := eventually(t, "a later drain to deliver the queued exchanges", 3*visibleWithin(t), func() (bool, error) {
			out, err := run("drain", mcpURL, map[string]any{"lifecycle": "closing"})
			if err != nil {
				return false, fmt.Errorf("drain after the outage: %w: %s", err, truncate(out, 300))
			}
			last = out
			var r struct {
				Remaining int    `json:"queued_remaining"`
				Delivery  string `json:"delivery"`
			}
			_ = json.Unmarshal([]byte(lastLine(out)), &r)
			return r.Delivery == "complete" && r.Remaining == 0, nil
		})
		if err != nil {
			return fmt.Errorf("%w; last drain: %s", err, truncate(last, 300))
		}
		return inOrder(all)
	})
	step("a_drain_with_nothing_new_adds_nothing", func() error {
		if out, err := run("drain", mcpURL, map[string]any{"lifecycle": "closing"}); err != nil {
			return fmt.Errorf("drain: %w: %s", err, truncate(out, 300))
		}
		return inOrder(append(append([]string{}, turns...), later...))
	})
	return errors.Join(errs...)
}

// fetchHooks downloads meko-skills at ref and keeps the Claude Code plugin's
// hook library, the code a user's machine runs.
func fetchHooks(ctx context.Context, ref, dir string) error {
	url := "https://codeload.github.com/yugabyte/meko-skills/tar.gz/" + ref
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("download meko-skills %s: %w", ref, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("download meko-skills %s: HTTP %d", ref, resp.StatusCode)
	}
	gz, err := gzip.NewReader(resp.Body)
	if err != nil {
		return err
	}
	tr := tar.NewReader(gz)
	const want = "/plugins/meko-agent-skills/hooks-handlers/lib/"
	n := 0
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		i := strings.Index(hdr.Name, want)
		if i < 0 || hdr.Typeflag != tar.TypeReg {
			continue
		}
		dst := filepath.Join(dir, "lib", filepath.Base(hdr.Name))
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return err
		}
		f, err := os.Create(dst)
		if err != nil {
			return err
		}
		if _, err := io.Copy(f, io.LimitReader(tr, 4<<20)); err != nil {
			f.Close()
			return err
		}
		f.Close()
		n++
	}
	if n == 0 {
		return fmt.Errorf("meko-skills %s has no %s", ref, want)
	}
	return nil
}

type resultSink struct{ t *core.T }

func (s resultSink) Add(name, kind string, content []byte) error {
	s.t.AttachBytes(name, kind, content)
	return nil
}

var preloadedRe = regexp.MustCompile(`preloaded (\d+) memor`)

func lastLine(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	return lines[len(lines)-1]
}
