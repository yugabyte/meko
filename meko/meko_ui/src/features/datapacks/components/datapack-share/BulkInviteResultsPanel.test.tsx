// Component test for BulkInviteResultsPanel — the table that surfaces
// per-address outcomes after a bulk-share POST. This is the piece the
// user actually reads when one out of ten emails fails, so the
// contract we're locking is:
//   1. One <tr> per result, keyed by email (server may return fewer
//      rows than were sent — dedup happens server-side, cf. #618).
//   2. The email string is rendered verbatim in column 1.
//   3. Column 2 is the human status label from the classifier.
//   4. Column 3 is `r.message` if the server sent one, else the
//      classifier's static fallback. The BE deliberately populates
//      `message` for every non-success outcome, so this is where
//      per-address error text lives.
import { afterEach, describe, expect, it } from "vitest";
import { cleanup, render, screen, within } from "@testing-library/react";

import { BulkInviteResultsPanel } from "./BulkInviteResultsPanel";
import { summarizeBulkShareResults } from "./bulkShareEmails";
import type { DatapackBulkShareResult } from "@/lib/api/mekoApi";

afterEach(cleanup);

function panel(results: DatapackBulkShareResult[]) {
  return <BulkInviteResultsPanel results={results} summary={summarizeBulkShareResults(results)} />;
}

/** Locate a table row by the email in column 1. */
function rowFor(email: string): HTMLElement {
  const cell = screen.getByText(email);
  const row = cell.closest("tr");
  if (!row) throw new Error(`no <tr> ancestor for ${email}`);
  return row as HTMLElement;
}

describe("BulkInviteResultsPanel", () => {
  it("renders one row per result, keyed by email", () => {
    render(
      panel([
        { email: "a@x.com", status: "invited", invite_id: "inv-1" },
        {
          email: "b@x.com",
          status: "already_member",
          message: "already has access to this datapack",
        },
        { email: "c@x.com", status: "invalid_email", message: "not a valid email address" },
      ]),
    );

    // Header + 3 data rows.
    const rows = screen.getAllByRole("row");
    expect(rows).toHaveLength(4);

    // Each email appears verbatim.
    expect(screen.getByText("a@x.com")).toBeDefined();
    expect(screen.getByText("b@x.com")).toBeDefined();
    expect(screen.getByText("c@x.com")).toBeDefined();
  });

  it("attaches the API's per-address message to the Status cell as a hover title", () => {
    render(
      panel([
        {
          email: "dave@x.com",
          status: "error",
          message: "failed to send invite, please try again or contact support",
        },
      ]),
    );

    const row = rowFor("dave@x.com");
    const cells = within(row).getAllByRole("cell");
    // 2 columns now: email · status. The Detail column was removed
    // for visual noise; per-address messages live on the Status
    // cell's `title` so operators can still hover to see them.
    expect(cells).toHaveLength(2);
    const statusCell = cells[1];
    expect(statusCell.getAttribute("title")).toBe(
      "failed to send invite, please try again or contact support",
    );
  });

  it("falls back to the classifier's static detail on the Status hover when message is empty", () => {
    render(panel([{ email: "erin@x.com", status: "invited", invite_id: "inv-2" }]));

    const row = rowFor("erin@x.com");
    const cells = within(row).getAllByRole("cell");
    expect(cells).toHaveLength(2);
    // Status cell has a title even without an API message — the
    // static classifier detail takes over (empty string would mean
    // hovering shows nothing, which is worse than a generic hint).
    const title = cells[1].getAttribute("title") ?? "";
    expect(title.length).toBeGreaterThan(0);
  });

  it("renders only the status label in the visible cell (message is in the title, not the DOM text)", () => {
    render(
      panel([
        {
          email: "frank@x.com",
          status: "already_member",
          message: "already has access to this datapack",
        },
      ]),
    );

    const row = rowFor("frank@x.com");
    // Visible label — pinned exactly so a copy tweak trips the test.
    expect(within(row).getByText("Already a member")).toBeDefined();
    // The API message is on the hover title, not in the visible row
    // text — no separate cell shows it.
    expect(within(row).queryByText("already has access to this datapack")).toBeNull();
    const statusCell = within(row).getAllByRole("cell")[1];
    expect(statusCell.getAttribute("title")).toBe("already has access to this datapack");
  });

  it("distinguishes invited vs invited_new_user in the summary line", () => {
    render(
      panel([
        { email: "a@x.com", status: "invited", invite_id: "i1" },
        { email: "b@x.com", status: "invited_new_user", invite_id: "i2" },
      ]),
    );

    // The summary badge counts both as "invited" total (2). The
    // no-Meko-account callout in the summary strip names the 1
    // new-user row separately — this is the copy the operator scans
    // to decide whether to nudge people to sign up.
    expect(screen.getByText(/2 invited/i)).toBeDefined();
    expect(screen.getByText(/no.+Meko account yet/i)).toBeDefined();
  });

  it("does not include a duplicate row for the same email", () => {
    // Server de-duplicates case-insensitively, so N inputs can return
    // fewer than N rows. The panel just reflects what came back;
    // there should never be two rows with the same email string.
    render(panel([{ email: "unique@x.com", status: "invited", invite_id: "i1" }]));

    const email = screen.getAllByText("unique@x.com");
    expect(email).toHaveLength(1);
  });
});
