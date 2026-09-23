// Routing rules for the Collections area: a well-formed id opens the detail
// page, and anything else falls back to the list rather than rendering a
// detail page bound to an id the API will reject.

import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import type { ReactElement } from "react";
import { cleanup, render, screen } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";

import CollectionsSection from "./CollectionsSection";

const COLLECTION_ID = "11111111-2222-3333-4444-555555555555";

vi.mock("@/lib/api/mekoApi", async (o) => ({
  ...(await o<typeof import("@/lib/api/mekoApi")>()),
  getCurrentUser: vi.fn().mockResolvedValue({ account_id: "u-1", email: "a@b.c" }),
}));

vi.mock("@/lib/api/collectionsApi", () => ({
  listCollections: vi.fn().mockResolvedValue([]),
  listCollectionThemes: vi.fn().mockResolvedValue([]),
  listDatapackPaths: vi.fn().mockResolvedValue({}),
  // The id is inlined rather than referencing COLLECTION_ID: vi.mock is hoisted
  // above the module's own consts.
  getCollection: vi.fn().mockResolvedValue({
    collection: {
      collection_id: "11111111-2222-3333-4444-555555555555",
      user_id: "u-1",
      collection_name: "database_dev",
      theme_key: "adlc",
      theme_version: "1.0",
      description: "",
      folder_count: 1,
      datapack_count: 0,
      created_at: new Date().toISOString(),
      updated_at: new Date().toISOString(),
    },
    folders: [],
    datapacks: [],
    members: { people: 1, agents: 0 },
  }),
  createCollection: vi.fn(),
  updateCollection: vi.fn(),
  deleteCollection: vi.fn(),
  createCollectionFolder: vi.fn(),
  renameCollectionFolder: vi.fn(),
  deleteCollectionFolder: vi.fn(),
  placeDatapack: vi.fn(),
  unplaceDatapack: vi.fn(),
}));

function renderAt(url: string): ReactElement {
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false } },
  });
  return (
    <QueryClientProvider client={client}>
      <MemoryRouter initialEntries={[url]}>
        <CollectionsSection />
      </MemoryRouter>
    </QueryClientProvider>
  );
}

beforeEach(() => {
  try {
    localStorage.clear();
  } catch {
    // jsdom always provides it; guarded to match the app's own access style.
  }
});
afterEach(cleanup);

describe("CollectionsSection routing", () => {
  it("shows the list at /collections", async () => {
    render(renderAt("/collections"));
    expect(await screen.findByRole("heading", { name: "Collections" })).toBeTruthy();
  });

  it("opens the detail page for a well-formed collection id", async () => {
    render(renderAt(`/collections/${COLLECTION_ID}`));
    expect(await screen.findByRole("heading", { name: "database_dev", level: 1 })).toBeTruthy();
  });

  it("falls back to the list when the id is not a UUID", async () => {
    render(renderAt("/collections/not-a-uuid"));
    expect(await screen.findByRole("heading", { name: "Collections" })).toBeTruthy();
  });

  it("falls back to the list for a path deeper than one collection", async () => {
    render(renderAt(`/collections/${COLLECTION_ID}/folders/extra`));
    expect(await screen.findByRole("heading", { name: "Collections" })).toBeTruthy();
  });
});
