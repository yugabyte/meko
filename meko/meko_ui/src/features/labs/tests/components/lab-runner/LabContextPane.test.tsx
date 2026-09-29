import { afterEach, describe, expect, it } from "vitest";
import { cleanup, render, screen } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";

import type { LabContextFeed } from "@/features/labs/hooks/useLabContextFeed";
import { LabContextPane } from "@/features/labs/components/lab-runner/LabContextPane";

afterEach(cleanup);

function feed(overrides: Partial<LabContextFeed> = {}): LabContextFeed {
  return {
    knowledge: [],
    knowledgeTotal: 0,
    memories: [],
    memoriesTotal: 0,
    collective: [],
    collectiveTotal: 0,
    conversations: [],
    conversationsTotal: 0,
    chunksTotal: null,
    chunkSize: null,
    loading: false,
    hasLoaded: true,
    ...overrides,
  };
}

function renderPane(f: LabContextFeed) {
  // TokenSavingsPane sits inside this pane and runs its own query.
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return render(
    <QueryClientProvider client={qc}>
    <LabContextPane
      feed={f}
      datapackId="dp-1"
      datapackName="foo-project"
      datapackGrant={null}
      lastLessonTrace={null}
      showContext
    />
    </QueryClientProvider>,
  );
}

describe("LabContextPane", () => {
  // document_name is the S3 key -- <userId>/<datapackId>/<contentHash>/<file>
  // (see S3Service.GenerateUploadURL). Rendering it raw filled the row with
  // UUIDs and truncated away the only part the user recognises.
  it("shows a knowledge document's filename, not its storage key", () => {
    renderPane(
      feed({
        knowledgeTotal: 1,
        knowledge: [
          {
            id: "doc-1",
            document_name:
              "2a6624ef-26c1-410c-bff3-dc111f2e45a2/e2446e0f-f0d3-428a-9f5a-b1c2d3e4f5a6/9f86d081884c/org-coding-standards.md",
            document_uri: "s3://bucket/key",
            status: "COMPLETED",
            chunks_processed: 4,
          },
        ] as never,
      }),
    );

    expect(screen.getByText("org-coding-standards.md")).toBeTruthy();
    expect(screen.queryByText(/2a6624ef-26c1/)).toBeNull();
  });

  it("says what will land in each section rather than just showing it empty", () => {
    renderPane(feed());
    expect(screen.getByText(/Upload your standards/)).toBeTruthy();
    expect(screen.getByText(/Facts you save/)).toBeTruthy();
  });

  it("shows nothing about context on the step that runs before any datapack exists", () => {
    render(
      <QueryClientProvider client={new QueryClient({ defaultOptions: { queries: { retry: false } } })}>
        <LabContextPane
          feed={feed({ knowledgeTotal: 3, memoriesTotal: 2 })}
          datapackId="dp-1"
          datapackName="foo-project"
          datapackGrant={null}
          lastLessonTrace={null}
          showContext={false}
        />
      </QueryClientProvider>,
    );
    // A resumed run still has a datapack selected; the opening step's whole
    // point is that the agent has nothing, so the pane must not contradict it.
    expect(screen.queryByText("Knowledge")).toBeNull();
    expect(screen.queryByText("foo-project")).toBeNull();
    expect(screen.getByText(/Nothing yet/)).toBeTruthy();
  });
});

// Conversations replaced collective memory in the pane. A conversation is a
// context source like the other two, and leaving it as a bare count made it
// the only one whose contents the reader could not see.
describe("LabContextPane conversations", () => {
  const conversation = {
    id: "conv-1",
    started_at: "2026-09-24T10:00:00Z",
    last_activity_at: "2026-09-24T10:02:00Z",
    first_message: "We always parameterize SQL, never string concatenation.",
    messages: 2,
    memories_added: 1,
    llm_calls: 1,
  };

  it("lists the threads, not just how many there are", () => {
    renderPane(feed({ conversationsTotal: 1, conversations: [conversation] as never }));
    expect(screen.getByText(/We always parameterize SQL/)).toBeTruthy();
    expect(screen.getByText("2 messages · 1 memories extracted")).toBeTruthy();
  });

  // MCP passes a title on conversation_create; a session started any other
  // way has none, and the opening prompt says more than "Untitled".
  it("falls back to the opening message when a thread has no title", () => {
    renderPane(
      feed({
        conversationsTotal: 1,
        conversations: [{ ...conversation, title: "  " }] as never,
      }),
    );
    expect(screen.getByText(/We always parameterize SQL/)).toBeTruthy();
  });

  it("shows at most three, since the pane is a glance and not a listing", () => {
    const many = Array.from({ length: 6 }, (_, i) => ({
      ...conversation,
      id: `conv-${i}`,
      first_message: `Thread number ${i}`,
    }));
    renderPane(feed({ conversationsTotal: many.length, conversations: many as never }));
    expect(screen.getByText("Thread number 0")).toBeTruthy();
    expect(screen.queryByText("Thread number 3")).toBeNull();
  });

  // Promoting a memory is no longer a lesson, so the section was permanently
  // empty.
  it("no longer shows a collective memory section", () => {
    renderPane(feed());
    expect(screen.queryByText(/collective memory/i)).toBeNull();
  });
});
