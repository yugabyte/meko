// Inline chat renderer. Owns the DOM inside a host element; renders user
// turns and a streaming assistant turn, collects source citations, and
// exposes methods for callers to update and focus the chat.

import { marked } from 'marked';
import DOMPurify from 'dompurify';

const INTERRUPTED_MESSAGE = 'Answer interrupted.';

marked.setOptions({
  gfm: true,
  breaks: true,
  headerIds: false,
  mangle: false,
});

// Force every rendered anchor to open in a new tab safely.
DOMPurify.addHook('afterSanitizeAttributes', (node) => {
  if (node.tagName === 'A') {
    node.setAttribute('target', '_blank');
    node.setAttribute('rel', 'noopener noreferrer');
  }
});

export function createChatUI({ host, onSubmit, onStop }) {
  const thread = document.createElement('div');
  thread.className = 'kapa-chat__thread';

  const composer = document.createElement('form');
  composer.className = 'kapa-chat__composer';
  composer.innerHTML = `
    <div class="kapa-chat__composer-row">
      <input
        type="text"
        class="kapa-chat__input"
        placeholder="Ask a question…"
        autocomplete="off"
      >
      <button type="submit" class="kapa-chat__send" aria-label="Send">
        <i class="fa fa-arrow-up" aria-hidden="true"></i>
      </button>
      <button type="button" class="kapa-chat__stop" aria-label="Stop" hidden>
        <i class="fa fa-stop" aria-hidden="true"></i>
      </button>
    </div>
    <label class="kapa-chat__deepthink">
      <input type="checkbox" class="kapa-chat__deepthink-input">
      <span class="kapa-chat__deepthink-pill"></span>
      <span>Deep thinking</span>
    </label>
  `;

  host.innerHTML = '';
  host.appendChild(thread);
  host.appendChild(composer);

  const input = composer.querySelector('.kapa-chat__input');
  const deepInput = composer.querySelector('.kapa-chat__deepthink-input');
  const sendBtn = composer.querySelector('.kapa-chat__send');
  const stopBtn = composer.querySelector('.kapa-chat__stop');

  composer.addEventListener('submit', (e) => {
    e.preventDefault();
    const q = input.value.trim();
    if (!q) return;
    input.value = '';
    onSubmit && onSubmit({ query: q, deep: deepInput.checked });
  });

  stopBtn.addEventListener('click', () => {
    onStop && onStop();
  });

  function setGenerating(generating) {
    sendBtn.hidden = generating;
    stopBtn.hidden = !generating;
  }

  let currentTurn = null;
  let turns = [];

  function syncPlaceholder() {
    const hasQuestion = turns.some((turn) => turn.role === 'user');
    input.placeholder = hasQuestion ? 'Ask a follow-up…' : 'Ask a question…';
  }

  function appendUser(text) {
    const row = document.createElement('div');
    row.className = 'kapa-chat__turn kapa-chat__turn--user';
    row.innerHTML = `<div class="kapa-chat__bubble">${escapeHtml(text)}</div>`;
    thread.appendChild(row);
    turns.push({ role: 'user', text });
    syncPlaceholder();
    scroll();
  }

  function startAssistantTurn() {
    interruptAssistantTurn();
    const row = document.createElement('div');
    row.className = 'kapa-chat__turn kapa-chat__turn--assistant';
    row.innerHTML = `
      <div class="kapa-chat__bubble">
        <div class="kapa-chat__answer"></div>
        <div class="kapa-chat__sources" hidden></div>
        <div class="kapa-chat__cursor" aria-hidden="true"></div>
      </div>
    `;
    thread.appendChild(row);
    currentTurn = {
      row,
      answerEl: row.querySelector('.kapa-chat__answer'),
      sourcesEl: row.querySelector('.kapa-chat__sources'),
      cursorEl: row.querySelector('.kapa-chat__cursor'),
      answerText: '',
      sources: [],
    };
    scroll();
    setGenerating(true);
    return currentTurn;
  }

  function appendChunk(frame) {
    if (!currentTurn) startAssistantTurn();
    // Kapa streams envelopes of shape { chunk: { type, content, stream_end } }.
    // The interesting types are:
    //   partial_answer  → content.text is a delta to append
    //   relevant_sources → content.relevant_sources is the full list
    //   identifiers     → content.thread_id is the conversation id
    // Also handle bare payloads (no outer `chunk`) defensively.
    const inner = (frame && frame.chunk) || frame || {};
    const type = inner.type;
    const content = inner.content || {};

    if (type === 'partial_answer' || !type) {
      const delta =
        (typeof content.text === 'string' && content.text) ||
        (typeof inner.answer === 'string' && inner.answer) ||
        (typeof inner.delta === 'string' && inner.delta) ||
        '';
      if (delta) {
        currentTurn.answerText += delta;
        renderAnswer(currentTurn);
        scroll();
      }
    }

    const sources =
      (Array.isArray(content.relevant_sources) && content.relevant_sources) ||
      (Array.isArray(inner.relevant_sources) && inner.relevant_sources) ||
      (Array.isArray(inner.sources) && inner.sources) ||
      null;
    if (sources && sources.length) {
      currentTurn.sources = sources;
      renderSources(currentTurn);
    }
  }

  function finishAssistantTurn() {
    if (!currentTurn) return;
    currentTurn.cursorEl.remove();
    turns.push({
      role: 'assistant',
      text: currentTurn.answerText,
      sources: currentTurn.sources.slice(),
    });
    currentTurn = null;
    setGenerating(false);
  }

  function interruptAssistantTurn() {
    if (!currentTurn) return;
    currentTurn.cursorEl.remove();
    const notice = document.createElement('div');
    notice.className = 'kapa-chat__error';
    notice.textContent = INTERRUPTED_MESSAGE;
    currentTurn.row.querySelector('.kapa-chat__bubble').appendChild(notice);
    turns.push({
      role: 'assistant',
      text: currentTurn.answerText,
      sources: currentTurn.sources.slice(),
      interrupted: true,
    });
    currentTurn = null;
    setGenerating(false);
    scroll();
  }

  function showError(message) {
    if (!currentTurn) startAssistantTurn();
    currentTurn.cursorEl.remove();
    const err = document.createElement('div');
    err.className = 'kapa-chat__error';
    err.textContent = message;
    currentTurn.row.querySelector('.kapa-chat__bubble').appendChild(err);
    turns.push({
      role: 'assistant',
      text: currentTurn.answerText,
      sources: currentTurn.sources.slice(),
      error: message,
    });
    currentTurn = null;
    setGenerating(false);
    scroll();
  }

  function transcript() {
    const saved = turns.map((turn) => ({ ...turn }));
    if (currentTurn) {
      saved.push({
        role: 'assistant',
        text: currentTurn.answerText,
        sources: currentTurn.sources.slice(),
        interrupted: true,
      });
    }
    return saved;
  }

  function reset() {
    turns = [];
    currentTurn = null;
    thread.innerHTML = '';
    syncPlaceholder();
    setGenerating(false);
  }

  function restore(saved) {
    reset();
    for (const turn of saved || []) {
      if (!turn) continue;
      const text = typeof turn.text === 'string' ? turn.text : '';
      if (turn.role === 'user') {
        appendUser(text);
        continue;
      }
      if (turn.role !== 'assistant') continue;
      startAssistantTurn();
      if (text) {
        currentTurn.answerText = text;
        renderAnswer(currentTurn);
      }
      if (Array.isArray(turn.sources) && turn.sources.length) {
        currentTurn.sources = turn.sources;
        renderSources(currentTurn);
      }
      if (turn.error) showError(String(turn.error));
      else if (turn.interrupted) interruptAssistantTurn();
      else finishAssistantTurn();
    }
  }

  function renderSources(turn) {
    if (!turn.sources.length) return;
    turn.sourcesEl.hidden = false;
    turn.sourcesEl.innerHTML =
      '<div class="kapa-chat__sources-title">Sources</div>' +
      '<ul>' +
      turn.sources
        .slice(0, 5)
        .map((s) => {
          const url = s.source_url || s.url || '#';
          const title = s.title || s.source_url || s.url || 'Source';
          return `<li><a href="${escapeAttr(url)}" target="_blank" rel="noopener noreferrer">${escapeHtml(title)}</a></li>`;
        })
        .join('') +
      '</ul>';
  }

  function scroll() {
    thread.scrollTop = thread.scrollHeight;
    requestAnimationFrame(() => {
      thread.scrollTop = thread.scrollHeight;
    });
  }

  function renderAnswer(turn) {
    turn.answerEl.innerHTML = renderMarkdownish(turn.answerText);
    turn.answerEl.querySelectorAll('img').forEach((image) => {
      if (!image.complete) image.addEventListener('load', scroll, { once: true });
    });
  }

  return {
    appendUser,
    startAssistantTurn,
    appendChunk,
    finishAssistantTurn,
    interruptAssistantTurn,
    showError,
    transcript,
    reset,
    restore,
    focusInput: () => input.focus(),
    setDeep: (v) => {
      deepInput.checked = !!v;
    },
  };
}

function escapeHtml(s) {
  return String(s).replace(
    /[&<>"']/g,
    (c) =>
      ({
        '&': '&amp;',
        '<': '&lt;',
        '>': '&gt;',
        '"': '&quot;',
        "'": '&#39;',
      })[c],
  );
}

function escapeAttr(s) {
  return escapeHtml(s);
}

// Kapa streams GitHub-Flavored Markdown. We parse with marked and sanitize
// with DOMPurify before injecting. DOMPurify keeps safe markup (headings,
// code blocks, lists, tables, links) and strips scripts, inline handlers,
// and anything else that could arrive in an untrusted stream.
function renderMarkdownish(text) {
  const html = marked.parse(text || '');
  return DOMPurify.sanitize(html, {
    ADD_ATTR: ['target', 'rel'],
  });
}
