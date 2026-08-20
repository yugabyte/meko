import { describe, it, expect } from "vitest";
import { objectOf, rowsOf, valueOf } from "./SettingsRows";

// These decide what actually reaches a daemon's config file, so each branch is
// pinned rather than left to the dialog's integration tests.
describe("objectOf", () => {
  it("refuses a named setting with no value", () => {
    const r = objectOf([{ key: "work_mem", value: "  " }], "string");
    expect(r.ok).toBe(false);
    if (!r.ok) expect(r.error).toEqual({ kind: "blank", key: "work_mem" });
  });

  it("refuses duplicate names rather than silently keeping the last", () => {
    const r = objectOf(
      [
        { key: "work_mem", value: "64MB" },
        { key: "work_mem", value: "128MB" },
      ],
      "string",
    );
    expect(r.ok).toBe(false);
    if (!r.ok) expect(r.error).toEqual({ kind: "duplicate", key: "work_mem" });
  });

  it("ignores a row the operator has not named yet", () => {
    const r = objectOf([{ key: "  ", value: "" }], "string");
    expect(r).toEqual({ ok: true, value: {} });
  });

  it("refuses a lease value that is not a number", () => {
    const r = objectOf([{ key: "ttl_ms", value: "soon" }], "number");
    expect(r.ok).toBe(false);
    if (!r.ok) expect(r.error.kind).toBe("type");
  });
});

describe("value typing", () => {
  it("keeps a stored string a string, even when it looks numeric", () => {
    const [row] = rowsOf({ yb_payload_compression: "none" });
    expect(valueOf({ ...row, value: "123" }, "scalar")).toBe("123");
  });

  it("infers bool and number for rows the operator adds", () => {
    expect(valueOf({ key: "a", value: "true" }, "scalar")).toBe(true);
    expect(valueOf({ key: "a", value: "8" }, "scalar")).toBe(8);
    expect(valueOf({ key: "a", value: "7 days" }, "scalar")).toBe("7 days");
  });

  it("sorts rows so the list does not reshuffle between openings", () => {
    expect(rowsOf({ b: "1", a: "2" }).map((r) => r.key)).toEqual(["a", "b"]);
  });
});
