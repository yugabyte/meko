package functional

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/yugabyte/meko/meko/test/functional/framework/apps/llmmock"
	"github.com/yugabyte/meko/meko/test/functional/framework/core"
	"github.com/yugabyte/meko/meko/test/functional/suites/internal/stacktest"
)

const chatTimeout = 3 * time.Minute

func init() {
	core.Register(core.Test{
		Name:       "functional/inference_chat",
		Suite:      "functional",
		Category:   core.Functional,
		Tags:       tags(),
		OwningTeam: core.TeamInferenceGateway,
		Env:        core.EnvSpec{Sharing: core.Shared, Shape: core.SingleStack},
		Description: "POST /chat on the inference gateway with a PAT answers with the reply scripted on the LLM mock, " +
			"opens a conversation, and the mock answered at least one chat call that carried the question and the gateway's system prompt.",
		Run: inferenceChat,
	})
}

type chatResponse struct {
	Message        string `json:"message"`
	SessionID      string `json:"session_id"`
	ConversationID string `json:"conversation_id"`
	Steps          []struct {
		Name   string `json:"name"`
		Status string `json:"status"`
		Detail string `json:"detail"`
	} `json:"processing_steps"`
}

func inferenceChat(t *core.T) error {
	s, err := newMemStack(t)
	defer s.close()
	if err != nil {
		return err
	}
	gateway, closeGateway := stacktest.Gateway(t)
	defer closeGateway()
	ctx := context.Background()
	marker := marker(t)
	question := fmt.Sprintf("What colour is the sky above %s today?", marker)
	reply := "Scripted sky reply " + marker

	id, err := s.llm.AddScript(ctx, llmmock.Script{Match: question, System: "Meko playground assistant", Reply: reply})
	if err != nil {
		return err
	}
	defer s.llm.RemoveScript(context.Background(), id)

	var resp chatResponse
	if err := t.Step("chat", func(h *core.StepHandle) error {
		body, _ := json.Marshal(map[string]any{
			"message": question, "user_token": s.token.Value, "user_id": "mekoqa-" + t.ResourceSuffix(),
			"datapack_id": s.datapack.ID,
		})
		cctx, cancel := context.WithTimeout(t, chatTimeout)
		defer cancel()
		req, _ := http.NewRequestWithContext(cctx, http.MethodPost, gateway+"/chat", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		r, err := http.DefaultClient.Do(req)
		if err != nil {
			return fmt.Errorf("POST /chat: %w", err)
		}
		defer r.Body.Close()
		raw, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		stacktest.FailOnLeak(t, "POST /chat", string(raw))
		if r.StatusCode != http.StatusOK {
			return fmt.Errorf("POST /chat answered %s: %s", r.Status, raw)
		}
		if err := json.Unmarshal(raw, &resp); err != nil {
			return fmt.Errorf("decode the chat response: %w: %s", err, raw)
		}
		h.Set("conversation_id", resp.ConversationID)
		if resp.Message != reply {
			return fmt.Errorf("the answer is %q, want the scripted %q", resp.Message, reply)
		}
		if resp.ConversationID == "" {
			return fmt.Errorf("the response carries no conversation id")
		}
		return nil
	}); err != nil {
		return err
	}

	return t.Step("mock_saw_the_chat_call", func(h *core.StepHandle) error {
		calls, err := waitCall(t, s.llm, llmmock.RouteChat, marker, 1, 30*time.Second)
		if err != nil {
			return err
		}
		var main *llmmock.Call
		for i := range calls {
			if calls[i].Answer == "script:"+id {
				main = &calls[i]
			}
		}
		if main == nil {
			return fmt.Errorf("none of the %d chat call(s) carrying the question was answered by the script: %v", len(calls), answersOf(calls))
		}
		h.Set("model", main.Model)
		h.Set("api", main.API+" "+main.Route)
		var system string
		for _, m := range main.Messages {
			if m.Role == "system" {
				system = m.Text
			}
		}
		if !strings.Contains(system, "Meko playground assistant") {
			return fmt.Errorf("the call carried no gateway system prompt, its messages were %+v", main.Messages)
		}
		return nil
	})
}

func answersOf(calls []llmmock.Call) []string {
	out := make([]string, len(calls))
	for i, c := range calls {
		out[i] = c.Answer
	}
	return out
}
