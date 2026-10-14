// Share page Members card: shows a capped preview and links to the members
// page for the rest, so a datapack with thousands of link members stays usable.

import { afterEach, describe, expect, it } from "vitest";
import { cleanup, render, screen } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import type { DatapackOutgoingMember } from "@/lib/api/mekoApi";
import { MembersSection, SHARE_MEMBERS_PREVIEW_LIMIT } from "./MembersSection";

function members(n: number): DatapackOutgoingMember[] {
  return Array.from({ length: n }, (_, i) => ({
    user_id: `u${i}`,
    is_owner: i === 0,
    user_name: `Member ${i}`,
    user_email_id: `m${i}@x.com`,
    grant: "viewer",
  }));
}

function renderSection(list: DatapackOutgoingMember[]) {
  const qc = new QueryClient();
  render(
    <QueryClientProvider client={qc}>
      <MemoryRouter>
        <MembersSection
          currentUser={null}
          datapackName="dp"
          datapackId="dp-1"
          members={list}
          loading={false}
          onMembersChanged={() => {}}
        />
      </MemoryRouter>
    </QueryClientProvider>,
  );
}

afterEach(cleanup);

describe("MembersSection", () => {
  it("shows a preview and links to the members page when there are more", () => {
    renderSection(members(SHARE_MEMBERS_PREVIEW_LIMIT + 4));
    expect(screen.getAllByText(/^m\d+@x\.com$/)).toHaveLength(SHARE_MEMBERS_PREVIEW_LIMIT);
    const link = screen.getByRole("link", {
      name: `View all ${SHARE_MEMBERS_PREVIEW_LIMIT + 4} members →`,
    });
    expect(link.getAttribute("href")).toContain("dp-1");
  });

  it("has no View all link when everyone fits", () => {
    renderSection(members(3));
    expect(screen.queryByRole("link", { name: /View all/ })).toBeNull();
  });
});
