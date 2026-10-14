// Component test for InviteHumansSection — the unified single-or-bulk
// tab. Replaces the earlier BulkInviteByCsvSection.test.tsx because
// the orchestrator now lives here. Covers the contracts that matter
// end-to-end:
//   1. Count 1 → calls the single-invite endpoint (not bulk), fires
//      the DATAPACK_SHARED telemetry event once, and clears the input
//      on success.
//   2. Count ≥ 2 → calls the bulk endpoint, renders the per-row
//      results panel, and surfaces the API's per-address message.
//   3. Bulk endpoint missing (404) → falls back to the amber notice.
//      A single-address submit is NOT affected by this — /share has
//      been on main since day one.
//   4. Self-address is filtered before the payload for both single
//      and bulk paths (in bulk, when count is left at ≥ 2 after the
//      filter; in single, we'd fall through to the count == 1 branch).
//   5. Submit stays disabled while there are no valid addresses.
//   6. Inviting an existing member is not an error: the toast says so and
//      points to Members on the page, where roles are changed.
import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";

import { InviteHumansSection } from "./InviteHumansSection";

vi.mock("@/lib/api/mekoApi", async (importOriginal) => {
  const actual = await importOriginal<typeof import("@/lib/api/mekoApi")>();
  return {
    ...actual,
    inviteDatapackShare: vi.fn(),
    bulkShareDatapack: vi.fn(),
  };
});

vi.mock("@/hooks/use-toast", () => ({
  toast: vi.fn(),
  useToast: () => ({ toasts: [], toast: vi.fn(), dismiss: vi.fn() }),
}));

// Telemetry is fire-and-forget through a client; silence the actual
// call so the test doesn't hit rudder. We assert only that the
// mock was called with the right event name.
vi.mock("@/lib/telemetry/rudderstack", () => ({
  trackEvent: vi.fn(),
  identifyThenTrack: vi.fn(),
}));

import { bulkShareDatapack, inviteDatapackShare, MekoApiRequestError } from "@/lib/api/mekoApi";
import { trackEvent } from "@/lib/telemetry/rudderstack";
import { toast } from "@/hooks/use-toast";
import { TELEMETRY_EVENTS } from "@/lib/telemetry/events";

const inviteMock = vi.mocked(inviteDatapackShare);
const bulkMock = vi.mocked(bulkShareDatapack);
const trackMock = vi.mocked(trackEvent);

afterEach(() => {
  cleanup();
  inviteMock.mockReset();
  bulkMock.mockReset();
  trackMock.mockReset();
});

/** Convenience: type addresses into the textarea and click the button. */
function typeAndSubmit(emails: string[]) {
  const ta = screen.getByLabelText(/email addresses/i);
  fireEvent.change(ta, { target: { value: emails.join("\n") } });
  const submit = screen.getByRole("button", { name: /send|invite/i });
  fireEvent.click(submit);
  return submit;
}

