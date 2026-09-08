// Streams a review through the deployed agent's AgentCore Runtime, proxied
// by api_server's POST /deployments/:id/invoke. axios (mekoApi.ts's client)
// can't read a streaming browser response body, so this is a standalone
// fetch()-based client rather than going through `request()` — deliberately
// not sharing mekoApi's axios instance, since this file's error/streaming
// semantics are different enough that coupling them would be more confusing
// than the small duplication below. `request()`'s 401-session-expired
// redirect isn't part of that streaming-specific logic, though, so it's
// reused directly (notifyAuthExpiredOnce) rather than left dropped.

import { notifyAuthExpiredOnce } from "@/lib/api/mekoApi";

// Not exported from mekoApi.ts; duplicated here rather than exporting it
// purely for this one file's sake.
const API_BASE = (import.meta.env.VITE_MEKO_API_BASE_URL || "/api/v1").replace(/\/$/, "");

/** The exact event shapes `agentcore_entrypoint.py`'s handler() yields, one
 *  SSE `data:` frame per event (see adlc-review-agent's own README for the
 *  canonical contract). `pr_title`/`pr_html_url` are only present on a
 *  fresh review's `meta` event, not a follow-up's. */
export type ReviewStreamEvent =
  | { type: "meta"; conversation_id: string; pr_title?: string; pr_html_url?: string | null }
  | { type: "text_delta"; text: string }
  | { type: "done"; review: string; slack_posted?: boolean; slack_error?: string | null };

/** A stream frame carrying an error instead of an event -- either a
 *  pre-stream InvocationError or the SDK's own mid-stream error frame.
 *  invokeDeployment throws on these rather than yielding them; not exported
 *  since callers only ever see the resulting InvokeDeploymentError message. */
type ReviewStreamErrorFrame = { error: string; error_type?: string; message?: string };

export class InvokeDeploymentError extends Error {}

/** `message` on the SDK's own mid-stream error frame is ALWAYS the same
 *  hardcoded string ("An error occurred during streaming", from
 *  bedrock_agentcore's runtime/app.py _sync_stream_with_error_handling) --
 *  the real detail is in `error` (str(exception)), with `error_type` (the
 *  exception's class name) as useful extra context. Prefer those; `message`
 *  is only ever a fallback for some other, differently-shaped error frame. */
function formatStreamError(event: ReviewStreamErrorFrame): string {
  const detail = event.error || event.message || "Unknown error";
  return event.error_type ? `${event.error_type}: ${detail}` : detail;
}

/** Streams a review (or follow-up) from a deployed agent. `body` is opaque
 *  to this function — it's the agent's own payload contract, forwarded
 *  through api_server verbatim; see `agentcore_entrypoint.py`'s two payload
 *  shapes (initial review vs. follow-up). */
export function invokeDeployment(
  deploymentId: string,
  body: Record<string, unknown>,
  signal?: AbortSignal,
): AsyncGenerator<ReviewStreamEvent> {
  return streamInvoke(`/deployments/${encodeURIComponent(deploymentId)}/invoke`, body, signal);
}

/** Same streaming contract as invokeDeployment, against agentSlug's shared
 *  deployment (one entry in the farm every signed-in user can use, via
 *  GET/POST /deployments/shared/:agent_slug*) instead of one they deployed
 *  themselves. `body` carries no `meko_pat` -- the shared instance
 *  authenticates to Meko with its own credential, not one forwarded from
 *  the browser per request. */
export function invokeSharedDeployment(
  agentSlug: string,
  body: Record<string, unknown>,
  signal?: AbortSignal,
): AsyncGenerator<ReviewStreamEvent> {
  return streamInvoke(`/deployments/shared/${encodeURIComponent(agentSlug)}/invoke`, body, signal);
}

async function* streamInvoke(
  path: string,
  body: Record<string, unknown>,
  signal?: AbortSignal,
): AsyncGenerator<ReviewStreamEvent> {
  const res = await fetch(`${API_BASE}${path}`, {
    method: "POST",
    credentials: "include",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify(body),
    signal,
  });

  if (!res.ok || !res.body) {
    if (res.status === 401) notifyAuthExpiredOnce();
    // api_server's own envelope error (auth/validation failures happen
    // before Invoke ever calls AWS) -- {"error": {"code", "message"}}.
    const text = await res.text();
    throw new InvokeDeploymentError(extractEnvelopeErrorMessage(text) ?? `Request failed (${res.status})`);
  }

  const reader = res.body.getReader();
  const decoder = new TextDecoder();
  let buffer = "";
  let sawAnyFrame = false;

  try {
    while (true) {
      const { done, value } = await reader.read();
      if (done) break;
      buffer += decoder.decode(value, { stream: true });
      const frames = buffer.split("\n\n");
      buffer = frames.pop() ?? ""; // last element may be a partial frame
      for (const frame of frames) {
        const dataLine = frame.split("\n").find((line) => line.startsWith("data:"));
        if (!dataLine) continue;
        sawAnyFrame = true;
        const event = JSON.parse(dataLine.slice(5).trim()) as ReviewStreamEvent | ReviewStreamErrorFrame;
        if ("error" in event) {
          throw new InvokeDeploymentError(formatStreamError(event));
        }
        yield event;
      }
    }

    // The stream can end right after the final frame's `data:` line with no
    // trailing blank line -- flush it instead of silently dropping it (this
    // is often the `done` frame carrying the full review text).
    const dataLine = buffer.split("\n").find((line) => line.startsWith("data:"));
    if (dataLine) {
      try {
        const event = JSON.parse(dataLine.slice(5).trim()) as ReviewStreamEvent | ReviewStreamErrorFrame;
        sawAnyFrame = true;
        buffer = "";
        if ("error" in event) {
          throw new InvokeDeploymentError(formatStreamError(event));
        }
        yield event;
      } catch (err) {
        if (err instanceof InvokeDeploymentError) throw err;
        // Genuinely truncated mid-JSON -- nothing safe to construct from it.
      }
    }
  } finally {
    // Also runs if the caller breaks/returns out of the `for await` early
    // (e.g. an aborted review) -- releases the reader lock instead of
    // leaving the response body's stream dangling.
    await reader.cancel().catch(() => {});
  }

  // A deployed image published before this feature shipped returns a single
  // non-streaming JSON blob (no `data:` framing at all) instead of erroring
  // -- whether it's error- or success-shaped, it means this image never
  // entered streaming mode, so surface that clearly instead of silently
  // yielding nothing (which left callers with a permanently-empty response).
  if (!sawAnyFrame && buffer.trim()) {
    let parsed: unknown;
    try {
      parsed = JSON.parse(buffer);
    } catch {
      return;
    }
    if (parsed && typeof parsed === "object" && "error" in parsed) {
      throw new InvokeDeploymentError(formatStreamError(parsed as ReviewStreamErrorFrame));
    }
    throw new InvokeDeploymentError(
      "This deployment's image doesn't support streaming reviews -- redeploy the agent to pick up the latest image.",
    );
  }
}

function extractEnvelopeErrorMessage(text: string): string | null {
  try {
    const parsed = JSON.parse(text) as { error?: { message?: string } | string };
    if (!parsed.error) return null;
    if (typeof parsed.error === "string") return parsed.error;
    return parsed.error.message ?? null;
  } catch {
    return null;
  }
}
