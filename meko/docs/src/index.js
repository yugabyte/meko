import { createChatUI } from './kapa/chat-ui.js';
import { streamChat } from './kapa/client.js';
import { initNavSearch } from './kapa/nav-search.js';
import { clearThread, loadThread, saveThread, threadIdFromFrame } from './kapa/thread-store.js';

document.addEventListener('DOMContentLoaded', function () {
  // Wrap HTML table into div element.
  document.querySelectorAll('.td-content table').forEach((table) => {
    // skip if already wrapped
    if (table.parentElement.classList.contains('table-wrapper')) {
      return;
    }

    const wrapper = document.createElement('div');
    wrapper.className = 'table-wrapper';

    table.parentNode.insertBefore(wrapper, table);
    wrapper.appendChild(table);
  });
});

/**
 * Add Image Popup.
 */
(() => {
  const popup = document.createElement('div');
  popup.className = 'image-zoom';
  popup.innerHTML =
    '<i class="bg-drop"></i><div class="img-preview"><i class="close-btn"></i></div>';
  document.body.appendChild(popup);
  const preview = popup.querySelector('.img-preview');

  function closePopup() {
    document.body.classList.remove('image-zoom');
    popup.classList.remove('open');
    const existingImg = preview.querySelector('img');
    if (existingImg) {
      existingImg.remove();
    }
  }

  document.addEventListener('click', function (e) {
    const target = e.target.closest(
      '.td-content > img:not(.icon), .td-content p > img, .td-content table img',
    );
    if (target) {
      const imgSrc = target.getAttribute('src');
      const imgWidth = target.getAttribute('width');
      const imgHeight = target.getAttribute('height');
      const newImg = document.createElement('img');
      newImg.src = imgSrc;

      document.body.classList.add('image-zoom');
      popup.classList.add('open');
      if (imgWidth) {
        newImg.setAttribute('width', imgWidth);
      }
      if (imgHeight) {
        newImg.setAttribute('height', imgHeight);
      }
      preview.appendChild(newImg);
    }
  });

  document.addEventListener('click', function (e) {
    if (e.target.closest('.bg-drop') || e.target.closest('.close-btn')) {
      closePopup();
    }
  });

  document.onkeydown = function (event) {
    const keycode = event.keyCode ? event.keyCode : event.which;
    if (keycode === 27) {
      closePopup();
    }
  };
})();

/**
 * Meko AI hero on the landing page: first submission swaps the intro copy
 * for an inline chat panel, then streams Kapa answers through our server
 * proxy. Subsequent messages reuse the thread_id for conversation continuity.
 */
