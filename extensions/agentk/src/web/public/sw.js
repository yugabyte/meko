// AgentK's service worker. It shows the notifications (see showNotification in App.tsx):
// unlike a page's own `new Notification()`, which Android Chrome refuses, its notifications stay
// clickable after the page that showed them reloads (as open pages do after an update) or closes,
// and a click can switch to whichever browser tab already shows the session.
// It caches nothing and handles no fetches, so the app always loads from the server.

self.addEventListener('install', () => self.skipWaiting());
self.addEventListener('activate', (event) => event.waitUntil(self.clients.claim()));

/** The route parts of an AgentK page's URL: `/p/<project>[/s/<session>][/…]`. */
function pageOf(url) {
  let parts;
  try {
    parts = new URL(url).pathname.split('/').filter(Boolean).map(decodeURIComponent);
  } catch {
    return {};
  }
  if (parts[0] !== 'p' || !parts[1]) return {};
  // A design doc open on its own page stays as it is.
  const design = parts[2] === 's' && parts[4] === 'design';
  return { projectId: parts[1], sessionId: parts[2] === 's' ? parts[3] : undefined, design };
}

/**
 * The window a click opens the notification in: one already showing its session, else one on its
 * project, else the one used last. `windows` is ordered most recently focused first (matchAll's order).
 */
function pickWindow(windows, projectId, sessionId) {
  const pages = windows.map((w) => ({ w, page: pageOf(w.url) }));
  const app = pages.filter((x) => !x.page.design);
  const first = (list) => (list[0] ? list[0].w : undefined);
  if (projectId) {
    const onProject = app.filter((x) => x.page.projectId === projectId);
    const onSession = onProject.filter((x) => x.page.sessionId === sessionId);
    const found = first(onSession) || first(onProject);
    if (found) return found;
  }
  const byUse = app.length ? app.map((x) => x.w) : windows;
  return byUse.find((w) => w.focused) || byUse.find((w) => w.visibilityState === 'visible') || byUse[0];
}

// Tapping a notification opens its session (or, for a shared datapack, the Open project dialog for
// it): in an open AgentK window if there is one, else a new one (where the share's banner is waiting).
self.addEventListener('notificationclick', (event) => {
  event.notification.close();
  const { projectId, sessionId, datapackId } = event.notification.data || {};
  let url;
  let message;
  if (projectId && sessionId) {
    url = `/p/${encodeURIComponent(projectId)}/s/${encodeURIComponent(sessionId)}`;
    message = { type: 'ak-open', projectId, sessionId };
  } else if (datapackId) {
    url = '/';
    message = { type: 'ak-invite', datapackId };
  } else {
    return;
  }
  event.waitUntil(
    (async () => {
      const windows = await self.clients.matchAll({ type: 'window', includeUncontrolled: true });
      const win = pickWindow(windows, projectId, sessionId);
      if (win) {
        try {
          await win.focus();
        } catch {
          // Focus refused: the window still opens the session for when it's next looked at.
        }
        win.postMessage(message);
        return;
      }
      await self.clients.openWindow(url);
    })(),
  );
});
