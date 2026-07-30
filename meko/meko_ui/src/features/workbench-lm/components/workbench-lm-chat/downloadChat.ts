// Download the current WorkbenchLM chat as a .txt file. The header carries
// datapack / conversation / session metadata; the body alternates speakers
// separated by `---`. Markdown syntax in assistant messages is preserved
// verbatim — still readable as plain text and renders cleanly when opened
// in a markdown viewer.

import type { Message } from "./types";

type DownloadArgs = {
  messages: Message[];
  datapackName: string | null;
  sessionId: string | null;
  conversationId: string | null;
};

/**
 * Format the chat as a human-readable .txt blob.
 *
 * Layout:
 *
 *   Meko WorkbenchLM chat
 *   Datapack: <name>
 *   Conversation: <uuid>
 *   Session: <id>
 *   Exported: 2026-05-08 15:42 UTC
 *
 *   ---
 *
 *   You:
 *   <message>
 *
 *   ---
 *
 *   Assistant:
 *   <message>
 */
export function serializeChat({
  messages,
  datapackName,
  sessionId,
  conversationId,
}: DownloadArgs): string {
  // ISO minute precision in UTC, e.g. "2026-05-08 15:42 UTC".
  const now = new Date();
  const iso = now.toISOString().slice(0, 16).replace("T", " ") + " UTC";

  const headerLines = [
    "Meko WorkbenchLM chat",
    datapackName ? `Datapack: ${datapackName}` : null,
    conversationId ? `Conversation: ${conversationId}` : null,
    sessionId ? `Session: ${sessionId}` : null,
    `Exported: ${iso}`,
  ].filter((line): line is string => line !== null);

  const body = messages
    .map((m) => {
      const speaker = m.role === "user" ? "You" : "Assistant";
      return `${speaker}:\n${m.content}`;
    })
    .join("\n\n---\n\n");

  return `${headerLines.join("\n")}\n\n---\n\n${body}\n`;
}

/**
 * Trigger a browser download of the serialized chat.
 *
 * Uses a transient `<a download>` element + `URL.createObjectURL`. The
 * object URL is revoked right after the click to avoid leaking memory
 * for the duration of the page session.
 */
export function downloadChat(opts: DownloadArgs): void {
  const text = serializeChat(opts);
  const blob = new Blob([text], { type: "text/plain;charset=utf-8" });
  const url = URL.createObjectURL(blob);

  // Filename: meko-chat-<safe-datapack>-<YYYY-MM-DD-HHmm>.txt
  const stamp = new Date()
    .toISOString()
    .slice(0, 16)
    .replace("T", "-")
    .replace(":", "");
  const safeName = (opts.datapackName ?? "workbench-lm").replace(/[^a-zA-Z0-9_-]/g, "_");
  const filename = `meko-chat-${safeName}-${stamp}.txt`;

  const a = document.createElement("a");
  a.href = url;
  a.download = filename;
  document.body.appendChild(a);
  a.click();
  document.body.removeChild(a);
  URL.revokeObjectURL(url);
}
