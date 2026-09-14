import { afterEach, describe, expect, it } from "vitest";
import { cleanup, render, screen } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";

import type { LabContextFeed } from "../../hooks/useLabContextFeed";
import { LabContextPane } from "./LabContextPane";

afterEach(cleanup);

function feed(overrides: Partial<LabContextFeed> = {}): LabContextFeed {
  return {
    knowledge: [],
    knowledgeTotal: 0,
    memories: [],
    memoriesTotal: 0,
    collective: [],
    collectiveTotal: 0,
    conversationsTotal: 0,
  chunkSize: null,
    loading: false,
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
