// The filter only appears once the list stops fitting, and the threshold is
// the thing worth pinning: an input that shows up over three rows is noise,
// and one that never shows up leaves a long list unusable.

import { afterEach, describe, expect, it } from "vitest";
import { cleanup, fireEvent, render, screen } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";

import { SidebarDatapackList } from "@/features/layout/SidebarDatapackList";
import {
  DATAPACK_FILTER_NO_MATCHES,
  DATAPACK_FILTER_PLACEHOLDER,
} from "@/features/layout/sidebarCopy";
import type { Datapack } from "@/lib/api/mekoApi";
import { DATAPACK_SHARE_GRANT_OWNER } from "@/lib/datapackShareGrants";

afterEach(cleanup);

// `grant` matters: without it a row renders the "Shared with you" icon, whose
// aria-label joins the link's accessible name. GET /datapacks always sets it.
function packs(names: string[]): Datapack[] {
  return names.map((datapack_name, i) => ({
    datapack_id: `dp-${i}`,
    datapack_name,
    grant: DATAPACK_SHARE_GRANT_OWNER,
  })) as Datapack[];
}

// Eight names, one over the seven-row cap, so the filter is showing.
const MANY = packs([
  "adlc-code-review-Oct7-2",
  "adlc-code-review-Oct7",
  "adlc-code-review-Sep24",
  "feature/tenant-usage-report",
  "adlc-code-review-Sep23",
  "adlc-code-review-demo-seed",
  "adlc-live-demo-India",
  "soccer-test",
]);

function renderList(datapacks: Datapack[]) {
  render(
    <MemoryRouter>
      <SidebarDatapackList datapacks={datapacks} activeDatapackId="" />
    </MemoryRouter>,
  );
}

describe("SidebarDatapackList filter", () => {
  it("stays out of the way while the list still fits", () => {
    renderList(packs(["alpha", "beta", "gamma"]));

    expect(screen.queryByPlaceholderText(DATAPACK_FILTER_PLACEHOLDER)).toBeNull();
    expect(screen.getByRole("link", { name: "alpha" })).toBeTruthy();
  });

  it("appears once the list is longer than the visible cap", () => {
    renderList(MANY);

    expect(screen.getByPlaceholderText(DATAPACK_FILTER_PLACEHOLDER)).toBeTruthy();
  });

  it("narrows the list to matching names, case-insensitively", () => {
    renderList(MANY);
    fireEvent.change(screen.getByPlaceholderText(DATAPACK_FILTER_PLACEHOLDER), {
      target: { value: "LIVE-DEMO" },
    });

    expect(screen.getByRole("link", { name: "adlc-live-demo-India" })).toBeTruthy();
    expect(screen.queryByRole("link", { name: "soccer-test" })).toBeNull();
  });

  it("renders nothing at all with no datapacks, filter or not", () => {
    // Both a new account and the first render before /datapacks resolves land
    // here with []. Treating that as "no matches" would put a filter message
    // under the Datapacks entry for every new user.
    renderList([]);

    expect(screen.queryByText(DATAPACK_FILTER_NO_MATCHES)).toBeNull();
    expect(screen.queryByPlaceholderText(DATAPACK_FILTER_PLACEHOLDER)).toBeNull();
  });

  it("says so when nothing matches, rather than showing an empty rail", () => {
    renderList(MANY);
    fireEvent.change(screen.getByPlaceholderText(DATAPACK_FILTER_PLACEHOLDER), {
      target: { value: "nothing-matches-this" },
    });

    expect(screen.getByText(DATAPACK_FILTER_NO_MATCHES)).toBeTruthy();
    // The input survives the empty result, or there is no way to undo the typo.
    expect(screen.getByPlaceholderText(DATAPACK_FILTER_PLACEHOLDER)).toBeTruthy();
  });
});
