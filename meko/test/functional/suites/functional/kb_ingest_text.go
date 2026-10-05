package functional

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/yugabyte/meko/meko/test/functional/framework/apps/llmmock"
	"github.com/yugabyte/meko/meko/test/functional/framework/apps/mekoapi"
	"github.com/yugabyte/meko/meko/test/functional/framework/core"
)

const indexWithin = 5 * time.Minute

func init() {
	core.Register(core.Test{
		Name:       "functional/kb_ingest_text",
		Suite:      "functional",
		Category:   core.Functional,
		Tags:       tags(),
		OwningTeam: core.TeamAPIServer,
		Env:        core.EnvSpec{Sharing: core.Shared, Shape: core.SingleStack},
		Timeout:    20 * time.Minute,
		Description: "Upload a text file to a datapack's knowledge base. The api-server stores it in MinIO, the text rag worker " +
			"chunks and embeds it through the LLM mock, the document reaches COMPLETED, and knowledgebase_search returns its chunk.",
		Run: kbIngestText,
	})
}

func kbIngestText(t *core.T) error {
	s, err := newMemStack(t)
	defer s.close()
	if err != nil {
		return err
	}
	ctx := context.Background()
	marker := marker(t)
	content := fmt.Sprintf("The mekoqa harbour report %s says the lighthouse keeper repaints the lamp room every spring.\n", marker)

	before, err := s.llm.Calls(ctx, llmmock.Filter{Route: llmmock.RouteEmbeddings})
	if err != nil {
		return err
	}
	after := 0
	for _, c := range before {
		after = max(after, c.Seq)
	}

	var key string
	if err := t.Step("upload", func(h *core.StepHandle) error {
		up, err := s.api.UploadKBFile(t, s.datapack.ID, "report-"+marker+".txt", []byte(content))
		if err != nil {
			return err
		}
		key = up.DocumentKey
		h.Set("document_key", key)
		h.Set("status", up.Status)
		if key == "" {
			return fmt.Errorf("the upload answered no document key: %+v", up)
		}
		return nil
	}); err != nil {
		return err
	}

	if err := t.Step("document_completes", func(h *core.StepHandle) error {
		var doc mekoapi.KBDocument
		err := t.WaitFor("the document to be indexed", indexWithin, 5*time.Second, func() (bool, error) {
			var err error
			doc, err = s.api.KBDocumentStatus(t, s.datapack.ID, key)
			if mekoapi.IsNotFound(err) {
				return false, nil
			}
			if err != nil {
				return false, err
			}
			if doc.DocumentStatus == "FAILED" {
				return false, fmt.Errorf("the document failed to index: %+v", doc)
			}
			return doc.DocumentStatus == "COMPLETED", nil
		})
		h.Set("status", doc.DocumentStatus)
		return err
	}); err != nil {
		return err
	}

	if err := t.Step("worker_embedded_through_the_mock", func(h *core.StepHandle) error {
		calls, err := s.llm.Calls(ctx, llmmock.Filter{Route: llmmock.RouteEmbeddings, After: after})
		if err != nil {
			return err
		}
		var ours []llmmock.Call
		for _, c := range calls {
			if c.Status == 200 && (strings.Contains(c.Text, marker) || tokenCall(c)) {
				ours = append(ours, c)
			}
		}
		h.Set("embedding_calls_seen", len(calls))
		if len(ours) == 0 {
			return fmt.Errorf("the mock saw %d embedding call(s) and none came from the rag worker", len(calls))
		}
		return nil
	}); err != nil {
		return err
	}

	return t.Step("knowledgebase_search_returns_the_chunk", func(*core.StepHandle) error {
		if err := s.createConversation(); err != nil {
			return err
		}
		return t.WaitFor("knowledgebase_search to return the chunk", time.Minute, 5*time.Second, func() (bool, error) {
			out, terr := s.tool("knowledgebase_search", map[string]any{"query": "lighthouse keeper " + marker, "conversation_id": s.conversion})
			if terr != nil {
				return false, terr
			}
			return strings.Contains(out, marker), nil
		})
	})
}

// tokenCall is an embedding call whose input was token ids, which is how the rag
// workers' langchain client sends text.
func tokenCall(c llmmock.Call) bool {
	for _, in := range c.Inputs {
		fields := strings.Fields(in)
		if len(fields) == 0 {
			return false
		}
		for _, f := range fields {
			for _, r := range f {
				if r < '0' || r > '9' {
					return false
				}
			}
		}
	}
	return len(c.Inputs) > 0
}
