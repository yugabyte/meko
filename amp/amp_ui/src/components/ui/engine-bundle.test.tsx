import { describe, it, expect, vi, afterEach } from "vitest";
import { render, screen, cleanup, fireEvent } from "@testing-library/react";
import { ExtensionsField } from "@/components/ui/extensions-field";
import { engineExtensions } from "@/components/ui/engine-field";

afterEach(cleanup);

// What an engine's bundle renders as in the picker. This is ExtensionsField
// given a bundle, NOT the wizard switching between engines — the recompute on
// switch lives in CreateClusterWizard and is not covered here.
describe("engine bundles in the extensions picker", () => {
  it("documentdb's bundle is pgvector + pg_cron alongside documentdb", () => {
    expect(engineExtensions("documentdb")).toEqual(["documentdb", "pgvector", "pg_cron"]);
    expect(engineExtensions("postgres")).toEqual([]);
  });

  it("the postgres bundle leaves everything unchecked and free", () => {
    render(
      <ExtensionsField value={[]} onChange={vi.fn()} enabled={engineExtensions("postgres")} />,
    );
    for (const n of ["documentdb", "pgvector", "pg_cron"]) {
      const b = screen.getByRole("checkbox", { name: n });
      expect(b.getAttribute("aria-checked")).toBe("false");
      expect((b as HTMLButtonElement).disabled).toBe(false);
    }
  });

  it("the documentdb bundle renders its three checked AND locked", () => {
    const onChange = vi.fn();
    render(
      <ExtensionsField value={[]} onChange={onChange} enabled={engineExtensions("documentdb")} />,
    );
    for (const n of ["documentdb", "pgvector", "pg_cron"]) {
      const b = screen.getByRole("checkbox", { name: n });
      expect(b.getAttribute("aria-checked")).toBe("true");
      expect((b as HTMLButtonElement).disabled).toBe(true);
      fireEvent.click(b);
    }
    expect(onChange).not.toHaveBeenCalled();
    // age is not in the bundle, so it stays free
    const age = screen.getByRole("checkbox", { name: "age" });
    expect(age.getAttribute("aria-checked")).toBe("false");
    expect((age as HTMLButtonElement).disabled).toBe(false);
  });
});
