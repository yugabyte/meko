// Shared Kapa proxy handler. Used by both the Netlify Function adapter
// (deployed to prod) and the local Node server adapter (Docker dev).
//
// Responsibility: accept a browser JSON POST { query, thread_id?, deep? },
// forward it to Kapa's streaming chat endpoint with the server-side API key,
// and pipe Kapa's U+241E-delimited response straight back to the browser as
// SSE events. The browser never sees the API key.

const KAPA_BASE = 'https://api.kapa.ai/query/v1';
const RECORD_SEPARATOR = '␞';

/**
 * Core handler. Input is a plain { query, thread_id?, deep? } payload + env.
 * Returns a ReadableStream of SSE-formatted bytes for the browser.
 *
 * @param {{ query: string, thread_id?: string, deep?: boolean }} payload
 * @param {{ KAPA_API_KEY: string, KAPA_PROJECT_ID: string, KAPA_INTEGRATION_ID?: string, KAPA_SOURCE_GROUP_IDS?: string }} env
 * @returns {Promise<{ status: number, body: ReadableStream|string, contentType: string }>}
 */
export async function handleKapaChat(payload, env) {
  const { query, thread_id, deep } = payload || {};

  if (!query || typeof query !== 'string' || !query.trim()) {
    return {
      status: 400,
      body: JSON.stringify({ error: 'query is required' }),
      contentType: 'application/json',
    };
  }

  if (!env.KAPA_API_KEY || !env.KAPA_PROJECT_ID) {
    return {
      status: 500,
      body: JSON.stringify({
        error: 'Server is not configured. Missing KAPA_API_KEY or KAPA_PROJECT_ID.',
      }),
      contentType: 'application/json',
    };
  }

  const url = thread_id
    ? `${KAPA_BASE}/threads/${encodeURIComponent(thread_id)}/chat/stream/`
    : `${KAPA_BASE}/projects/${encodeURIComponent(env.KAPA_PROJECT_ID)}/chat/stream/`;

  const body = {
    query: query.trim(),
  };
  if (env.KAPA_INTEGRATION_ID) {
    body.integration_id = env.KAPA_INTEGRATION_ID;
  }
  if (env.KAPA_SOURCE_GROUP_IDS) {
    // Kapa's chat/stream API names this field `source_group_ids_include`;
    // the seemingly-natural `source_group_ids` is silently ignored and
    // returns unfiltered results. Confirmed against the kapa-discord-bot
    // build — do not "simplify" this field name.
    const ids = env.KAPA_SOURCE_GROUP_IDS.split(',')
      .map((s) => s.trim())
      .filter(Boolean);
    if (ids.length) body.source_group_ids_include = ids;
  }
  if (deep) {
    // Kapa's chat API choices are "default" and "deep_thinking".
    // "ai-deep-research" is rejected with 400.
    body.mode = 'deep_thinking';
  }

  // Cancelled when the browser drops the response. Aborting the fetch is what
  // stops Kapa; closing only the SSE wrapper leaves the upstream request running.
  const abort = new AbortController();
  let upstream;
  try {
    upstream = await fetch(url, {
      method: 'POST',
      headers: {
        'X-API-KEY': env.KAPA_API_KEY,
        'Content-Type': 'application/json',
      },
      body: JSON.stringify(body),
      signal: abort.signal,
    });
  } catch (err) {
    return {
      status: 502,
      body: JSON.stringify({ error: `Upstream fetch failed: ${err.message}` }),
      contentType: 'application/json',
    };
  }

  if (!upstream.ok || !upstream.body) {
    const text = await upstream.text().catch(() => '');
    return {
      status: upstream.status || 502,
      body: JSON.stringify({
        error: `Kapa returned ${upstream.status}`,
        detail: text.slice(0, 500),
      }),
      contentType: 'application/json',
    };
  }

  // Translate Kapa's U+241E-delimited JSON stream into SSE events.
  // Each Kapa chunk becomes one `data: {...}\n\n` SSE frame.
  let reader;
  let stopped = false;
  const sseStream = new ReadableStream({
    async start(controller) {
      const encoder = new TextEncoder();
      const decoder = new TextDecoder();
      let buffer = '';

      const write = (obj) => {
        if (stopped) return;
        controller.enqueue(encoder.encode(`data: ${JSON.stringify(obj)}\n\n`));
      };

      try {
        reader = upstream.body.getReader();
        while (!stopped) {
          const { value, done } = await reader.read();
          if (done || stopped) break;
          buffer += decoder.decode(value, { stream: true });

          let idx;
          while ((idx = buffer.indexOf(RECORD_SEPARATOR)) !== -1) {
            const raw = buffer.slice(0, idx).trim();
            buffer = buffer.slice(idx + 1);
            if (!raw) continue;
            try {
              write(JSON.parse(raw));
            } catch {
              write({ type: 'raw', data: raw });
            }
          }
        }
        const tail = buffer.trim();
        if (!stopped && tail) {
          try {
            write(JSON.parse(tail));
          } catch {
            write({ type: 'raw', data: tail });
          }
        }
        if (!stopped) controller.enqueue(encoder.encode('event: done\ndata: {}\n\n'));
      } catch (err) {
        if (!stopped) {
          try {
            controller.enqueue(
              encoder.encode(`event: error\ndata: ${JSON.stringify({ error: err.message })}\n\n`),
            );
          } catch {
            // The consumer already cancelled this stream.
          }
        }
      } finally {
        if (reader) await reader.cancel().catch(() => {});
        if (!stopped) {
          try {
            controller.close();
          } catch {
            // The consumer already cancelled this stream.
          }
        }
      }
    },
    cancel(reason) {
      stopped = true;
      abort.abort(reason);
      if (reader) return reader.cancel(reason);
    },
  });

  return {
    status: 200,
    body: sseStream,
    contentType: 'text/event-stream',
  };
}
