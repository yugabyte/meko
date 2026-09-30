// Vanilla JS client for the Meko Kapa proxy. Sends a chat query and streams
// SSE frames back, emitting structured events to callbacks.
//
// The proxy at /api/kapa/chat returns `data: <json>\n\n` frames where each
// json is one chunk from Kapa (partial answer token, sources, thread_id,
// etc). We split on blank lines and dispatch.

const DEFAULT_ENDPOINT = '/api/kapa/chat';

/**
 * Start a streaming chat request.
 *
 * @param {object} params
 * @param {string} params.query
 * @param {string|null} [params.threadId]
 * @param {boolean} [params.deep]
 * @param {AbortSignal} [params.signal]
 * @param {string} [params.endpoint]
 * @param {(chunk: object) => void} [params.onChunk]
 * @param {(detail: {threadId: string|null}) => void} [params.onDone]
 * @param {(err: Error) => void} [params.onError]
 */
export async function streamChat({
  query,
  threadId = null,
  deep = false,
  signal,
  endpoint = DEFAULT_ENDPOINT,
  onChunk,
  onDone,
  onError,
}) {
  let response;
  try {
    response = await fetch(endpoint, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ query, thread_id: threadId, deep }),
      signal,
    });
  } catch (err) {
    if (err.name !== 'AbortError') onError && onError(err);
    return;
  }

  if (!response.ok) {
    const text = await response.text().catch(() => '');
    onError && onError(new Error(`Proxy returned ${response.status}: ${text.slice(0, 300)}`));
    return;
  }

  if (!response.body) {
    onError && onError(new Error('Proxy returned an empty body'));
    return;
  }

  const reader = response.body.getReader();
  const decoder = new TextDecoder();
  let buffer = '';
  let currentEvent = 'message';
  let latestThreadId = threadId;

  try {
    while (true) {
      const { value, done } = await reader.read();
      if (done) break;
      buffer += decoder.decode(value, { stream: true });

      // SSE frames are separated by a blank line (\n\n).
      let boundary;
      while ((boundary = buffer.indexOf('\n\n')) !== -1) {
        const rawFrame = buffer.slice(0, boundary);
        buffer = buffer.slice(boundary + 2);
        const frame = parseSseFrame(rawFrame);
        if (!frame) continue;
        if (frame.event) currentEvent = frame.event;
        const evt = frame.event || currentEvent || 'message';

        if (evt === 'error') {
          onError &&
            onError(new Error(frame.data && frame.data.error ? frame.data.error : 'Stream error'));
          continue;
        }

        if (frame.data && typeof frame.data === 'object') {
          // Kapa stream envelope: { chunk: { type, content, stream_end } }.
          // thread_id ships in chunk.type === 'identifiers' (content.thread_id),
          // occasionally at top-level on older responses.
          const inner = frame.data.chunk || frame.data;
          const innerContent = (inner && inner.content) || {};
          const tid = frame.data.thread_id || (inner && inner.thread_id) || innerContent.thread_id;
          if (tid) latestThreadId = tid;
          onChunk && onChunk(frame.data);
        }
      }
    }
    onDone && onDone({ threadId: latestThreadId });
  } catch (err) {
    if (err.name !== 'AbortError') onError && onError(err);
  } finally {
    await reader.cancel().catch(() => {});
  }
}

function parseSseFrame(raw) {
  const lines = raw.split(/\r?\n/);
  let event = null;
  let dataLines = [];
  for (const line of lines) {
    if (!line) continue;
    if (line.startsWith(':')) continue; // SSE comment
    if (line.startsWith('event:')) {
      event = line.slice(6).trim();
    } else if (line.startsWith('data:')) {
      dataLines.push(line.slice(5).trim());
    }
  }
  if (!dataLines.length) return event ? { event, data: null } : null;
  const joined = dataLines.join('\n');
  try {
    return { event, data: JSON.parse(joined) };
  } catch {
    return { event, data: joined };
  }
}
