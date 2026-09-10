import { afterEach, describe, expect, it, vi } from "vitest";

import { uploadFileToS3 } from "./uploadFileToS3";

function file(name: string, body: string): File {
  return new File([new TextEncoder().encode(body)], name);
}

/** Minimal controllable stand-in for the browser's XMLHttpRequest: captures
 *  what uploadFileToS3 actually sends, and lets tests drive its lifecycle
 *  events synchronously instead of hitting a real network. */
class FakeXHR {
  static instances: FakeXHR[] = [];
  method = "";
  url = "";
  status = 0;
  statusText = "";
  responseText = "";
  sentBody: FormData | null = null;
  upload: { onprogress: ((e: ProgressEvent) => void) | null; onload: (() => void) | null } = {
    onprogress: null,
    onload: null,
  };
  onload: (() => void) | null = null;
  onerror: (() => void) | null = null;
  onabort: (() => void) | null = null;
  aborted = false;

  constructor() {
    FakeXHR.instances.push(this);
  }
  open(method: string, url: string) {
    this.method = method;
    this.url = url;
  }
  send(body: FormData) {
    this.sentBody = body;
  }
  abort() {
    this.aborted = true;
    this.onabort?.();
  }
}

function install(): void {
  FakeXHR.instances = [];
  vi.stubGlobal("XMLHttpRequest", FakeXHR);
}

/** verifyFileContent resolves through several real async steps (a file
 *  read, a dynamic import) before uploadFileToS3 ever constructs its XHR, so
 *  a fixed number of microtask flushes is fragile. Polls instead. */
async function waitForXHR(): Promise<FakeXHR> {
  await vi.waitFor(() => {
    if (FakeXHR.instances.length === 0) throw new Error("XHR not constructed yet");
  });
  return FakeXHR.instances[0]!;
}

afterEach(() => {
  vi.unstubAllGlobals();
});

describe("uploadFileToS3", () => {
  it("sends every signed field, with the file part last", async () => {
    install();
    const promise = uploadFileToS3(
      "https://bucket.example.com",
      { key: "u/dp/hash/report.txt", policy: "eyJ...", "x-amz-signature": "abcd" },
      file("report.txt", "hello world"),
    );

    const xhr = await waitForXHR();
    const entries = Array.from(xhr.sentBody!.entries());
    expect(entries.map(([k]) => k)).toEqual(["key", "policy", "x-amz-signature", "file"]);
    expect(entries.at(-1)![0]).toBe("file");

    xhr.status = 204;
    xhr.onload?.();
    await expect(promise).resolves.toBeUndefined();
  });

  it("resolves on 204, not just the 200-299 range's conventional 200", async () => {
    install();
    const promise = uploadFileToS3("https://bucket.example.com", {}, file("a.txt", "abc"));

    const xhr = await waitForXHR();
    xhr.status = 204;
    xhr.onload?.();
    await expect(promise).resolves.toBeUndefined();
  });

  it("translates S3's EntityTooLarge into an actionable message", async () => {
    install();
    const promise = uploadFileToS3("https://bucket.example.com", {}, file("a.txt", "abc"));

    const xhr = await waitForXHR();
    xhr.status = 400;
    xhr.statusText = "Bad Request";
    xhr.responseText = "<Error><Code>EntityTooLarge</Code></Error>";
    xhr.onload?.();
    await expect(promise).rejects.toThrow("larger than your plan allows");
  });

  it("falls back to the raw status for an unrecognized S3 error code", async () => {
    install();
    const promise = uploadFileToS3("https://bucket.example.com", {}, file("a.txt", "abc"));

    const xhr = await waitForXHR();
    xhr.status = 500;
    xhr.statusText = "Internal Server Error";
    xhr.responseText = "oops";
    xhr.onload?.();
    await expect(promise).rejects.toThrow("500 Internal Server Error: oops");
  });

  it("rejects a file that fails content verification before ever touching XHR", async () => {
    install();
    // .exe isn't an accepted KB extension, so this must be refused by the
    // pre-transfer sniff, with no request ever opened.
    await expect(
      uploadFileToS3("https://bucket.example.com", {}, file("virus.exe", "MZ...")),
    ).rejects.toThrow();
    expect(FakeXHR.instances).toHaveLength(0);
  });
});