document.addEventListener('DOMContentLoaded', function () {
  initNavSearch();

  const hero = document.querySelector('[data-kapa-hero]');
  if (!hero) {
    return;
  }

  const endpointMeta = document.querySelector('meta[name="meko-kapa-chat-endpoint"]');
  const endpoint = (endpointMeta && endpointMeta.getAttribute('content')) || '/api/kapa/chat';

  const chatHost = hero.querySelector('[data-kapa-hero-chat]');
  const introForm = hero.querySelector('[data-kapa-hero-form]');
  const introInput = hero.querySelector('[data-kapa-hero-input]');
  const introDeep = hero.querySelector('[data-kapa-hero-deepthink]');
  const prompts = hero.querySelectorAll('[data-kapa-hero-prompt]');
  const newChatBtn = hero.querySelector('[data-kapa-hero-new]');

  let threadId = null;
  let chat = null;
  let abortCtl = null;
  let epoch = 0;

  function activateChat(initialDeep) {
    if (chat) return chat;
    hero.classList.add('kapa-hero--chatting');
    chat = createChatUI({
      host: chatHost,
      onSubmit: ({ query, deep }) => send(query, deep),
      onStop: () => stopGeneration(),
    });
    if (initialDeep) chat.setDeep(true);
    return chat;
  }

  function send(query, deep) {
    if (!query || !query.trim()) return;
    const ui = activateChat(deep);
    epoch += 1;
    const requestEpoch = epoch;
    if (abortCtl) {
      abortCtl.abort();
      ui.interruptAssistantTurn();
    }
    const requestCtl = new AbortController();
    abortCtl = requestCtl;

    ui.appendUser(query);
    ui.startAssistantTurn();
    persist();

    streamChat({
      query,
      threadId,
      deep,
      endpoint,
      signal: requestCtl.signal,
      onChunk: (chunk) => {
        if (requestEpoch !== epoch) return;
        const tid = threadIdFromFrame(chunk);
        if (tid) threadId = tid;
        ui.appendChunk(chunk);
        persist();
      },
      onDone: ({ threadId: tid }) => {
        if (requestEpoch !== epoch) return;
        if (tid) threadId = tid;
        if (abortCtl === requestCtl) abortCtl = null;
        ui.finishAssistantTurn();
        persist();
        ui.focusInput();
      },
      onError: (err) => {
        if (requestEpoch !== epoch) return;
        if (abortCtl === requestCtl) abortCtl = null;
        ui.showError(err.message || 'Something went wrong. Try again.');
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

  function persist() {
    if (!chat) return;
    saveThread(threadId, chat.transcript());
    syncNewChat();
  }

  function syncNewChat() {
    if (!newChatBtn || !chat) return;
    newChatBtn.hidden = !chat.transcript().some((turn) => turn.role === 'user');
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
    syncNewChat();
    if (chat) chat.focusInput();
  }

  const saved = loadThread();
  if (saved && saved.turns.length) {
    threadId = saved.threadId || null;
    activateChat(false).restore(saved.turns);
    syncNewChat();
  }

  if (newChatBtn) newChatBtn.addEventListener('click', resetConversation);

  if (introForm) {
    introForm.addEventListener('submit', function (e) {
      e.preventDefault();
      const q = introInput && introInput.value.trim();
      const deep = introDeep && introDeep.checked;
      send(q, deep);
    });
  }

  prompts.forEach((btn) => {
    btn.addEventListener('click', function () {
      const q = btn.textContent.trim();
      const deep = introDeep && introDeep.checked;
      if (introInput) introInput.value = q;
      send(q, deep);
    });
  });
});

/**
 * Copy the page's Markdown file into the clipboard.
 */
(() => {
  const button = document.querySelector('.markdown-actions button.copy-link');
  if (!button || !navigator.clipboard) {
    return;
  }

  const label = button.querySelector('span');
  const originalLabel = label ? label.textContent : '';
  let resetTimer = 0;
  let requestId = 0;

  button.addEventListener('click', () => {
    const url = button.getAttribute('data-copy-file');
    const id = ++requestId;
    clearTimeout(resetTimer);
    button.classList.add('is-copying');
    button.classList.remove('is-copied', 'is-failed');
    if (label) {
      label.textContent = 'Copying...';
    }

    const pending = fetch(url).then((response) => {
      if (!response.ok) {
        throw new Error('File not found');
      }
      return response.text();
    });

    // clipboard.write must run in the click so the user gesture is still
    // active. ClipboardItem accepts a promise and waits for the fetch.
    // writeText after await is rejected by Safari, and by Chrome when the
    // fetch is slow.
    let copy;
    try {
      if (typeof ClipboardItem !== 'undefined' && navigator.clipboard.write) {
        copy = navigator.clipboard.write([
          new ClipboardItem({
            'text/plain': pending.then((text) => new Blob([text], { type: 'text/plain' })),
          }),
        ]);
      } else {
        copy = pending.then((text) => navigator.clipboard.writeText(text));
      }
    } catch (error) {
      copy = Promise.reject(error);
    }

    copy
      .then(() => {
        if (id !== requestId) return;
        button.classList.add('is-copied');
        if (label) {
          label.textContent = 'Copied';
        }
      })
      .catch(() => {
        if (id !== requestId) return;
        button.classList.add('is-copied', 'is-failed');
        if (label) {
          label.textContent = 'Failed to copy';
        }
      })
      .finally(() => {
        if (id !== requestId) return;
        button.classList.remove('is-copying');
        resetTimer = setTimeout(() => {
          button.classList.remove('is-copied', 'is-failed');
          if (label) {
            label.textContent = originalLabel;
          }
        }, 2000);
      });
  });
})();
