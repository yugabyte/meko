// AgentK Local's service worker. It exists only so phones can show notifications: Android
// Chrome refuses `new Notification()` in a page and needs `registration.showNotification()`.
// It caches nothing and handles no fetches, so the app always loads from the server.

self.addEventListener('install', () => self.skipWaiting());
self.addEventListener('activate', (event) => event.waitUntil(self.clients.claim()));

// Tapping a notification opens its session (or, for a shared datapack, the New project dialog for
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
      const win = windows.find((w) => w.focused) || windows.find((w) => w.visibilityState === 'visible') || windows[0];
      if (win) {
        await win.focus();
        win.postMessage(message);
        return;
      }
      await self.clients.openWindow(url);
    })(),
  );
});
