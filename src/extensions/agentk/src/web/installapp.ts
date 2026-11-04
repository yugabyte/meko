// Offering to install AgentK as an app (its own window, with a dock or home screen icon): the
// banner in Banners.tsx and the command palette's entry. The manifest and the service worker
// already make it installable; this only catches the browser's offer and says when to show it.
import { useEffect, useState } from 'react';

/** Chrome's install offer, which TypeScript's DOM types leave out. */
interface InstallPromptEvent extends Event {
  prompt(): Promise<void>;
  readonly userChoice: Promise<{ outcome: 'accepted' | 'dismissed' }>;
}

/** Set once the offer is turned down in this browser ("Not now", or cancelling the browser's dialog). */
export const INSTALL_DISMISS_KEY = 'ak-dismissed-install';

/** What the banner shows: the browser's install offer, iOS's Add to Home Screen steps, or nothing. */
export type InstallOffer = 'prompt' | 'ios' | null;

export function installOffer(o: { installed: boolean; canPrompt: boolean; ios: boolean; dismissed: boolean }): InstallOffer {
  if (o.installed || o.dismissed) return null;
  if (o.canPrompt) return 'prompt';
  return o.ios ? 'ios' : null;
}

/** An iPhone or iPad, which installs only through Share › Add to Home Screen. An iPad says it's a Mac, but has a touch screen. */
export function isIos(platform: string, userAgent: string, maxTouchPoints: number): boolean {
  if (/iPhone|iPad|iPod/.test(platform) || /iPhone|iPad|iPod/.test(userAgent)) return true;
  return /Mac/.test(platform) && maxTouchPoints > 1;
}

let deferred: InstallPromptEvent | null = null;
let installed = false;
let prompting = false;
// Set by a dismissal in this page, so it holds here when storage can't keep it; another tab's change clears it.
let dismissedHere: boolean | null = null;
const listeners = new Set<() => void>();
const changed = () => listeners.forEach((f) => f());

/** Starts listening for the browser's offer. Called once from main.tsx, before the app renders: Chrome can make it first. */
export function watchInstall(): void {
  window.addEventListener('beforeinstallprompt', (e) => {
    // The banner offers it, so Android Chrome doesn't also show its own install bar.
    e.preventDefault();
    deferred = e as InstallPromptEvent;
    changed();
  });
  window.addEventListener('appinstalled', () => {
    deferred = null;
    installed = true;
    changed();
  });
}

function standalone(): boolean {
  return window.matchMedia?.('(display-mode: standalone)').matches || (navigator as { standalone?: boolean }).standalone === true;
}

function dismissed(): boolean {
  if (dismissedHere !== null) return dismissedHere;
  try {
    return localStorage.getItem(INSTALL_DISMISS_KEY) !== null;
  } catch {
    return false;
  }
}

function dismissInstall(): void {
  dismissedHere = true;
  try {
    localStorage.setItem(INSTALL_DISMISS_KEY, '1');
  } catch {
    /* storage unavailable: it holds for this page */
  }
  changed();
}

/** Whether the browser's install dialog is open (from a click just now), so that click doesn't also ask for notifications. */
export function installPrompting(): boolean {
  return prompting;
}

async function promptInstall(): Promise<void> {
  const e = deferred;
  if (!e) return;
  // An offer can be shown only once.
  deferred = null;
  prompting = true;
  changed();
  try {
    await e.prompt();
    const { outcome } = await e.userChoice;
    if (outcome === 'accepted') installed = true;
    else dismissInstall();
  } catch {
    // The browser refused to show it: the browser's own menu still installs.
  } finally {
    prompting = false;
    changed();
  }
}

/** The install offer for this page, kept current as the browser makes or withdraws it, and as another tab dismisses it. */
export function useInstall(): { offer: InstallOffer; canPrompt: boolean; install: () => Promise<void>; dismiss: () => void } {
  const [, setTick] = useState(0);
  useEffect(() => {
    const update = () => setTick((t) => t + 1);
    const onStorage = (e: StorageEvent) => {
      if (e.key !== INSTALL_DISMISS_KEY && e.key !== null) return;
      dismissedHere = null;
      update();
    };
    listeners.add(update);
    window.addEventListener('storage', onStorage);
    return () => {
      listeners.delete(update);
      window.removeEventListener('storage', onStorage);
    };
  }, []);
  const canPrompt = !!deferred && !installed;
  const offer = installOffer({
    installed: installed || standalone(),
    canPrompt,
    ios: isIos(navigator.platform, navigator.userAgent, navigator.maxTouchPoints),
    dismissed: dismissed(),
  });
  return { offer, canPrompt, install: promptInstall, dismiss: dismissInstall };
}
