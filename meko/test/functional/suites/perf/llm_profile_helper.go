package perf

import (
	"context"
	"fmt"
	"time"

	"github.com/yugabyte/meko/meko/test/functional/framework/apps/llmmock"
	"github.com/yugabyte/meko/meko/test/functional/framework/core"
	"github.com/yugabyte/meko/meko/test/functional/suites/internal/stacktest"
)

// llmProfile makes the mock answer as slowly as a real model: every chat call
// waits chat and every embedding call waits embed. A real model is what a
// latency through the memory and chat paths is made of, and an instant mock
// would measure only the plumbing. The returned function removes the delays.
func llmProfile(t *core.T, chat, embed time.Duration) (func(), error) {
	llm := stacktest.LLM(t)
	var ids []string
	remove := func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		for _, id := range ids {
			_ = llm.RemoveFault(ctx, id)
		}
	}
	for _, f := range []struct {
		route string
		delay time.Duration
	}{{llmmock.RouteChat, chat}, {llmmock.RouteEmbeddings, embed}} {
		if f.delay <= 0 {
			continue
		}
		id, err := llm.AddFault(t, llmmock.Fault{Route: f.route, Kind: llmmock.KindLatency, DelayMS: int(f.delay / time.Millisecond)})
		if err != nil {
			remove()
			return nil, fmt.Errorf("add the %s latency to the LLM mock: %w", f.route, err)
		}
		ids = append(ids, id)
	}
	t.Metric("llm_profile.chat_ms", float64(chat.Milliseconds()), "ms")
	t.Metric("llm_profile.embed_ms", float64(embed.Milliseconds()), "ms")
	return remove, nil
}
