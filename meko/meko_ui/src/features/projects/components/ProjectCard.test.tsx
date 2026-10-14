// The project card's "Shared by" strip: shown on someone else's project with
// the owner's email, absent on the caller's own.

import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, render, screen, waitFor } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";

import { ProjectCard } from "./ProjectCard";
import type { Project } from "@/lib/api/projectsApi";
import { getCurrentUser } from "@/lib/api/mekoApi";

vi.mock("@/lib/api/mekoApi", async (o) => ({
  ...(await o<typeof import("@/lib/api/mekoApi")>()),
  getCurrentUser: vi.fn().mockResolvedValue({ account_id: "me", email: "me@example.com" }),
}));

function project(overrides: Partial<Project>): Project {
  return {
    project_id: "p-1",
    user_id: "me",
    project_name: "payments",
    theme_key: "adlc",
    theme_version: "1.0",
    folder_count: 1,
    datapack_count: 1,
    created_at: new Date().toISOString(),
    updated_at: new Date().toISOString(),
    ...overrides,
  };
}

function renderCard(p: Project) {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  render(
    <QueryClientProvider client={client}>
      <ProjectCard project={p} learningsCount={0} onOpen={() => {}} />
    </QueryClientProvider>,
  );
}

afterEach(cleanup);

describe("ProjectCard", () => {
  it("names the owner on a shared project", async () => {
    renderCard(
      project({ user_id: "alice", owner_name: "Alice", owner_email: "alice@example.com" }),
    );
    expect(await screen.findByText("alice@example.com")).toBeTruthy();
    expect(screen.getByText("Shared by")).toBeTruthy();
  });

  it("has no Shared by strip on the caller's own project", async () => {
    renderCard(project({}));
    // Wait for the user to load, or the strip is absent only because it hasn't.
    await waitFor(() => expect(getCurrentUser).toHaveBeenCalled());
    await new Promise((r) => setTimeout(r, 0));
    expect(screen.queryByText("Shared by")).toBeNull();
  });
});
