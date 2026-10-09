import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { notifyAuthExpiredOnce } from "@/lib/api/mekoApi";
import { InvokeDeploymentError, invokeDeployment } from "./deploymentInvokeApi";

vi.mock("@/lib/api/mekoApi", () => ({ notifyAuthExpiredOnce: vi.fn() }));
const mockNotifyAuthExpired = vi.mocked(notifyAuthExpiredOnce);

/** Builds a minimal fetch Response stand-in whose body is a real
 *  ReadableStream<Uint8Array> emitting `chunks` (already SSE-framed
 *  strings) one at a time — close enough to a real streamed fetch to
 *  exercise the reader/decoder/buffer-splitting logic for real. */
function fakeStreamingResponse(chunks: string[], status = 200) {
  const encoder = new TextEncoder();
  const body = new ReadableStream<Uint8Array>({
    start(controller) {
      for (const chunk of chunks) controller.enqueue(encoder.encode(chunk));
      controller.close();
    },
  });
  return { ok: status >= 200 && status < 300, status, body, text: async () => "" } as Response;
}

function fakeErrorResponse(status: number, jsonBody: unknown) {
  return {
    ok: false,
    status,
    body: null,
    text: async () => JSON.stringify(jsonBody),
  } as unknown as Response;
}

async function collect<T>(gen: AsyncGenerator<T>): Promise<T[]> {
  const out: T[] = [];
  for await (const item of gen) out.push(item);
  return out;
}

describe("invokeDeployment", () => {
  beforeEach(() => {
    vi.stubGlobal("fetch", vi.fn());
    mockNotifyAuthExpired.mockClear();
  });
  afterEach(() => {
    vi.unstubAllGlobals();
  });

  it("yields events parsed from SSE frames, even when a frame arrives split across chunks", async () => {
    vi.mocked(fetch).mockResolvedValue(
      fakeStreamingResponse([
        'data: {"type":"meta","conversation_id":"conv-1"}\n\ndata: {"type":"text_del',
        'ta","text":"hello"}\n\ndata: {"type":"done","review":"hello"}\n\n',
      ]),
    );

    const events = await collect(invokeDeployment("dep-1", { foo: "bar" }));

    expect(events).toEqual([
      { type: "meta", conversation_id: "conv-1" },
      { type: "text_delta", text: "hello" },
      { type: "done", review: "hello" },
    ]);
  });

  it("sends the deployment id and body to the invoke endpoint", async () => {
    vi.mocked(fetch).mockResolvedValue(
      fakeStreamingResponse(['data: {"type":"done","review":"ok"}\n\n']),
    );

    await collect(invokeDeployment("dep-1", { pr_title: "x" }));

    const [url, init] = vi.mocked(fetch).mock.calls[0];
    expect(String(url)).toContain("/deployments/dep-1/invoke");
    expect(init?.credentials).toBe("include");
    expect(JSON.parse(init?.body as string)).toEqual({ pr_title: "x" });
  });

  it("throws on a non-2xx response, extracting api_server's error envelope", async () => {
    vi.mocked(fetch).mockResolvedValue(
      fakeErrorResponse(400, {
        error: { code: "VALIDATION_ERROR", message: "deployment not active" },
      }),
    );

    await expect(collect(invokeDeployment("dep-1", {}))).rejects.toThrow("deployment not active");
  });

  it("throws on a mid-stream error frame, preferring `error`/`error_type` over the SDK's generic `message`", async () => {
    // bedrock_agentcore's own _sync_stream_with_error_handling always sets
    // `message` to this same generic string regardless of what actually
    // failed -- the real detail is in `error` (str(exception)) and
    // `error_type` (the exception's class name). A fresh stream per call --
    // ReadableStream can only be read once, and both assertions below need
    // their own invocation.
    vi.mocked(fetch).mockImplementation(async () =>
      fakeStreamingResponse([
        'data: {"type":"meta","conversation_id":"conv-1"}\n\ndata: {"error":"boom","error_type":"RuntimeError","message":"An error occurred during streaming"}\n\n',
      ]),
    );

    await expect(collect(invokeDeployment("dep-1", {}))).rejects.toThrow(InvokeDeploymentError);
    await expect(collect(invokeDeployment("dep-1", {}))).rejects.toThrow("RuntimeError: boom");
  });

  it("falls back to parsing a plain (non-SSE) JSON error body from a stale deployed image", async () => {
    vi.mocked(fetch).mockResolvedValue(
      fakeStreamingResponse(['{"error": "Missing required field: datapack_id"}']),
    );

    await expect(collect(invokeDeployment("dep-1", {}))).rejects.toThrow(
      "Missing required field: datapack_id",
    );
  });

  it("throws a clear error on a plain (non-SSE) success-shaped body instead of silently yielding nothing", async () => {
    vi.mocked(fetch).mockResolvedValue(fakeStreamingResponse(['{"review": "looks good"}']));

    await expect(collect(invokeDeployment("dep-1", {}))).rejects.toThrow(
      /doesn't support streaming/,
    );
  });

  it("notifies auth-expired on a 401 before throwing", async () => {
    vi.mocked(fetch).mockResolvedValue(
      fakeErrorResponse(401, { error: { message: "Session expired" } }),
    );

    await expect(collect(invokeDeployment("dep-1", {}))).rejects.toThrow("Session expired");
    expect(mockNotifyAuthExpired).toHaveBeenCalledTimes(1);
  });

  it("flushes a final frame that isn't terminated by a trailing blank line", async () => {
    vi.mocked(fetch).mockResolvedValue(
      fakeStreamingResponse([
        'data: {"type":"meta","conversation_id":"conv-1"}\n\ndata: {"type":"done","review":"hello"}',
      ]),
    );

    const events = await collect(invokeDeployment("dep-1", {}));

    expect(events).toEqual([
      { type: "meta", conversation_id: "conv-1" },
      { type: "done", review: "hello" },
    ]);
  });

  it("silently drops a genuinely truncated final frame instead of throwing", async () => {
    vi.mocked(fetch).mockResolvedValue(
      fakeStreamingResponse([
        'data: {"type":"meta","conversation_id":"conv-1"}\n\ndata: {"type":"done","rev',
      ]),
    );

    const events = await collect(invokeDeployment("dep-1", {}));

    expect(events).toEqual([{ type: "meta", conversation_id: "conv-1" }]);
  });

  it("cancels the reader once the stream completes normally", async () => {
    vi.mocked(fetch).mockResolvedValue(
      fakeStreamingResponse(['data: {"type":"done","review":"ok"}\n\n']),
    );
    const cancelSpy = vi.spyOn(ReadableStreamDefaultReader.prototype, "cancel");

    await collect(invokeDeployment("dep-1", {}));

    expect(cancelSpy).toHaveBeenCalled();
    cancelSpy.mockRestore();
  });

  it("cancels the reader when the caller stops consuming early", async () => {
    vi.mocked(fetch).mockResolvedValue(
      fakeStreamingResponse([
        'data: {"type":"meta","conversation_id":"conv-1"}\n\ndata: {"type":"done","review":"ok"}\n\n',
      ]),
    );
    const cancelSpy = vi.spyOn(ReadableStreamDefaultReader.prototype, "cancel");

    for await (const _event of invokeDeployment("dep-1", {})) {
      break;
    }

    expect(cancelSpy).toHaveBeenCalled();
    cancelSpy.mockRestore();
  });
});
