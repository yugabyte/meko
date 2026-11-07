import { StrictMode } from 'react';
import { createRoot } from 'react-dom/client';
import '@xterm/xterm/css/xterm.css';
import './styles.css';
import { BASE, OLD_BASE } from './base';
import { App } from './components/App';
import { markOverlayScrollbars, trackAppHeight } from './hooks/useMedia';
import { watchInstall } from './installapp';
import { watchTermTheme } from './termtheme';

trackAppHeight();
markOverlayScrollbars();
watchInstall();
watchTermTheme();

// It shows the notifications (see public/sw.js); browsers allow it on HTTPS and localhost. Its
// scope is Kode's pages alone: Meko's own share the origin.
if ('serviceWorker' in navigator && window.isSecureContext) {
  navigator.serviceWorker.register(`${BASE}sw.js`, { scope: BASE }).catch(() => undefined);
  // Meko served Kode at /ext/agentk/ before: once it serves it elsewhere, the worker registered
  // there would show nothing.
  if (BASE !== OLD_BASE) {
    void navigator.serviceWorker.getRegistrations().then((regs) => {
      for (const r of regs) if (new URL(r.scope).pathname === OLD_BASE) void r.unregister();
    }).catch(() => undefined);
  }
}

createRoot(document.getElementById('root')!).render(
  <StrictMode>
    <App />
  </StrictMode>,
);
