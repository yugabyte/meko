import { StrictMode } from 'react';
import { createRoot } from 'react-dom/client';
import '@xterm/xterm/css/xterm.css';
import './styles.css';
import { BASE } from './base';
import { Root } from './components/Root';
import { trackAppHeight } from './hooks/useMedia';
import { watchInstall } from './installapp';
import { watchTermTheme } from './termtheme';

trackAppHeight();
watchInstall();
watchTermTheme();

// It shows the notifications (see public/sw.js); browsers allow it on HTTPS and localhost. Its
// scope is AgentK's pages alone: Meko's own share the origin.
if ('serviceWorker' in navigator && window.isSecureContext) {
  navigator.serviceWorker.register(`${BASE}sw.js`, { scope: BASE }).catch(() => undefined);
}

createRoot(document.getElementById('root')!).render(
  <StrictMode>
    <Root />
  </StrictMode>,
);
