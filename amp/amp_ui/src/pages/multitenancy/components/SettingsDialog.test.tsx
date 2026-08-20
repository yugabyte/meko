import { describe, it, expect, afterEach, vi } from "vitest";
import { render, screen, cleanup, fireEvent } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { SettingsDialog } from "./SettingsDialog";

// The controller reads an omitted surface as "leave it alone" and `{}` as
// "clear it". If the dialog sent every surface on every save, editing the write
// lease would rewrite the branch's Postgres settings with whatever the dialog
// read when it opened — a destructive edit nobody asked for, and an invisible
// one.
afterEach(cleanup);

const updateBranch = vi.fn();
const updateEndpoint = vi.fn();

vi.mock("@/lib/api/queries", () => ({
  useUpdateBranchSettings: () => ({ mutate: updateBranch, isPending: false }),
}));
vi.mock("@/hooks/useEndpoints", () => ({
  useUpdateEndpointSettings: () => ({ mutate: updateEndpoint, isPending: false }),
}));

function view(props: {
  open?: boolean;
  endpointId?: string;
  stored?: Record<string, unknown>;
}) {
  return (
    <QueryClientProvider client={new QueryClient()}>
      <SettingsDialog
        open={props.open ?? true}
        onOpenChange={vi.fn()}
        poolId="pool-1"
        clusterId="cluster-1"
        branchId="branch-1"
        endpointId={props.endpointId}
        stored={props.stored}
        title="main"
      />
    </QueryClientProvider>
  );
}

const renderDialog = (props: Parameters<typeof view>[0] = {}) => render(view(props));

/** The nth value box of a surface, by its accessible label. */
const valueBox = (surface: string, n = 1) =>
  screen.getByLabelText(`settings-${surface} value ${n}`) as HTMLInputElement;
const nameBox = (surface: string, n = 1) =>
  screen.getByLabelText(`settings-${surface} name ${n}`) as HTMLInputElement;
const save = () => fireEvent.click(screen.getByRole("button", { name: /^save$/i }));

