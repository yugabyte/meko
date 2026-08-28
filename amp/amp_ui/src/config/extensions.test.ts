import { describe, it, expect } from "vitest";
import { execFileSync } from "node:child_process";
import { fileURLToPath } from "node:url";
import path from "node:path";
import catalog from "./extensions.json";

// The picker's catalog is generated from deploy/extensions/extensions.yaml. The
// generator's --check mode re-derives it and diffs, so adding an extension to
// the manifest without regenerating fails here rather than silently shipping a
// picker that can't offer it.
const uiRoot = path.resolve(path.dirname(fileURLToPath(import.meta.url)), "../..");

describe("extension catalog", () => {
  it("is in step with deploy/extensions/extensions.yaml", () => {
    expect(() =>
      execFileSync("python3", ["scripts/gen-extensions.py", "--check"], {
        cwd: uiRoot,
        encoding: "utf8",
        stdio: "pipe",
      }),
    ).not.toThrow();
  });

  it("carries a name, version and description for every entry", () => {
    expect(catalog.extensions.length).toBeGreaterThan(0);
    for (const e of catalog.extensions) {
      expect(e.name).toBeTruthy();
      expect(e.version).toBeTruthy();
      expect(e.description).toBeTruthy();
    }
  });
});
