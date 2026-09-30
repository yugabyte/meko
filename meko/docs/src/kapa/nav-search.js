// Navbar search button + floating overlay. On the home page we scroll to
// the hero input; on deep pages we open the overlay which hosts the same
// chat UI the hero uses. Shares streamChat + createChatUI so answers
// stream exactly like the landing experience.

import { streamChat } from './client.js';
import { createChatUI } from './chat-ui.js';
import { clearThread, loadThread, saveThread, threadIdFromFrame } from './thread-store.js';

export function initNavSearch() {
  const trigger = document.querySelector('[data-meko-nav-search]');
  const overlay = document.querySelector('[data-kapa-overlay]');
  if (!trigger || !overlay) return;

  const heroInput = document.querySelector('[data-kapa-hero-input]');
  const isHome = !!heroInput && !!document.querySelector('[data-kapa-hero]');

  const chatHost = overlay.querySelector('[data-kapa-overlay-chat]');
  const intro = overlay.querySelector('.kapa-overlay__intro');
  const promptBtns = overlay.querySelectorAll('[data-kapa-overlay-prompt]');
  const closeEls = overlay.querySelectorAll('[data-kapa-overlay-close]');
  const newChatBtn = overlay.querySelector('[data-kapa-overlay-new]');

  const endpointMeta = document.querySelector('meta[name="meko-kapa-chat-endpoint"]');
  const endpoint = (endpointMeta && endpointMeta.getAttribute('content')) || '/api/kapa/chat';

  let chat = null;
  let threadId = null;
  let abortCtl = null;
  let epoch = 0;

  function openOverlay() {
    overlay.hidden = false;
    document.body.classList.add('kapa-overlay-open');
    ensureChat();
    const composerInput = chatHost.querySelector('.kapa-chat__input');
    if (composerInput) composerInput.focus();
  }

  function closeOverlay() {
    overlay.hidden = true;
    document.body.classList.remove('kapa-overlay-open');
  }

  function ensureChat() {
    if (chat) return;
    chat = createChatUI({
      host: chatHost,
      onSubmit: ({ query, deep }) => sendOverlay(query, deep),
      onStop: () => stopGeneration(),
    });
    const saved = loadThread();
    if (saved && saved.turns.length) {
      threadId = saved.threadId || null;
      chat.restore(saved.turns);
      markIntroDone();
      syncNewChat();
    }
  }

  function sendOverlay(query, deep) {
    if (!query || !query.trim()) return;
    ensureChat();
    epoch += 1;
    const requestEpoch = epoch;
    if (abortCtl) {
      abortCtl.abort();
      chat.interruptAssistantTurn();
    }
    const requestCtl = new AbortController();
    abortCtl = requestCtl;

    markIntroDone();
    chat.appendUser(query);
    chat.startAssistantTurn();
    syncNewChat();
    persist();

    streamChat({
      query,
      threadId,
      deep,
      endpoint,
      signal: requestCtl.signal,
      onChunk: (frame) => {
        if (requestEpoch !== epoch) return;
        const tid = threadIdFromFrame(frame);
        if (tid) threadId = tid;
        chat.appendChunk(frame);
        persist();
      },
      onDone: ({ threadId: tid }) => {
        if (requestEpoch !== epoch) return;
        if (tid) threadId = tid;
        if (abortCtl === requestCtl) abortCtl = null;
        chat.finishAssistantTurn();
        persist();
        if (!overlay.hidden) chat.focusInput();
      },
      onError: (err) => {
        if (requestEpoch !== epoch) return;
        if (abortCtl === requestCtl) abortCtl = null;
        chat.showError(err.message || 'Something went wrong. Try again.');
        persist();
      },
    });
  }

  function stopGeneration() {
    if (!abortCtl) return;
    epoch += 1;
    abortCtl.abort();
    abortCtl = null;
    if (chat) chat.interruptAssistantTurn();
    persist();
  }

  function resetConversation() {
    epoch += 1;
    if (abortCtl) {
      abortCtl.abort();
      abortCtl = null;
    }
    threadId = null;
    if (chat) chat.reset();
    clearThread();
    if (intro) intro.classList.remove('kapa-overlay__intro--done');
    syncNewChat();
    if (chat && !overlay.hidden) chat.focusInput();
  }

  function persist() {
    if (!chat) return;
    saveThread(threadId, chat.transcript());
    syncNewChat();
  }

  function syncNewChat() {
    if (!newChatBtn || !chat) return;
    newChatBtn.hidden = !chat.transcript().some((turn) => turn.role === 'user');
  }

  function markIntroDone() {
    if (intro && !intro.classList.contains('kapa-overlay__intro--done')) {
      intro.classList.add('kapa-overlay__intro--done');
    }
  }

  trigger.addEventListener('click', () => {
    if (isHome && heroInput) {
      const hero = document.querySelector('[data-kapa-hero]');
      if (hero) hero.scrollIntoView({ behavior: 'smooth', block: 'center' });
      setTimeout(() => {
        const composerInput = hero && hero.querySelector('.kapa-chat__input');
        (composerInput || heroInput).focus();
      }, 300);
      return;
    }
    openOverlay();
  });

  closeEls.forEach((el) => el.addEventListener('click', closeOverlay));
  if (newChatBtn) newChatBtn.addEventListener('click', resetConversation);

  document.addEventListener('keydown', (e) => {
    // ESC closes overlay.
    if (e.key === 'Escape' && !overlay.hidden) {
      closeOverlay();
      return;
    }
    // "/" shortcut jumps to the search (when not typing into a field).
    if (e.key === '/' && !e.metaKey && !e.ctrlKey && !e.altKey && !isEditable(e.target)) {
      e.preventDefault();
      trigger.click();
    }
  });

  promptBtns.forEach((btn) => {
    btn.addEventListener('click', () => {
      sendOverlay(btn.textContent.trim(), false);
    });
  });
}

function isEditable(el) {
  if (!el) return false;
  const tag = el.tagName;
  if (tag === 'INPUT' || tag === 'TEXTAREA' || tag === 'SELECT') return true;
  return el.isContentEditable;
}