describe("SettingsDialog", () => {
  it("opens showing what is stored, as rows", () => {
    renderDialog({
      stored: {
        pg_settings: { work_mem: "64MB" },
        write_token_settings: { ttl_ms: 15000, renew_seconds: 5 },
      },
    });
    expect(nameBox("pg_settings").value).toBe("work_mem");
    expect(valueBox("pg_settings").value).toBe("64MB");
    // Sorted, so the list does not reshuffle between openings.
    expect(nameBox("write_token_settings", 1).value).toBe("renew_seconds");
    expect(nameBox("write_token_settings", 2).value).toBe("ttl_ms");
  });

  it("says nothing is set rather than showing an example", () => {
    renderDialog({ stored: {} });
    expect(screen.getAllByText(/nothing set/i).length).toBeGreaterThan(0);
    // No value box exists at all when a surface is empty.
    expect(screen.queryByLabelText("settings-pg_settings value 1")).toBeNull();
  });

  it("does not resend a surface the operator did not touch", () => {
    updateBranch.mockClear();
    renderDialog({
      stored: {
        pg_settings: { work_mem: "64MB" },
        write_token_settings: { ttl_ms: 15000, renew_seconds: 5 },
      },
    });
    fireEvent.change(valueBox("write_token_settings", 2), { target: { value: "20000" } });
    save();

    const { settings } = updateBranch.mock.calls[0][0];
    expect(settings).toEqual({
      write_token_settings: { renew_seconds: 5, ttl_ms: 20000 },
    });
    expect("pg_settings" in settings).toBe(false);
  });

  it("sends numbers for the lease, not strings", () => {
    updateBranch.mockClear();
    renderDialog({ stored: { write_token_settings: { ttl_ms: 15000 } } });
    fireEvent.change(valueBox("write_token_settings"), { target: { value: "30000" } });
    save();
    expect(updateBranch.mock.calls[0][0].settings.write_token_settings.ttl_ms).toBe(30000);
  });

  it("refuses a lease value that is not a number", () => {
    updateBranch.mockClear();
    renderDialog({ stored: { write_token_settings: { ttl_ms: 15000 } } });
    fireEvent.change(valueBox("write_token_settings"), { target: { value: "soon" } });
    save();
    expect(updateBranch).not.toHaveBeenCalled();
  });

  it("keeps a daemon value the type it was stored with", () => {
    updateEndpoint.mockClear();
    renderDialog({
      endpointId: "ep-1",
      stored: {
        pageserver_settings: {
          "tenant_config.enable_gc": true,
          "yb_payload_compression": "none",
        },
      },
    });
    // A string value that looks numeric must not become a number.
    fireEvent.change(valueBox("pageserver_settings", 2), { target: { value: "123" } });
    save();
    const sent = updateEndpoint.mock.calls[0][0].settings.pageserver_settings;
    expect(sent["yb_payload_compression"]).toBe("123");
    expect(sent["tenant_config.enable_gc"]).toBe(true);
  });

  it("infers a type for a setting the operator adds", () => {
    updateEndpoint.mockClear();
    renderDialog({ endpointId: "ep-1", stored: { pageserver_settings: {} } });
    fireEvent.click(
      screen.getAllByRole("button", { name: /add setting/i })[1], // pageserver
    );
    fireEvent.change(nameBox("pageserver_settings"), {
      target: { value: "tenant_config.enable_gc" },
    });
    fireEvent.change(valueBox("pageserver_settings"), { target: { value: "false" } });
    save();
    expect(
      updateEndpoint.mock.calls[0][0].settings.pageserver_settings[
        "tenant_config.enable_gc"
      ],
    ).toBe(false);
  });

  // A named setting with no value renders as `work_mem = ''` in
  // postgresql.conf, and Postgres refuses to start — on the restart this same
  // save triggers. Nothing downstream validates pg_settings values, so the
  // dialog is the only thing that can catch it.
  it("refuses a setting whose value was left blank", () => {
    updateBranch.mockClear();
    renderDialog({ stored: {} });
    fireEvent.click(screen.getAllByRole("button", { name: /add setting/i })[0]);
    fireEvent.change(nameBox("pg_settings"), { target: { value: "work_mem" } });
    save();
    expect(updateBranch).not.toHaveBeenCalled();
  });

  // Two rows with one name collapse last-wins, so the list the operator is
  // looking at would not be what got sent.
  it("refuses two rows with the same name", () => {
    updateBranch.mockClear();
    renderDialog({ stored: { pg_settings: { work_mem: "64MB" } } });
    fireEvent.click(screen.getAllByRole("button", { name: /add setting/i })[0]);
    fireEvent.change(nameBox("pg_settings", 2), { target: { value: "work_mem" } });
    fireEvent.change(valueBox("pg_settings", 2), { target: { value: "128MB" } });
    save();
    expect(updateBranch).not.toHaveBeenCalled();
  });

  it("treats removing every row as a clear", () => {
    updateBranch.mockClear();
    renderDialog({ stored: { pg_settings: { work_mem: "64MB" } } });
    fireEvent.click(screen.getByLabelText("settings-pg_settings remove 1"));
    save();
    expect(updateBranch.mock.calls[0][0].settings).toEqual({ pg_settings: {} });
  });

  it("does not call the API when nothing was edited", () => {
    updateBranch.mockClear();
    renderDialog({ stored: { pg_settings: { work_mem: "64MB" } } });
    save();
    expect(updateBranch).not.toHaveBeenCalled();
  });

  // The parent builds `stored` inline, so it is a new object on every parent
  // render. Re-reading on that identity would wipe half-typed input the moment
  // anything else re-rendered — a react-query refetch on focus is enough.
  it("keeps what the operator typed when the parent re-renders", () => {
    const stored = { pg_settings: { work_mem: "64MB" } };
    const { rerender } = renderDialog({ stored });
    fireEvent.change(valueBox("pg_settings"), { target: { value: "999MB" } });
    rerender(view({ stored: { pg_settings: { work_mem: "64MB" } } }));
    expect(valueBox("pg_settings").value).toBe("999MB");
  });
});
