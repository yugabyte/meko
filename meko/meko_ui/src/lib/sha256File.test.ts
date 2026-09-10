import { describe, expect, it, vi } from "vitest";

import { isFileSnapshotError, sha256Hex } from "./sha256File";

/** Known-answer vector: SHA-256 of "abc". */
const ABC_SHA256 = "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad";

function file(name: string, body: string): File {
  return new File([new TextEncoder().encode(body)], name);
}

describe("sha256Hex", () => {
  it("hashes the file bytes", async () => {
    expect(await sha256Hex(file("a.txt", "abc"))).toBe(ABC_SHA256);
  });

  it("rejects a file that grew since it was selected", async () => {
    // A log still being appended to, or a download that hasn't finished. The
    // File's `size` is from the snapshot taken at selection, so more bytes
    // coming back than that is the tell -- and it is the only tell in Chrome,
    // which re-snapshots and serves the new bytes rather than failing the read.
    const f = file("growing.txt", "abc");
    Object.defineProperty(f, "size", { value: 1 });
    await expect(sha256Hex(f)).rejects.toThrow("changed while it was being read");
  });

  it("rejects a file that shrank since it was selected", async () => {
    const f = file("shrinking.txt", "abc");
    Object.defineProperty(f, "size", { value: 4096 });
    await expect(sha256Hex(f)).rejects.toThrow("changed while it was being read");
  });

  it("translates the browser's NotReadableError", async () => {
    const f = file("locked.txt", "abc");
    vi.spyOn(f, "arrayBuffer").mockRejectedValue(
      new DOMException("snapshot changed", "NotReadableError"),
    );
    await expect(sha256Hex(f)).rejects.toThrow("changed while it was being read");
  });

  it("lets an unrelated read failure through unchanged", async () => {
    const f = file("a.txt", "abc");
    vi.spyOn(f, "arrayBuffer").mockRejectedValue(new Error("disk on fire"));
    await expect(sha256Hex(f)).rejects.toThrow("disk on fire");
  });
});

describe("isFileSnapshotError", () => {
  it("matches only NotReadableError", () => {
    expect(isFileSnapshotError(new DOMException("x", "NotReadableError"))).toBe(true);
    expect(isFileSnapshotError(new DOMException("x", "NotFoundError"))).toBe(false);
    expect(isFileSnapshotError(new Error("NotReadableError"))).toBe(false);
    expect(isFileSnapshotError(undefined)).toBe(false);
  });
});
