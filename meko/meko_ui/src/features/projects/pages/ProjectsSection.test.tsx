// Routing rules for the Projects area: a well-formed id opens the detail
// page, and anything else falls back to the list rather than rendering a
// detail page bound to an id the API will reject.

import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import type { ReactElement } from "react";
import { cleanup, render, screen } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";

import ProjectsSection from "./ProjectsSection";

const PROJECT_ID = "11111111-2222-3333-4444-555555555555";

vi.mock("@/lib/api/mekoApi", async (o) => ({
  ...(await o<typeof import("@/lib/api/mekoApi")>()),
  getCurrentUser: vi.fn().mockResolvedValue({ account_id: "u-1", email: "a@b.c" }),
}));

vi.mock("@/lib/api/projectsApi", () => ({
  listProjects: vi.fn().mockResolvedValue([]),
  listProjectThemes: vi.fn().mockResolvedValue([]),
  listDatapackPaths: vi.fn().mockResolvedValue({}),
  // The id is inlined rather than referencing PROJECT_ID: vi.mock is hoisted
  // above the module's own consts.
  getProject: vi.fn().mockResolvedValue({
    project: {
      project_id: "11111111-2222-3333-4444-555555555555",
      user_id: "u-1",
      project_name: "database_dev",
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
  createProject: vi.fn(),
  updateProject: vi.fn(),
  deleteProject: vi.fn(),
  createProjectFolder: vi.fn(),
  renameProjectFolder: vi.fn(),
  deleteProjectFolder: vi.fn(),
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
        <ProjectsSection />
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

describe("ProjectsSection routing", () => {
  it("shows the list at /projects", async () => {
    render(renderAt("/projects"));
    expect(await screen.findByRole("heading", { name: "Projects" })).toBeTruthy();
  });

  it("opens the detail page for a well-formed project id", async () => {
    render(renderAt(`/projects/${PROJECT_ID}`));
    expect(await screen.findByRole("heading", { name: "database_dev", level: 1 })).toBeTruthy();
  });

  it("falls back to the list when the id is not a UUID", async () => {
    render(renderAt("/projects/not-a-uuid"));
    expect(await screen.findByRole("heading", { name: "Projects" })).toBeTruthy();
  });

  it("falls back to the list for a path deeper than one project", async () => {
    render(renderAt(`/projects/${PROJECT_ID}/folders/extra`));
    expect(await screen.findByRole("heading", { name: "Projects" })).toBeTruthy();
  });
});
