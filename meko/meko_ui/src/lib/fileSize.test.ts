import { describe, it, expect } from "vitest";
import { formatFileSize, MB, GB, KiB, MiB, GiB } from "@/lib/fileSize";

describe("formatFileSize", () => {
  it("renders null / undefined / negative / non-finite as an em dash", () => {
    expect(formatFileSize(null)).toBe("—");
    expect(formatFileSize(undefined)).toBe("—");
    expect(formatFileSize(-1)).toBe("—");
    expect(formatFileSize(NaN)).toBe("—");
    expect(formatFileSize(Infinity)).toBe("—");
  });

  it("renders raw bytes below 1 KiB", () => {
    expect(formatFileSize(0)).toBe("0 B");
    expect(formatFileSize(900)).toBe("900 B");
    expect(formatFileSize(KiB - 1)).toBe("1,023 B");
  });

  it("switches units at each power-of-two boundary", () => {
    expect(formatFileSize(KiB)).toBe("1.0 KB");
    expect(formatFileSize(MiB)).toBe("1.0 MB");
    expect(formatFileSize(GiB)).toBe("1.0 GB");
  });

  it("uses one decimal below 10 and whole numbers at/above 10", () => {
    expect(formatFileSize(2048)).toBe("2.0 KB"); // 2 KiB, < 10 → 1 decimal
    expect(formatFileSize(15 * KiB)).toBe("15 KB"); // ≥ 10 → rounded
    expect(formatFileSize(3_500_000)).toBe("3.3 MB");
  });
});

describe("MB / GB threshold builders", () => {
  it("MB(n) is n mebibytes and GB(n) is n gibibytes", () => {
    expect(MB(1)).toBe(MiB);
    expect(MB(100)).toBe(100 * 1024 * 1024);
    expect(GB(2)).toBe(2 * GiB);
  });
});
