import { StrictMode } from 'react';
import { createRoot } from 'react-dom/client';
import '@xterm/xterm/css/xterm.css';
import './styles.css';
import { Root } from './components/Root';
import { trackAppHeight } from './hooks/useMedia';
import { watchInstall } from './installapp';

trackAppHeight();
watchInstall();

// It shows the notifications (see public/sw.js); browsers allow it on HTTPS and localhost.
if ('serviceWorker' in navigator && window.isSecureContext) {
  navigator.serviceWorker.register('/sw.js').catch(() => undefined);
}

createRoot(document.getElementById('root')!).render(
  <StrictMode>
    <Root />
  </StrictMode>,
);