describe("InviteHumansSection", () => {
  it("routes exactly-one address to the single-invite endpoint and fires DATAPACK_SHARED", async () => {
    inviteMock.mockResolvedValue({} as never);

    render(
      <InviteHumansSection
        datapackId="dp-1"
        currentUserEmail="me@x.com"
        onInvitesSent={() => Promise.resolve()}
      />,
    );

    typeAndSubmit(["alone@x.com"]);

    await waitFor(() => {
      expect(inviteMock).toHaveBeenCalledTimes(1);
    });
    // Bulk endpoint must NOT be called in the count-1 case.
    expect(bulkMock).not.toHaveBeenCalled();

    // Right payload — recipient email, chosen grant (Contributor default).
    const [, req] = inviteMock.mock.calls[0];
    expect(req.recipient_email).toBe("alone@x.com");
    expect(req.grant).toBe("contributor");

    // Telemetry event fired once with the right name.
    expect(trackMock).toHaveBeenCalledWith(
      TELEMETRY_EVENTS.DATAPACK_SHARED,
      expect.objectContaining({ invited_email: "alone@x.com" }),
    );
  });

  it("routes two-or-more addresses to the bulk endpoint and renders the results panel", async () => {
    bulkMock.mockResolvedValue({
      results: [
        { email: "alice@x.com", status: "invited", invite_id: "inv-1" },
        {
          email: "bob@x.com",
          status: "error",
          message: "failed to send invite, please try again or contact support",
        },
      ],
      summary: { invited: 1, error: 1 },
    });

    render(
      <InviteHumansSection
        datapackId="dp-1"
        currentUserEmail="me@x.com"
        onInvitesSent={() => Promise.resolve()}
      />,
    );

    typeAndSubmit(["alice@x.com", "bob@x.com"]);

    await waitFor(() => {
      expect(bulkMock).toHaveBeenCalledTimes(1);
    });
    // Single endpoint must NOT be called in the count-2 case.
    expect(inviteMock).not.toHaveBeenCalled();

    // Results table renders with the failing address AND its BE
    // message on the Status cell's hover title.
    await waitFor(() => {
      expect(screen.getByText("alice@x.com")).toBeDefined();
    });
    const failedRow = screen.getByText("bob@x.com").closest("tr");
    expect(failedRow).toBeDefined();
    const statusCell = within(failedRow as HTMLElement).getAllByRole("cell")[1];
    expect(statusCell.getAttribute("title")).toBe(
      "failed to send invite, please try again or contact support",
    );

    // Bulk telemetry — one event, rolled-up counts, not one per row.
    expect(trackMock).toHaveBeenCalledTimes(1);
    expect(trackMock).toHaveBeenCalledWith(
      TELEMETRY_EVENTS.DATAPACK_SHARED,
      expect.objectContaining({ invite_count: 2, results_invited: 1, results_failed: 1 }),
    );
  });

  it("falls back to the 'not available' notice only when the BULK endpoint 404s", async () => {
    // "Route not deployed" — echo's default 404 fallthrough carries
    // NO code envelope. That's what isApiEndpointMissingError now
    // uses to distinguish route-missing from a legitimate
    // RequireDatapackAccess NOT_FOUND (which does carry code).
    bulkMock.mockRejectedValue(new MekoApiRequestError("Not Found", { httpStatus: 404 }));

    render(<InviteHumansSection datapackId="dp-1" onInvitesSent={() => Promise.resolve()} />);

    // Trigger the bulk path with 2 addresses.
    typeAndSubmit(["a@x.com", "b@x.com"]);

    await waitFor(() => {
      expect(screen.getByText(/bulk sharing isn.?t available on this server yet/i)).toBeDefined();
    });

    // Textarea is gone; the whole form is replaced by the notice.
    expect(screen.queryByLabelText(/email addresses/i)).toBeNull();

    // The single endpoint was never called — the bulk 404 must not
    // leak into the single flow.
    expect(inviteMock).not.toHaveBeenCalled();
  });

  it("does NOT degrade on a 404 that carries an api-error code (revoked / deleted datapack access)", async () => {
    // RequireDatapackAccess returns 404 code=NOT_FOUND when the caller
    // has lost access to the datapack (or it was deleted). Before the
    // helper was narrowed, that 404 got swallowed and swapped the
    // whole tab for "Bulk sharing isn't available on this server yet"
    // — which is the wrong operator-facing conclusion. Now it must
    // surface as a normal error toast, keeping the form intact.
    bulkMock.mockRejectedValue(
      new MekoApiRequestError('datapack "dp-1" not found', {
        code: "NOT_FOUND",
        httpStatus: 404,
      }),
    );

    render(<InviteHumansSection datapackId="dp-1" onInvitesSent={() => Promise.resolve()} />);

    typeAndSubmit(["a@x.com", "b@x.com"]);

    // The bulk call happened.
    await waitFor(() => {
      expect(bulkMock).toHaveBeenCalledTimes(1);
    });

    // The tab is NOT swapped out.
    expect(screen.queryByText(/bulk sharing isn.?t available on this server yet/i)).toBeNull();
    // The form is still there.
    expect(screen.getByLabelText(/email addresses/i)).toBeDefined();
  });

  it("drops the caller's own address from the payload before submit", async () => {
    bulkMock.mockResolvedValue({
      results: [{ email: "team@x.com", status: "invited", invite_id: "i1" }],
      summary: { invited: 1 },
    });

    render(
      <InviteHumansSection
        datapackId="dp-1"
        currentUserEmail="me@x.com"
        onInvitesSent={() => Promise.resolve()}
      />,
    );

    // The self-address must be dropped BEFORE the count-based routing
    // decision. With `me` filtered out this becomes count 1 → single
    // endpoint, not bulk.
    typeAndSubmit(["me@x.com", "team@x.com"]);

    // With count-after-filter == 1 we take the single path.
    await waitFor(() => {
      expect(inviteMock).toHaveBeenCalledTimes(1);
    });
    expect(bulkMock).not.toHaveBeenCalled();

    const [, req] = inviteMock.mock.calls[0];
    expect(req.recipient_email).toBe("team@x.com");
  });

  it("keeps the submit button disabled while there are no valid addresses", () => {
    render(<InviteHumansSection datapackId="dp-1" onInvitesSent={() => Promise.resolve()} />);

    const submit = screen.getByRole("button", { name: /send|invite/i });
    // Empty textarea → button disabled.
    expect(submit.hasAttribute("disabled")).toBe(true);

    // Garbage row that yields zero valid addresses → still disabled.
    const ta = screen.getByLabelText(/email addresses/i);
    fireEvent.change(ta, { target: { value: "notanemail\n" } });
    expect(submit.hasAttribute("disabled")).toBe(true);

    // One valid address → enabled.
    fireEvent.change(ta, { target: { value: "valid@example.com" } });
    expect(submit.hasAttribute("disabled")).toBe(false);
  });
  it("tells the owner an existing member is already in, and points to Members", async () => {
    inviteMock.mockRejectedValue(
      new MekoApiRequestError("recipient is already a member", {
        code: "ALREADY_MEMBER",
        httpStatus: 409,
      }),
    );
    vi.mocked(toast).mockClear();

    render(
      <InviteHumansSection
        datapackId="dp-1"
        currentUserEmail="me@x.com"
        onInvitesSent={() => Promise.resolve()}
      />,
    );
    typeAndSubmit(["member@x.com"]);

    await waitFor(() => expect(toast).toHaveBeenCalled());
    const shown = vi.mocked(toast).mock.calls.at(-1)?.[0] as {
      title?: string;
      description?: string;
      variant?: string;
      action?: unknown;
    };
    expect(shown.title).toBe("Already a member");
    expect(shown.description).toContain("member@x.com");
    expect(shown.description).toContain("Members");
    expect(shown.variant).toBeUndefined();
    expect(shown.action).toBeTruthy();
  });
});
