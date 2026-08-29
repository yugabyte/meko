// Client-side Slack post for the review summary. WRITE-ONLY — never reads or
// searches Slack (see plan: "Slack posting is write-only").
//
// KNOWN RISK, not yet verified: Slack's Web API generally does not send
// permissive CORS headers for arbitrary-origin browser requests with a bot
// token, so this direct call may fail in practice with an opaque network
// error rather than a clean 4xx. If that's confirmed, this needs a small
// serverless relay (forward {channel, text} to Slack, never log/store the
// token) instead of calling api.slack.com directly from the browser. Verify
// against the actual token type before relying on this in production.

export class SlackApiError extends Error {}

export async function postToSlackChannel(
  token: string,
  channel: string,
  text: string,
): Promise<void> {
  const res = await fetch("https://slack.com/api/chat.postMessage", {
    method: "POST",
    headers: {
      Authorization: `Bearer ${token}`,
      "Content-Type": "application/json; charset=utf-8",
    },
    body: JSON.stringify({ channel, text }),
  });
  const data = (await res.json().catch(() => null)) as { ok?: boolean; error?: string } | null;
  if (!res.ok || !data?.ok) {
    throw new SlackApiError(data?.error || `Slack API request failed: ${res.status} ${res.statusText}`);
  }
}
