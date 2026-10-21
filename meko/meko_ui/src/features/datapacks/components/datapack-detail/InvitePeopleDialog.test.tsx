// InvitePeopleDialog is a thin wrapper around InviteHumansSection — its own
// behaviour (bulk vs. single routing, self-filter, telemetry, 404 fallback)
// is pinned by InviteHumansSection.test.tsx. These two cases pin only what
// the wrapper owns: that opening the dialog mounts the real invite form,
// and that the trigger controls open-state.

import { afterEach, beforeAll, describe, expect, it, vi } from "vitest";
import { cleanup, render, screen } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";

beforeAll(() => {
  global.ResizeObserver = class {
    observe() {}
    unobserve() {}
    disconnect() {}
  };
  Element.prototype.scrollIntoView = vi.fn();
  Element.prototype.hasPointerCapture = vi.fn(() => false);
  Element.prototype.setPointerCapture = vi.fn();
  Element.prototype.releasePointerCapture = vi.fn();
});

import { InvitePeopleDialog } from "./InvitePeopleDialog";

function renderDialog(open: boolean) {
  render(
    <MemoryRouter>
      <InvitePeopleDialog
        open={open}
        onOpenChange={vi.fn()}
        datapackId="dp-1"
        currentUserEmail="me@example.com"
        sharedByRole="owner"
      />
    </MemoryRouter>,
  );
}

afterEach(cleanup);

describe("InvitePeopleDialog", () => {
  it("mounts InviteHumansSection — bulk-capable form, same component the Share page uses", () => {
    renderDialog(true);
    // The three role tiles (Viewer / Contributor / Maintainer) are rendered
    // by InviteHumansSection's InviteRoleTiles, not by the Dialog wrapper, so
    // asserting them proves the real form actually mounted (the wrapper's
    // DialogTitle also matches /Invite people/, which is why that alone
    // doesn't prove the mount).
    expect(screen.getByText(/^Viewer$/)).toBeTruthy();
    expect(screen.getByText(/^Contributor$/)).toBeTruthy();
    expect(screen.getByText(/^Maintainer$/)).toBeTruthy();
    expect(screen.getByRole("button", { name: /Send invite/i })).toBeTruthy();
  });

  it("renders nothing when closed", () => {
    renderDialog(false);
    expect(screen.queryByRole("button", { name: /Send invite/i })).toBeNull();
  });
});
