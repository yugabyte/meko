// The pending share link is what brings a visitor back to the link after an
// OAuth / OIDC sign-in that drops return_to.
import { afterEach, describe, expect, it } from "vitest";

import { shareTokenFromPath } from "@/app/routes/paths";
import {
  PENDING_SHARE_TTL_MS,
  clearPendingShareToken,
  pendingSharePath,
  savePendingShareToken,
} from "@/lib/pendingShareLink";

afterEach(() => sessionStorage.clear());

describe("pending share link", () => {
  it("resumes the saved link until it is cleared", () => {
    expect(pendingSharePath()).toBe("");
    savePendingShareToken("abc_-123");
    expect(pendingSharePath()).toBe("/share/abc_-123");
    clearPendingShareToken();
    expect(pendingSharePath()).toBe("");
  });

  it("expires, so an abandoned sign-in can't send a later one to the link", () => {
    savePendingShareToken("abc", 1_000);
    expect(pendingSharePath(1_000 + PENDING_SHARE_TTL_MS - 1)).toBe("/share/abc");
    expect(pendingSharePath(1_000 + PENDING_SHARE_TTL_MS)).toBe("");
    // An expired entry is removed, not just ignored.
    expect(pendingSharePath(1_000)).toBe("");
  });
});

describe("shareTokenFromPath", () => {
  it("reads the token from a share path, with or without a trailing slash", () => {
    expect(shareTokenFromPath("/share/abc_-123")).toBe("abc_-123");
    expect(shareTokenFromPath("/share/abc/")).toBe("abc");
  });

  it("is null for every other path, and for a malformed escape", () => {
    for (const p of [
      "/share",
      "/share/",
      "/share/a/b",
      "/datapacks/share/abc",
      "/",
      "/share/%E0",
      "/share/%",
    ]) {
      expect(shareTokenFromPath(p)).toBeNull();
    }
  });
});
