import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen } from "@testing-library/react";

import type { useLabRunnerSession } from "../../hooks/useLabRunnerSession";
import type { LabContextFeed } from "../../hooks/useLabContextFeed";
import { KnowledgeBaseStage } from "./KnowledgeBaseStage";

function feed(overrides: Partial<LabContextFeed> = {}): LabContextFeed {
  return {
    knowledge: [],
    knowledgeTotal: 0,
    memories: [],
    memoriesTotal: 0,
    collective: [],
    collectiveTotal: 0,
    conversationsTotal: 0,
    chunksTotal: null,
    chunkSize: null,
    loading: false,
    ...overrides,
  };
}

// Scope this test to KnowledgeBaseStage's own composition, not FileUpload's
// internals (which have no existing tests of their own to lean on either).
// Captures the props KnowledgeBaseStage passes in, and exposes buttons that
// simulate FileUpload invoking its onQueuedDocsChange/onProcessingActiveChange
// callbacks, so the wiring around those props is actually exercised.
const fileUploadPropsSpy = vi.fn();
vi.mock("@/features/datapacks/components/FileUpload", () => ({
  default: (props: {
    initialFiles?: File[];
    onQueuedDocsChange?: (docs: { name: string; status: string }[]) => void;
    onProcessingActiveChange?: (active: boolean) => void;
  }) => {
    fileUploadPropsSpy(props);
    return (
      <div data-testid="file-upload-mock">
        <button onClick={() => props.onQueuedDocsChange?.([{ name: "x", status: "QUEUED" }])}>
          simulate-queue-doc
        </button>
        <button onClick={() => props.onProcessingActiveChange?.(true)}>simulate-processing-start</button>
        <button onClick={() => props.onProcessingActiveChange?.(false)}>simulate-processing-stop</button>
      </div>
    );
  },
}));

function fakeSession(overrides: Partial<ReturnType<typeof useLabRunnerSession>> = {}) {
  return {
    userId: "user-1",
    currentUser: null,
    datapackId: "dp-1",
    datapackName: "test-datapack",
    accessibleDatapacks: [],
    datapacksLoading: false,
    selectDatapack: vi.fn(),
    createAndSelectDatapack: vi.fn(),
    creating: false,
    userToken: "mko_tkn_fake",
    loading: false,
    error: null,
    refreshTick: 0,
    bumpRefreshTick: vi.fn(),
    lastLessonTrace: null,
    recordLessonTrace: vi.fn(),
    ...overrides,
  } as ReturnType<typeof useLabRunnerSession>;
}

afterEach(cleanup);
beforeEach(() => vi.clearAllMocks());

describe("KnowledgeBaseStage", () => {
  it("shows the degrade prompt when no datapack is selected", () => {
    render(<KnowledgeBaseStage session={fakeSession({ datapackId: null })} feed={feed()} onGoToCreateDatapack={vi.fn()} />);
    expect(screen.getByText(/choose or create a datapack first/i)).toBeTruthy();
    expect(screen.queryByTestId("file-upload-mock")).toBeNull();
  });

  it("renders the upload widget once a datapack is selected", () => {
    render(<KnowledgeBaseStage session={fakeSession()} feed={feed()} onGoToCreateDatapack={vi.fn()} />);
    expect(screen.getByTestId("file-upload-mock")).toBeTruthy();
  });


  it("shows the sample-vs-own card, and 'Add the sample' seeds FileUpload's initialFiles", () => {
    render(<KnowledgeBaseStage session={fakeSession()} feed={feed()} onGoToCreateDatapack={vi.fn()} />);
    expect(screen.getByText("org-coding-standards.md")).toBeTruthy();

    fireEvent.click(screen.getByRole("button", { name: /add the sample/i }));

    const lastProps = fileUploadPropsSpy.mock.calls.at(-1)?.[0];
    expect(lastProps.initialFiles).toHaveLength(1);
    expect(lastProps.initialFiles[0].name).toBe("org-coding-standards.md");
  });

  it("shows the sample file's actual content inline by default, and can be hidden/re-shown", () => {
    render(<KnowledgeBaseStage session={fakeSession()} feed={feed()} onGoToCreateDatapack={vi.fn()} />);
    expect(screen.getByText(/sql injection vulnerability/i)).toBeTruthy();

    fireEvent.click(screen.getByRole("button", { name: /hide preview/i }));
    expect(screen.queryByText(/sql injection vulnerability/i)).toBeNull();

    fireEvent.click(screen.getByRole("button", { name: /^preview$/i }));
    expect(screen.getByText(/sql injection vulnerability/i)).toBeTruthy();
  });

  it("hides the sample card once FileUpload reports a queued doc", () => {
    render(<KnowledgeBaseStage session={fakeSession()} feed={feed()} onGoToCreateDatapack={vi.fn()} />);
    expect(screen.getByText("org-coding-standards.md")).toBeTruthy();

    fireEvent.click(screen.getByText("simulate-queue-doc"));

    expect(screen.queryByText("org-coding-standards.md")).toBeNull();
  });

  it("shows a processing indicator while the upload is active", () => {
    render(<KnowledgeBaseStage session={fakeSession()} feed={feed()} onGoToCreateDatapack={vi.fn()} />);
    expect(screen.queryByText(/chunking and embedding your file/i)).toBeNull();

    fireEvent.click(screen.getByText("simulate-processing-start"));
    expect(screen.getByText(/chunking and embedding your file/i)).toBeTruthy();
  });

  // The indicator used to hang on FileUpload's own flag, which can stay true
  // after the work is done and is false on a remount that missed the
  // transition -- so an indexed document would sit behind a spinner forever.
  it("reports done from the indexed document, not from the upload widget's flag", () => {
    render(
      <KnowledgeBaseStage
        session={fakeSession()}
        feed={feed({ knowledgeTotal: 1, knowledge: [{ id: "d1", status: "COMPLETED" }] as never })}
        onGoToCreateDatapack={vi.fn()}
      />,
    );
    expect(screen.getByText(/1 document indexed/i)).toBeTruthy();
    expect(screen.queryByText(/chunking and embedding your file/i)).toBeNull();
  });

  it("keeps showing done across a remount, since the document is still indexed", () => {
    const indexed = feed({ knowledgeTotal: 1, knowledge: [{ id: "d1", status: "COMPLETED" }] as never });
    const { unmount } = render(
      <KnowledgeBaseStage session={fakeSession()} feed={indexed} onGoToCreateDatapack={vi.fn()} />,
    );
    unmount();
    render(<KnowledgeBaseStage session={fakeSession()} feed={indexed} onGoToCreateDatapack={vi.fn()} />);
    expect(screen.getByText(/1 document indexed/i)).toBeTruthy();
  });

  it("records one aggregate X-Ray call once processing finishes", () => {
    const recordLessonTrace = vi.fn();
    render(<KnowledgeBaseStage session={fakeSession({ recordLessonTrace })} feed={feed()} onGoToCreateDatapack={vi.fn()} />);

    fireEvent.click(screen.getByText("simulate-processing-start"));
    expect(recordLessonTrace).not.toHaveBeenCalled();

    fireEvent.click(screen.getByText("simulate-processing-stop"));
    expect(recordLessonTrace).toHaveBeenCalledWith({
      lessonTitle: "Build a knowledge base",
      calls: [expect.objectContaining({ name: "Upload & index file(s)", status: "ok" })],
    });
  });
});
