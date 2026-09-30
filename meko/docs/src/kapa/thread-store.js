const THREAD_KEY = 'meko-docs-ask-thread';

export function loadThread() {
  try {
    const raw = sessionStorage.getItem(THREAD_KEY);
    if (!raw) return null;
    const parsed = JSON.parse(raw);
    if (!parsed || !Array.isArray(parsed.turns)) return null;
    return { threadId: parsed.threadId || null, turns: parsed.turns };
  } catch {
    return null;
  }
}

export function saveThread(threadId, turns) {
  try {
    if (!turns.length && !threadId) {
      sessionStorage.removeItem(THREAD_KEY);
      return;
    }
    sessionStorage.setItem(THREAD_KEY, JSON.stringify({ threadId, turns: turns.map(slimTurn) }));
  } catch {
    // Storage can be blocked or full. The current page still keeps its thread.
  }
}

export function clearThread() {
  try {
    sessionStorage.removeItem(THREAD_KEY);
  } catch {
    // Storage can be blocked. The caller still clears the visible thread.
  }
}

export function threadIdFromFrame(frame) {
  if (!frame || typeof frame !== 'object') return null;
  const inner = frame.chunk || frame;
  const content = inner.content || {};
  return frame.thread_id || inner.thread_id || content.thread_id || null;
}

function slimTurn(turn) {
  const saved = { role: turn.role, text: turn.text || '' };
  if (turn.error) saved.error = turn.error;
  if (turn.interrupted) saved.interrupted = true;
  if (Array.isArray(turn.sources) && turn.sources.length) {
    saved.sources = turn.sources.slice(0, 5).map((source) => ({
      title: source.title || '',
      source_url: source.source_url || source.url || '',
    }));
  }
  return saved;
}
