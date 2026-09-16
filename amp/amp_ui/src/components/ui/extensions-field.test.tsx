import { describe, it, expect, vi, afterEach } from "vitest";
import { render, screen, cleanup, fireEvent } from "@testing-library/react";
import {
  ExtensionsField,
  BUNDLED_EXTENSIONS,
} from "@/components/ui/extensions-field";

// ExtensionsField is a multi-select over whatever catalog it is handed, with one
// twist: what is already enabled on the target is locked, because the API unions
// and offers no removal. These guard the toggle contract and that locking.
//
// The component stays presentation-only — the live catalog is fetched by its
// callers (useExtensionOptions), so these render it bare with no query client.
afterEach(cleanup);

describe("ExtensionsField", () => {
  it("renders every catalog extension as an unchecked checkbox by default", () => {
    render(<ExtensionsField value={[]} onChange={vi.fn()} />);
    const boxes = screen.getAllByRole("checkbox");
    expect(boxes).toHaveLength(BUNDLED_EXTENSIONS.length);
    for (const b of boxes) expect(b.getAttribute("aria-checked")).toBe("false");
  });

  it("adds a name on select and removes it on re-select", () => {
    const onChange = vi.fn();
    const { rerender } = render(<ExtensionsField value={[]} onChange={onChange} />);

    fireEvent.click(screen.getByRole("checkbox", { name: "pgvector" }));
    expect(onChange).toHaveBeenCalledWith(["pgvector"]);

    rerender(<ExtensionsField value={["pgvector"]} onChange={onChange} />);
    fireEvent.click(screen.getByRole("checkbox", { name: "pgvector" }));
    expect(onChange).toHaveBeenLastCalledWith([]);
  });

  it("accumulates multiple selections rather than replacing", () => {
    const onChange = vi.fn();
    render(<ExtensionsField value={["pgvector"]} onChange={onChange} />);
    fireEvent.click(screen.getByRole("checkbox", { name: "age" }));
    expect(onChange).toHaveBeenCalledWith(["pgvector", "age"]);
  });

  it("locks an already-enabled extension: checked, disabled, and inert", () => {
    const onChange = vi.fn();
    render(<ExtensionsField value={[]} onChange={onChange} enabled={["pg_cron"]} />);

    const cron = screen.getByRole("checkbox", { name: "pg_cron" });
    expect(cron.getAttribute("aria-checked")).toBe("true");
    expect((cron as HTMLButtonElement).disabled).toBe(true);

    fireEvent.click(cron);
    expect(onChange).not.toHaveBeenCalled();
  });

  it("flags the extensions whose preload forces a restart", () => {
    render(<ExtensionsField value={[]} onChange={vi.fn()} />);
    // pg_cron and hexabridge need shared_preload_libraries; pgvector and age don't.
    expect(screen.getAllByText(/needs restart/i)).toHaveLength(2);
  });

  // Creating a cluster has no compute yet, so the restart tag would be noise.
  it("drops the restart tag when showRestartTag is off", () => {
    render(<ExtensionsField value={[]} onChange={vi.fn()} showRestartTag={false} />);
    expect(screen.queryByText(/needs restart/i)).toBeNull();
  });

  // The point of fetching the catalog: an image an admin registered is not in
  // the bundled list, and must still be offered.
  it("renders a registered extension the bundled list does not contain", () => {
    const onChange = vi.fn();
    render(
      <ExtensionsField
        value={[]}
        onChange={onChange}
        options={[
          { name: "postgis", version: "3.6.2", description: "Spatial types", builtin: false },
        ]}
      />,
    );
    const gis = screen.getByRole("checkbox", { name: "postgis" });
    expect(gis).toBeTruthy();
    fireEvent.click(gis);
    expect(onChange).toHaveBeenCalledWith(["postgis"]);
  });
});
