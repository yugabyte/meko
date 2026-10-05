import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, render, screen, waitFor } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";

import { FEATURE_FLAGS } from "@/lib/featureFlags";
import {
  getKnowledgeBaseDocuments,
  listMemories,
  listPromotedMemories,
  type KnowledgeBaseDocument,
} from "@/lib/api/mekoApi";
import { TooltipProvider } from "@/components/ui/tooltip";
import {
  WorkbenchLMLiveObservationPanel,
  type WorkbenchLMLiveObservationPanelProps,
} from "./WorkbenchLMLiveObservationPanel";

function renderPanel(props: WorkbenchLMLiveObservationPanelProps) {
  return render(
    <MemoryRouter>
      <TooltipProvider>
        <WorkbenchLMLiveObservationPanel {...props} />
      </TooltipProvider>
    </MemoryRouter>,
  );
}

vi.mock("@/lib/api/mekoApi", async (importOriginal) => {
  const actual = await importOriginal<typeof import("@/lib/api/mekoApi")>();
  return {
    ...actual,
    listMemories: vi.fn(),
    listPromotedMemories: vi.fn(),
    getKnowledgeBaseDocuments: vi.fn(),
  };
});

vi.mock("@/hooks/useCurrentUser", () => ({
  useCurrentUser: () => ({
    data: { account_id: "user-1", email: "me@example.com", user_type: "free" },
  }),
}));

const mockListMemories = vi.mocked(listMemories);
const mockListPromoted = vi.mocked(listPromotedMemories);
const mockGetKbDocs = vi.mocked(getKnowledgeBaseDocuments);

afterEach(() => {
  cleanup();
  localStorage.clear();
});

beforeEach(() => {
  vi.clearAllMocks();
  mockListMemories.mockResolvedValue({ memories: [], total: 0 } as never);
  mockListPromoted.mockResolvedValue({ promoted_memories: [], total: 0 } as never);
  mockGetKbDocs.mockResolvedValue({ items: [], total: 0, page: 1, page_size: 5, has_more: false });
});

describe("WorkbenchLMLiveObservationPanel — stacked layout (default, WorkbenchLM's own usage)", () => {
  it("renders Memories and Collective Memories, with no Shared Knowledge column, and never fetches KB docs", async () => {
    renderPanel({ datapackId: "dp-1", datapackName: "Test" });
    await waitFor(() => expect(mockListMemories).toHaveBeenCalled());
    expect(screen.getByText("Memories")).toBeTruthy();
    expect(screen.getByText("Collective Memories")).toBeTruthy();
    expect(screen.queryByText("Shared Knowledge")).toBeNull();
    expect(mockGetKbDocs).not.toHaveBeenCalled();
  });

  it("keeps the 'Live observation' title", () => {
    renderPanel({ datapackId: "dp-1", datapackName: "Test" });
    expect(screen.getByText("Live observation")).toBeTruthy();
  });
});

describe("WorkbenchLMLiveObservationPanel — memory delete is behind enableMemoryDeletion", () => {
  // The caller's own post-cutoff memory: every other delete gate passes, so
  // the flag is the only thing deciding whether the button renders.
  beforeEach(() => {
    mockListMemories.mockResolvedValue({
      memories: [{ id: "m-1", data: "Raj likes pencil", user_id: "user-1", created_at: new Date().toISOString() }],
      total: 1,
    } as never);
  });

  it("hides the delete button by default", async () => {
    renderPanel({ datapackId: "dp-1", datapackName: "Test" });
    await screen.findByText("Raj likes pencil");
    expect(screen.queryByRole("button", { name: "Delete memory" })).toBeNull();
  });

  it("shows the delete button when the flag is set in localStorage", async () => {
    localStorage.setItem(FEATURE_FLAGS.enableMemoryDeletion.storageKey, "true");
    renderPanel({ datapackId: "dp-1", datapackName: "Test" });
    await screen.findByText("Raj likes pencil");
    expect(screen.getByRole("button", { name: "Delete memory" })).toBeTruthy();
  });
});

describe("WorkbenchLMLiveObservationPanel — columns layout (Labs CONTEXT panel)", () => {
  it("renders all three sections, Shared Knowledge as its own column", async () => {
    renderPanel({ datapackId: "dp-1", datapackName: "Test", layout: "columns" });
    await waitFor(() =>
      expect(mockGetKbDocs).toHaveBeenCalledWith(
        "dp-1",
        expect.objectContaining({ page: 1, page_size: 5 }),
      ),
    );
    expect(screen.getByText("Memories")).toBeTruthy();
    expect(screen.getByText("Collective Memories")).toBeTruthy();
    expect(screen.getByText("Shared Knowledge")).toBeTruthy();
  });

  it("shows the 'Meko Context - Live' title instead of 'Live observation'", () => {
    renderPanel({ datapackId: "dp-1", datapackName: "Test", layout: "columns" });
    expect(screen.getByText("Meko Context - Live")).toBeTruthy();
    expect(screen.queryByText("Live observation")).toBeNull();
  });

  it("groups Collective Memory with Shared Knowledge, separate from private Memories", () => {
    renderPanel({ datapackId: "dp-1", datapackName: "Test", layout: "columns" });
    const privateColumn = screen.getByTestId("private-memory-column");
    const sharedColumn = screen.getByTestId("shared-side-column");

    expect(privateColumn.contains(screen.getByText("Memories"))).toBe(true);
    expect(sharedColumn.contains(screen.getByText("Collective Memories"))).toBe(true);
    expect(sharedColumn.contains(screen.getByText("Shared Knowledge"))).toBe(true);
  });

  it("shows the empty state before any documents exist", async () => {
    renderPanel({ datapackId: "dp-1", datapackName: "Test", layout: "columns" });
    expect(await screen.findByText("No knowledge sources yet.")).toBeTruthy();
  });

  it("renders a document tile with its name, chunk count, and status pill", async () => {
    const doc: KnowledgeBaseDocument = {
      id: "doc-1",
      document_name: "coding-standards.md",
      document_uri: "s3://bucket/coding-standards.md",
      status: "COMPLETED",
      chunks_processed: 12,
    };
    mockGetKbDocs.mockResolvedValue({ items: [doc], total: 1, page: 1, page_size: 5, has_more: false });
    renderPanel({ datapackId: "dp-1", datapackName: "Test", layout: "columns" });
    expect(await screen.findByText("coding-standards.md")).toBeTruthy();
    expect(screen.getByText("12 chunks")).toBeTruthy();
    expect(screen.getByText("Completed")).toBeTruthy();
  });

  it("shows an error message when the KB fetch fails", async () => {
    mockGetKbDocs.mockRejectedValue(new Error("boom"));
    renderPanel({ datapackId: "dp-1", datapackName: "Test", layout: "columns" });
    expect(await screen.findByText("boom")).toBeTruthy();
  });

  it("prompts to select a datapack when none is active", async () => {
    renderPanel({ datapackId: null, datapackName: null, layout: "columns" });
    expect(await screen.findByText("Select a datapack to see shared knowledge.")).toBeTruthy();
    expect(mockGetKbDocs).not.toHaveBeenCalled();
  });
});
