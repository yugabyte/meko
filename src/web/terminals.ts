// One xterm.js Terminal (and its WebSocket) per session, cached across tab
// switches so that returning to a tab is instant and keeps its scrollback.

import { Terminal } from '@xterm/xterm';
import { FitAddon } from '@xterm/addon-fit';
import { WebLinksAddon } from '@xterm/addon-web-links';
import type { WebglAddon } from '@xterm/addon-webgl';
import { MAX_PASTE_BYTES, SCREEN_IDLE_MS, type ScreenName, type ScreenState, type TermClientMessage, type TermServerMessage } from '../shared/types';
import { api, checkLogin, errorMessage, wsUrl, WS_UNAUTHORIZED } from './api';
import { copyText, decodeOsc52, pastedImages, readClipboardImages } from './clipboard';
import { HeldImages, type HeldImage } from './heldImages';
import { MOBILE_QUERY, TOUCH_QUERY, matches } from './hooks/useMedia';
import {
  clampFont, ctrlSequence, decayVelocity, flingVelocity, isTerminalReport, keySequence, pastePath, pasteText, touchRow, wheelReport, wheelSteps,
  FLING_WINDOW_MS,
  type BarKey, type TouchSample,
} from './keys';

const IS_MAC = /Mac|iPhone|iPad/.test(navigator.platform);

export type TermConn = 'connecting' | 'open' | 'closed' | 'ended';
/** Which screen has the session: this one, this one once the other is idle, or another one. */
export type TermScreen = 'here' | 'waiting' | 'elsewhere';

/** Random hex. Not crypto.randomUUID: a plain-http network bind isn't a secure context, which it needs. */
function randomId(): string {
  const b = crypto.getRandomValues(new Uint8Array(12));
  return Array.from(b, (x) => x.toString(16).padStart(2, '0')).join('');
}

/** This page load's id: the server knows it as one screen, a reconnect included. */
const PAGE_ID = randomId();
const BROWSER_KEY = 'ak-browser';
/** This browser's id, so another tab in it is named as one. Empty when storage is blocked. */
const BROWSER_ID = (() => {
  try {
    let id = localStorage.getItem(BROWSER_KEY);
    if (!id) {
      id = randomId();
      localStorage.setItem(BROWSER_KEY, id);
    }
    return id;
  } catch {
    return '';
  }
})();

/** Another screen, in words ("another tab in this browser", "another screen (Chrome on macOS)"). */
export function screenLabel(other: ScreenName | null): string {
  if (!other) return 'another screen';
  return other.sameBrowser ? 'another tab in this browser' : `another screen (${other.name})`;
}
const ELSEWHERE = 'This session is open on another screen. Use Move to this screen to type here.';

const THEME = {
  background: '#101412',
  foreground: '#D5DDD8',
  cursor: '#D5DDD8',
  cursorAccent: '#101412',
  selectionBackground: '#2E3B55',
  black: '#1A1F1C',
  red: '#F07366',
  green: '#6FD19F',
  yellow: '#F2B35B',
  blue: '#8FB0FF',
  magenta: '#C9A2FF',
  cyan: '#6CCFD6',
  white: '#D5DDD8',
  brightBlack: '#7C8A83',
  brightRed: '#FF8F84',
  brightGreen: '#8BE3B5',
  brightYellow: '#FFC97A',
  brightBlue: '#AFC6FF',
  brightMagenta: '#DCC0FF',
  brightCyan: '#8FE3E8',
  brightWhite: '#F2F5F3',
};

const FONT = '"JetBrains Mono", ui-monospace, "SF Mono", Menlo, Consolas, monospace';

const FONT_KEY = 'ak-term-font';
/** Phones get a smaller font so Claude Code has about 48 columns instead of 38. */
const defaultFont = () => (matches(MOBILE_QUERY) ? 11 : 13);

function storedFont(): number | null {
  try {
    const v = Number(localStorage.getItem(FONT_KEY));
    return v ? clampFont(v) : null;
  } catch {
    return null;
  }
}

/**
 * Finger travel, in rows, per mouse-wheel step. When Claude Code runs full screen it takes the
 * mouse, gets the steps and decides how far each one scrolls; otherwise the terminal scrolls itself.
 */
const SWIPE_ROWS = 2.5;
/** A swipe let go faster than this (px/ms) coasts; momentum stops below FLING_STOP. */
const FLING_MIN = 0.3;
const FLING_STOP = 0.05;
/** Pause between a compose paste and the Enter that submits it, as the server does for prompts. */
const SUBMIT_DELAY = 300;
/** Pause between the paths of several pasted images, so Claude Code takes each paste on its own. */
const IMAGE_PASTE_GAP = 200;
const NOT_CONNECTED = 'Not connected to the session. Try again once it reconnects.';
const sleep = (ms: number) => new Promise((r) => setTimeout(r, ms));
/**
 * A connection that stays up this long resets the reconnect backoff. An open alone proves nothing:
 * the server upgrades first, then may turn the socket away (a lost login, a failed attach).
 */
const STABLE_MS = 5000;
/** On phones, a terminal taken off screen lets go of its session after this long. */
const DETACH_RELEASE_MS = 5000;
/**
 * How long a screen showing a session waits, after the screen that had it closed, before it asks for
 * it: a page that only lost its connection reconnects within this (backoff 0.5 s, then 1 s) and keeps it.
 */
const RECLAIM_MS = 3000;

/** Why the pty behind a terminal exited, in words (the UI adds the dots while it reconnects). */
const EXIT_REASONS: Record<string, string> = {
  detached: 'The terminal was detached. Reconnecting',
};

class SessionTerminal {
  readonly term: Terminal;
  readonly el: HTMLDivElement;
  private fit = new FitAddon();
  private ws: WebSocket | null = null;
  private attempt = 0;
  private timer: ReturnType<typeof setTimeout> | null = null;
  /** When the current socket opened (ms), or 0. */
  private openedAt = 0;
  /** Whether a connection was ever started; the first attach starts it, at the terminal's size. */
  private started = false;
  private releaseTimer: ReturnType<typeof setTimeout> | null = null;
  private disposed = false;
  private resizeObs: ResizeObserver | null = null;
  /** The size this terminal last asked for (what fits its box). */
  private lastSize = { cols: 0, rows: 0 };
  /**
   * This terminal sets the session's size (it was opened or used last). Otherwise it shows the
   * session at the size another viewer set, until it's used.
   */
  private active = true;
  private refitFrame = 0;
  /** Disconnected on purpose while the page is hidden; reconnects when it's shown again. */
  private paused = false;
  private touch: { x: number; y: number; travel: number } | null = null;
  /** In select mode, the buffer line the finger went down on. */
  private selectAnchor: number | null = null;
  /** Where the last swipe was, so momentum reports its wheel steps at the same cell. */
  private point = { x: 0, y: 0 };
  private samples: TouchSample[] = [];
  /** Wheel steps waiting for the next frame (positive scrolls up, to older lines). */
  private pendingSteps = 0;
  private flushFrame = 0;
  private flingFrame = 0;
  /** GPU renderer for the terminal on screen (touch screens only); null uses the DOM renderer. */
  private webgl: WebglAddon | null = null;
  private webglFailed = false;
  private webglLoading = false;
  /** On touch screens, whether focusing the terminal opens the on-screen keyboard. */
  typing = false;
  /** Sticky Ctrl from the key bar: the next character typed is sent with Ctrl. */
  ctrl = false;
  /** Select mode from the key bar (touch screens): a drag selects whole lines and copies them. */
  selecting = false;
  conn: TermConn = 'connecting';
  /** Which screen has the session; the server says, and a server that doesn't leaves it here. */
  screen: TermScreen = 'here';
  /** The screen that has the session, while this one doesn't (null: it closed). */
  other: ScreenName | null = null;
  /** While waiting: when the session moves here (ms), unless the other screen is used first. */
  moveAt = 0;
  /** This screen had the session and lost it: it doesn't ask again on a reconnect, only when shown again. */
  private lost = false;
  /** What the server last said (kept across reconnects; `screen` starts at 'here' on each connect). */
  private told: TermScreen | null = null;
  /** The screen that last asked while this one had the session, as told (for the notice). */
  private asker = '';
  message: string | null = null;
  /** `message` says what is being retried ("Reconnecting"): the UI shows it with moving dots. */
  retrying = false;
  /** The server said why it can't attach (in `message`): no reconnecting until reconnect(). */
  failed = false;
  /** A short-lived note after copying ("Copied", or why it failed), or while sending images. */
  notice: string | null = null;
  /** `notice` is something still going on: the UI shows it with moving dots. */
  noticeBusy = false;
  private noticeTimer: ReturnType<typeof setTimeout> | null = null;
  private listeners = new Set<() => void>();
  /**
   * Images for Claude, held until the next Enter or Send (flush). Claude Code can't see this browser's
   * clipboard, so each image uploads to the host as it's added, and sending pastes its saved path,
   * which Claude Code reads as the image.
   */
  readonly held = new HeldImages<Blob>({
    upload: async (image) => (await api.pasteImage(this.sessionId, image)).path,
    thumb: (image) => URL.createObjectURL(image),
    revoke: (url) => URL.revokeObjectURL(url),
    onChange: () => this.listeners.forEach((l) => l()),
    onError: (e) => this.showNotice(`Couldn’t add the image: ${errorMessage(e)}`, 6000),
  });
  /** Input typed while held images are being sent (non-null only then); it follows them. */
  private queued: string[] | null = null;
  /** While a send waits on uploads: calls it off (Esc or Ctrl+C). */
  private cancelWait: (() => void) | null = null;

  constructor(readonly sessionId: string) {
    this.el = document.createElement('div');
    this.el.className = 'xterm-host';
    this.term = new Terminal({
      fontFamily: FONT,
      fontSize: storedFont() ?? defaultFont(),
      lineHeight: 1.15,
      cursorBlink: true,
      allowProposedApi: true,
      scrollback: 10000,
      theme: THEME,
      macOptionIsMeta: true,
      // While Claude has the mouse, a plain drag goes to Claude; Shift-drag, or Option-drag on a
      // Mac, selects in the browser instead.
      macOptionClickForcesSelection: true,
    });
    this.term.loadAddon(this.fit);
    this.term.loadAddon(
      new WebLinksAddon((_e, uri) => {
        window.open(uri, '_blank', 'noopener');
      }),
    );
    this.term.open(this.el);
    this.setTyping(false);
    // Capture, so a swipe is ours before xterm sees it.
    // Not passive: in select mode a touch is prevented, so no tap reaches Claude as a mouse click.
    this.el.addEventListener('touchstart', this.touchStart, { capture: true, passive: false });
    this.el.addEventListener('touchmove', this.touchMove, { capture: true, passive: false });
    this.el.addEventListener('touchend', this.touchEnd, { capture: true, passive: false });
    this.el.addEventListener('touchcancel', this.touchCancel, { capture: true, passive: true });
    // Capture, so an image paste or drop is ours before xterm takes the paste as text.
    this.el.addEventListener('paste', this.onPaste, { capture: true });
    this.el.addEventListener('dragover', this.onDragOver);
    this.el.addEventListener('drop', this.onDrop);
    // xterm measures its cells when it opens. If the web font arrives later (a cold cache), the
    // cells were measured with a fallback font and the rows no longer fit: re-measure and refit.
    document.fonts?.load(`13px ${FONT}`).then(
      () => {
        if (this.disposed || !document.fonts.check(`13px ${FONT}`)) return;
        this.term.options.fontFamily = `${FONT}, monospace`;
        this.term.options.fontFamily = FONT;
        this.refit();
      },
      () => undefined,
    );
    // Focusing a terminal makes it the one whose size the session takes.
    this.term.textarea?.addEventListener('focus', () => this.claimActive());
    this.term.onData((data) => {
      // The session host answers the program's queries itself; this terminal's answers would repeat them.
      if (isTerminalReport(data)) return;
      // In typing mode the phone keyboard types here; a sticky Ctrl applies to its next character.
      if (this.ctrl && this.typeCtrl(data)) return;
      this.input(data);
    });
    this.term.onBinary((data) => this.send({ type: 'input', data }));
    // A program copying text sends it as OSC 52; put it on the browser clipboard.
    this.term.parser.registerOscHandler(52, (data) => {
      const text = decodeOsc52(data);
      if (text) void this.copy(text);
      return true;
    });
    // Let the app's own shortcuts through; everything else goes to the terminal.
    this.term.attachCustomKeyEventHandler((e) => {
      if (e.type !== 'keydown') return true;
      const mod = IS_MAC ? e.metaKey : e.ctrlKey;
      // With text selected, ⌘C (Ctrl+C or Ctrl+Shift+C elsewhere) copies it instead of interrupting Claude.
      if (mod && (e.key === 'c' || e.key === 'C') && this.term.hasSelection()) {
        e.preventDefault();
        void this.copy(this.term.getSelection());
        if (!IS_MAC) this.term.clearSelection();
        return false;
      }
      // Ctrl+V pastes from this browser's clipboard, images included. Claude Code's own Ctrl+V reads the
      // clipboard of the host it runs on, which is this browser's only when both run on one computer;
      // the page can't tell (a tunnel to a remote host is localhost too), so this serves both cases.
      if (e.ctrlKey && !e.shiftKey && !e.altKey && !e.metaKey && (e.key === 'v' || e.key === 'V')) {
        // Elsewhere Ctrl+V is the browser's paste: let it fire the paste event (onPaste).
        if (!IS_MAC) return false;
        // On a Mac ⌘V pastes and Ctrl+V doesn't, so read the clipboard for an image.
        e.preventDefault();
        void this.ctrlV();
        return false;
      }
      if (mod && (e.key === 'k' || e.key === 'K')) return false;
      if (mod && /^[1-9]$/.test(e.key)) return false;
      if (e.ctrlKey && e.key === 'Tab') return false;
      return true;
    });
  }

  subscribe(fn: () => void): () => void {
    this.listeners.add(fn);
    return () => this.listeners.delete(fn);
  }

  private async copy(text: string, copied = 'Copied') {
    const ok = await copyText(text);
    const how = matches(TOUCH_QUERY) ? '' : ` Select with ${IS_MAC ? 'Option' : 'Shift'}-drag and press ${IS_MAC ? '⌘C' : 'Ctrl+C'}.`;
    this.showNotice(ok ? copied : `Couldn’t copy: the browser blocked the clipboard.${how}`, ok ? 1500 : 6000);
  }

  /** Shows a short note over the terminal for `ms`; `busy` for something still going on. */
  showNotice(text: string, ms: number, busy = false) {
    this.notice = text;
    this.noticeBusy = busy;
    this.listeners.forEach((l) => l());
    if (this.noticeTimer) clearTimeout(this.noticeTimer);
    this.noticeTimer = setTimeout(() => {
      this.notice = null;
      this.noticeBusy = false;
      this.listeners.forEach((l) => l());
    }, ms);
  }

  private hideNotice() {
    if (this.noticeTimer) clearTimeout(this.noticeTimer);
    this.noticeTimer = null;
    this.notice = null;
    this.noticeBusy = false;
    this.listeners.forEach((l) => l());
  }

  /** Holds images for Claude until the next Enter or Send; each starts uploading now. */
  holdImages(images: Blob[]) {
    if (!images.length) return;
    if (images.some((i) => i.size > MAX_PASTE_BYTES)) {
      const what = images.length === 1 ? 'the image' : 'the images';
      this.showNotice(`Couldn’t add ${what}: images over ${MAX_PASTE_BYTES / 1024 / 1024} MB can’t be pasted.`, 6000);
      return;
    }
    this.held.add(images);
  }

  /** Held images are being sent: Send waits, and typed keys are queued. */
  get sending(): boolean {
    return this.queued !== null;
  }

  /**
   * Typed input. An Enter with images held sends them first; while they're sent, input waits its
   * turn. Returns false when the input couldn't be sent or queued (not connected).
   */
  private input(data: string): boolean {
    if (this.queued) {
      // Esc or Ctrl+C while a send waits on an upload calls the send off, then goes through.
      if (data === '\x1b' || data === '\x03') this.cancelWait?.();
      this.queued.push(data);
      return true;
    }
    if (data === '\r' && this.held.size) {
      void this.flush('', NOT_CONNECTED);
      return true;
    }
    return this.send({ type: 'input', data });
  }

  /**
   * Sends `text` (the compose box's), then the held images' paths, then Enter, letting go of each
   * image once its path is out. Resolves whether anything reached Claude. False means nothing did (not
   * connected, an upload failed, or Esc or Ctrl+C called it off while it waited on an upload): the
   * images stay held. True can still mean the connection dropped partway: what went out isn't sent
   * again, and the notice says to check Claude's prompt.
   */
  private async flush(text: string, offline: string): Promise<boolean> {
    if (!this.canType) {
      this.showNotice(this.conn === 'open' && this.screen !== 'here' ? ELSEWHERE : offline, 4000);
      return false;
    }
    this.queued = [];
    this.listeners.forEach((l) => l());
    let sent = false;
    const cutOff = () => {
      this.showNotice(sent ? 'The connection dropped while sending. Part of it is in Claude’s prompt: check it once the terminal reconnects.' : offline, 6000);
      return sent;
    };
    try {
      let images: HeldImage[] | null;
      if (this.held.uploading) {
        const waiting = `Sending once ${this.held.size === 1 ? 'the image uploads' : 'the images upload'}. Esc cancels.`;
        // Until the uploads finish: an upload can stall, and nothing bounds it.
        this.showNotice(waiting, 3_600_000);
        const cancelled = new Promise<null>((r) => (this.cancelWait = () => r(null)));
        images = await Promise.race([this.held.ready(), cancelled]);
        this.cancelWait = null;
        // A failed upload's notice replaced this one: leave that showing.
        if (this.notice === waiting) this.hideNotice();
      } else images = await this.held.ready();
      if (!images) return false;
      const bracketed = this.term.modes.bracketedPasteMode;
      const pastes: { data: string; image?: HeldImage }[] = [
        ...(text ? [{ data: pasteText(text, bracketed) }] : []),
        ...images.map((image) => ({ data: pastePath(image.path!, bracketed), image })),
      ];
      for (const [i, p] of pastes.entries()) {
        // One paste at a time, so Claude Code makes each image path an [Image #N].
        if (i) await sleep(IMAGE_PASTE_GAP);
        if (!this.send({ type: 'input', data: p.data })) return cutOff();
        sent = true;
        if (p.image) this.held.release([p.image]);
      }
      if (pastes.length) await sleep(SUBMIT_DELAY);
      if (!this.send({ type: 'input', data: '\r' })) return cutOff();
      return true;
    } finally {
      this.cancelWait = null;
      const queued = this.queued ?? [];
      this.queued = null;
      this.listeners.forEach((l) => l());
      // An Enter in here may start another send, which queues what's left behind it.
      let dropped = false;
      for (const data of queued) if (!this.input(data)) dropped = true;
      // Say so, unless a notice already says why (the connection dropped).
      if (dropped && !this.notice) this.showNotice(this.cantType(), 4000);
    }
  }

  private onPaste = (e: ClipboardEvent) => {
    const images = pastedImages(e.clipboardData);
    if (!images.length) return; // text: xterm pastes it
    e.preventDefault();
    e.stopPropagation();
    this.holdImages(images);
  };

  /**
   * Ctrl+V on a Mac: an image on the clipboard is held for Claude; otherwise Claude gets Ctrl+V, as before (on
   * the same computer, Claude Code then reads the clipboard itself). Safari asks before each read.
   */
  private async ctrlV() {
    const images = await readClipboardImages();
    if (images?.length) {
      this.holdImages(images);
      return;
    }
    this.send({ type: 'input', data: '\x16' });
    if (images === null) this.showNotice('To paste an image, press ⌘V.', 4000);
  }

  private onDragOver = (e: DragEvent) => {
    if (!e.dataTransfer?.types.includes('Files')) return;
    e.preventDefault();
    e.dataTransfer.dropEffect = 'copy';
  };

  /** Images dropped on the terminal are held for Claude; the browser would otherwise open the file. */
  private onDrop = (e: DragEvent) => {
    if (!e.dataTransfer?.types.includes('Files')) return;
    e.preventDefault();
    const images = pastedImages(e.dataTransfer);
    if (images.length) this.holdImages(images);
    else this.showNotice('Only images can be dropped here: PNG, JPEG, GIF or WebP.', 4000);
  };

  private setConn(c: TermConn, message: string | null = null, retrying = false) {
    this.conn = c;
    this.message = message;
    this.retrying = retrying;
    this.listeners.forEach((l) => l());
  }

  /** Returns false (and drops the message) when not connected. */
  private send(msg: TermClientMessage): boolean {
    if (this.ws?.readyState !== WebSocket.OPEN) return false;
    // Only the screen that has the session types into it.
    if (msg.type === 'input' && this.screen !== 'here') return false;
    this.ws.send(JSON.stringify(msg));
    // Input makes this the active terminal (the host does the same on its side): take its size back.
    if (msg.type === 'input' && !this.active) {
      this.active = true;
      this.refit();
    }
    return true;
  }

  /** This terminal was focused: on this screen the session takes its size; elsewhere it asks for the session. */
  private claimActive() {
    if (this.screen !== 'here') {
      this.show();
      return;
    }
    if (this.active || !this.send({ type: 'active' })) return;
    this.active = true;
    this.refit();
  }

  /** On screen: in the page, and the page isn't hidden. */
  private showing(): boolean {
    return this.el.isConnected && !document.hidden;
  }

  /**
   * This screen shows the session: the server hands it here, now or once the other screen is idle.
   * A socket still connecting asks as it opens.
   */
  show() {
    this.lost = false;
    this.send({ type: 'show' });
  }

  /** This screen stopped showing the session: a screen that asks for it gets it at once. */
  hide() {
    this.send({ type: 'hide' });
  }

  /** Move to this screen: takes the session from the screen that has it. */
  moveHere() {
    this.send({ type: 'move' });
  }

  /** Whether what's typed reaches Claude: connected, and this screen has the session. */
  get canType(): boolean {
    return this.conn === 'open' && this.ws?.readyState === WebSocket.OPEN && this.screen === 'here';
  }

  /** Why typing can't reach Claude now; `kept`: the compose box keeps the message. */
  cantType(kept = false): string {
    if (this.conn === 'open' && this.screen !== 'here') return ELSEWHERE;
    return kept ? 'Not connected to the session. Your message is kept; send it again once it reconnects.' : NOT_CONNECTED;
  }

  private setScreen(s: ScreenState) {
    const was = this.screen;
    if (this.told === 'here' && s.state === 'elsewhere') this.lost = true;
    this.told = s.state;
    this.screen = s.state;
    this.other = s.state === 'here' ? null : s.other;
    this.moveAt = s.state === 'waiting' ? Date.now() + s.moveInMs : 0;
    if (s.state === 'here') {
      this.lost = false;
      // Another screen asked for it: say so once, with the rule that moves it there.
      const asker = s.waiting ? JSON.stringify(s.waiting) : '';
      if (s.waiting && asker !== this.asker)
        this.showNotice(
          `This session is open on ${screenLabel(s.waiting)} too. It moves there once this screen goes ${SCREEN_IDLE_MS / 1000} s unused or stops showing it.`,
          6000,
        );
      this.asker = asker;
      if (was !== 'here') {
        // The host sends a fresh screen; the session takes this terminal's size again.
        this.active = true;
        this.lastSize = { cols: 0, rows: 0 };
        this.refit();
      }
    } else {
      this.asker = '';
      // The screen that had it closed: a screen showing the session takes it, unless that page reconnects first.
      if (s.state === 'elsewhere' && !s.other)
        setTimeout(() => {
          if (!this.disposed && this.screen === 'elsewhere' && !this.other && this.showing()) this.show();
        }, RECLAIM_MS);
    }
    this.listeners.forEach((l) => l());
  }

  /** What fits this terminal's box, or null when it isn't laid out. */
  private wantedSize(): { cols: number; rows: number } | null {
    if (!this.el.isConnected || this.el.clientWidth === 0) return null;
    try {
      const d = this.fit.proposeDimensions();
      return d && Number.isFinite(d.cols) && Number.isFinite(d.rows) ? { cols: d.cols, rows: d.rows } : null;
    } catch {
      return null;
    }
  }

  private connect() {
    if (this.disposed) return;
    this.started = true;
    this.setConn('connecting');
    // A terminal that connects is the newest viewer: the session takes its size.
    this.active = true;
    // Fit first when on screen, so the session starts at this terminal's size.
    if (this.el.isConnected && this.el.clientWidth) {
      try {
        this.fit.fit();
      } catch {
        /* not laid out yet: the size is reported on open */
      }
    }
    const { cols, rows } = this.term;
    // On screen, it asks for the session as it connects, unless this screen lost it to another one.
    const claimed = this.showing() && !this.lost;
    const claim = claimed ? '&claim=1' : '';
    const ws = new WebSocket(
      wsUrl(`/ws/term/${encodeURIComponent(this.sessionId)}?cols=${cols}&rows=${rows}&screen=${PAGE_ID}&browser=${BROWSER_ID}${claim}`),
    );
    // Until the server says otherwise (a server from before screens never does).
    this.screen = 'here';
    this.other = null;
    ws.binaryType = 'arraybuffer';
    this.ws = ws;
    ws.onopen = () => {
      this.openedAt = Date.now();
      this.setConn('open');
      this.lastSize = { cols: 0, rows: 0 };
      this.refit();
      // Off screen refit does nothing: report the size anyway, so the session doesn't keep the start size.
      if (!this.lastSize.cols) {
        this.lastSize = { cols: this.term.cols, rows: this.term.rows };
        this.send({ type: 'resize', ...this.lastSize });
      }
      // Shown while it was connecting (a show() then had no socket to go out on): ask now.
      if (!claimed && this.showing() && !this.lost) this.show();
    };
    ws.onmessage = (ev) => {
      if (this.ws !== ws) return;
      if (typeof ev.data === 'string') {
        let msg: TermServerMessage;
        try {
          msg = JSON.parse(ev.data);
        } catch {
          return;
        }
        if (msg.type === 'size') {
          // The session's size: this terminal's own while it's active, else another viewer's.
          this.active = msg.active;
          if (msg.active) this.refit();
          else if (this.term.cols !== msg.cols || this.term.rows !== msg.rows) this.term.resize(msg.cols, msg.rows);
        } else if (msg.type === 'screen') this.setScreen(msg);
        else if (msg.type === 'exit') this.setConn('ended', EXIT_REASONS[msg.reason] ?? 'The terminal closed. Reconnecting', true);
        else if (msg.type === 'error') {
          // A missing or archived session is final (Try again); a failed attach may pass, so back off and retry.
          this.failed = !msg.retry;
          this.setConn('ended', msg.retry ? `${msg.error} Retrying` : msg.error, msg.retry);
        }
        return;
      }
      this.term.write(new Uint8Array(ev.data as ArrayBuffer));
    };
    ws.onclose = (ev) => {
      if (this.ws !== ws) return;
      this.ws = null;
      if (this.disposed) return;
      if (this.openedAt && Date.now() - this.openedAt >= STABLE_MS) this.attempt = 0;
      this.openedAt = 0;
      if (this.conn !== 'ended') this.setConn('closed');
      // Logged out, password changed or login expired: the 401 handler shows the login screen.
      if (ev.code === WS_UNAUTHORIZED) void checkLogin().then((ok) => ok && this.retry());
      else this.retry();
    };
    ws.onerror = () => ws.close();
  }

  /** Reconnects after the backoff, unless paused, disposed or refused for good. */
  private retry() {
    if (this.disposed || this.paused || this.failed || this.ws) return;
    if (this.timer) clearTimeout(this.timer);
    const delay = Math.min(15000, 500 * 2 ** this.attempt++) + Math.random() * 300;
    this.timer = setTimeout(() => {
      this.timer = null;
      this.connect();
    }, delay);
  }

  /** Reconnect immediately (after Resume, or Try again when the attach failed). */
  reconnect() {
    if (this.timer) clearTimeout(this.timer);
    this.timer = null;
    this.paused = false;
    this.failed = false;
    this.attempt = 0;
    const old = this.ws;
    this.ws = null;
    old?.close();
    this.connect();
  }

  /** Drops the connection until resume(), so a hidden phone stops holding a connection open. */
  pause() {
    if (this.paused || this.disposed) return;
    this.paused = true;
    if (this.timer) clearTimeout(this.timer);
    this.timer = null;
    // Let go of it now: a socket the server already dropped may not report its close for minutes,
    // and resume() reconnects only once there's no socket.
    const old = this.ws;
    this.ws = null;
    this.openedAt = 0;
    old?.close();
    if (old && this.conn !== 'ended') this.setConn('closed');
  }

  resume() {
    if (!this.paused || this.disposed) return;
    this.paused = false;
    this.attempt = 0;
    if (!this.ws && this.started && !this.failed) this.connect();
  }

  /**
   * Presses a key from the key bar. Returns false when not connected (nothing was sent). While held
   * images are being sent, the key is queued behind them.
   */
  sendKey(key: BarKey): boolean {
    return this.input(keySequence(key, this.term.modes.applicationCursorKeysMode));
  }

  /** Arms or cancels the sticky Ctrl. */
  setCtrl(on: boolean) {
    if (this.ctrl === on) return;
    this.ctrl = on;
    this.listeners.forEach((l) => l());
  }

  /**
   * Sends Ctrl plus `ch` and releases the sticky Ctrl. Returns false (and sends nothing) when `ch`
   * has no Ctrl form or the terminal isn't connected; Ctrl stays armed so the next character can use it.
   */
  typeCtrl(ch: string): boolean {
    const seq = ctrlSequence(ch);
    // Through input(), so a Ctrl+letter typed while images are being sent waits behind them.
    if (!seq || !this.input(seq)) return false;
    this.setCtrl(false);
    return true;
  }

  /**
   * Types text from the compose box, then any held images, and submits them with Enter. Resolves
   * false when nothing reached Claude (see flush), so the box can put its text back.
   */
  submit(text: string): Promise<boolean> {
    if (this.sending) return Promise.resolve(false);
    return this.flush(text, 'Not connected to the session. Your message is kept; send it again once it reconnects.');
  }

  /**
   * On touch screens, focusing the terminal only opens the keyboard in typing mode, so taps and
   * swipes don't keep bringing it up. Everywhere else the terminal always takes typing.
   */
  setTyping(on: boolean) {
    this.typing = on;
    if (on) this.endSelecting();
    const ta = this.term.textarea;
    if (!ta) return;
    const wasFocused = document.activeElement === ta;
    if (!on && matches(TOUCH_QUERY)) ta.setAttribute('inputmode', 'none');
    else ta.removeAttribute('inputmode');
    // A focused field only picks up a new inputmode when it's focused again.
    if (on) {
      ta.blur();
      this.term.scrollToBottom();
      this.term.focus();
    } else if (wasFocused) ta.blur();
    this.listeners.forEach((l) => l());
  }

  /** Turns select mode on or off. On, it puts the keyboard away so there's more to select from. */
  setSelecting(on: boolean) {
    if (this.selecting === on) return;
    if (on) {
      this.selecting = true;
      this.stopFling();
      if (this.typing) this.setTyping(false);
      this.showNotice('Drag over lines to copy them', 2500);
    } else this.endSelecting();
    this.listeners.forEach((l) => l());
  }

  private endSelecting() {
    this.selecting = false;
    this.selectAnchor = null;
    this.term.clearSelection();
  }

  /** The buffer line under a finger. */
  private lineAt(y: number): number {
    const r = this.el.getBoundingClientRect();
    return this.term.buffer.active.viewportY + touchRow(y, r.top, r.height, this.term.rows);
  }

  private selectTo(y: number) {
    if (this.selectAnchor === null) return;
    const line = this.lineAt(y);
    this.term.selectLines(Math.min(this.selectAnchor, line), Math.max(this.selectAnchor, line));
  }

  setFontSize(size: number) {
    if (this.term.options.fontSize === size) return;
    this.term.options.fontSize = size;
    this.refit();
  }

  private touchStart = (e: TouchEvent) => {
    this.stopFling();
    const t = e.touches[0];
    if (this.selecting) {
      e.preventDefault();
      e.stopPropagation();
      this.touch = null;
      this.selectAnchor = e.touches.length === 1 && t ? this.lineAt(t.clientY) : null;
      if (t) this.selectTo(t.clientY);
      return;
    }
    this.touch = e.touches.length === 1 && t ? { x: t.clientX, y: t.clientY, travel: 0 } : null;
    this.samples = t ? [{ y: t.clientY, t: e.timeStamp }] : [];
    if (t) this.point = { x: t.clientX, y: t.clientY };
  };

  private touchMove = (e: TouchEvent) => {
    const t = e.touches[0];
    if (this.selecting) {
      e.preventDefault();
      e.stopPropagation();
      if (e.touches.length === 1 && t) this.selectTo(t.clientY);
      return;
    }
    if (!this.touch || e.touches.length !== 1 || !t) return;
    e.preventDefault();
    e.stopPropagation();
    this.touch.travel += t.clientY - this.touch.y;
    this.touch.y = t.clientY;
    this.point = { x: t.clientX, y: t.clientY };
    this.samples.push({ y: t.clientY, t: e.timeStamp });
    // Keep a little more than the window: the lift may come a moment after the last move.
    this.samples = this.samples.filter((s) => e.timeStamp - s.t <= 2 * FLING_WINDOW_MS);
    const { steps, rest } = wheelSteps(this.touch.travel, this.swipeStep());
    this.touch.travel = rest;
    // A finger moving down pulls older lines into view, like a wheel turned up.
    this.queueSteps(steps);
  };

  private touchEnd = (e: TouchEvent) => {
    if (this.selecting) {
      e.preventDefault();
      e.stopPropagation();
      if (this.selectAnchor === null) return;
      this.selectAnchor = null;
      // Lifting the finger is the gesture the clipboard needs.
      const text = this.term.getSelection();
      if (text.trim()) {
        const lines = text.split('\n').length;
        void this.copy(text, lines === 1 ? 'Copied 1 line' : `Copied ${lines} lines`);
      }
      return;
    }
    const wasSwiping = !!this.touch;
    this.touch = null;
    const v = wasSwiping ? flingVelocity(this.samples, e.timeStamp) : 0;
    this.samples = [];
    if (Math.abs(v) >= FLING_MIN) this.startFling(v);
  };

  private touchCancel = () => {
    this.touch = null;
    this.selectAnchor = null;
    this.samples = [];
  };

  /** Finger travel (px) per wheel step. */
  private swipeStep() {
    return Math.max(8, (this.el.clientHeight / Math.max(1, this.term.rows)) * SWIPE_ROWS);
  }

  /** Keeps scrolling after a flick, slowing down, until it stops or a new touch lands. */
  private startFling(velocity: number) {
    this.stopFling();
    let v = velocity;
    let travel = 0;
    let last = performance.now();
    const tick = (now: number) => {
      const dt = Math.min(64, Math.max(0, now - last));
      last = now;
      travel += v * dt;
      v = decayVelocity(v, dt);
      const { steps, rest } = wheelSteps(travel, this.swipeStep());
      travel = rest;
      this.queueSteps(steps);
      this.flingFrame = Math.abs(v) > FLING_STOP ? requestAnimationFrame(tick) : 0;
    };
    this.flingFrame = requestAnimationFrame(tick);
  }

  private stopFling() {
    if (this.flingFrame) cancelAnimationFrame(this.flingFrame);
    this.flingFrame = 0;
  }

  /** Sends wheel steps at most once a frame, together, so Claude redraws once for several. */
  private queueSteps(steps: number) {
    if (!steps) return;
    this.pendingSteps += steps;
    if (!this.flushFrame) {
      this.flushFrame = requestAnimationFrame(() => {
        this.flushFrame = 0;
        this.flushSteps();
      });
    }
  }

  private flushSteps() {
    const n = this.pendingSteps;
    this.pendingSteps = 0;
    if (!n) return;
    const up = n > 0;
    const count = Math.abs(n);
    if (this.term.modes.mouseTrackingMode === 'none') {
      this.term.scrollLines((up ? -3 : 3) * count);
      return;
    }
    const { cols, rows } = this.term;
    const r = this.el.getBoundingClientRect();
    const col = ((this.point.x - r.left) / Math.max(1, r.width)) * cols + 0.5;
    const row = ((this.point.y - r.top) / Math.max(1, r.height)) * rows + 0.5;
    this.send({ type: 'input', data: wheelReport(up, col, row, cols, rows).repeat(count) });
  }

  /**
   * On touch screens the terminal on screen draws with WebGL: full-screen redraws are much cheaper.
   * The renderer is its own chunk, fetched the first time, so desktops never download it.
   */
  private enableWebgl() {
    if (this.webgl || this.webglFailed || this.webglLoading || !matches(TOUCH_QUERY)) return;
    this.webglLoading = true;
    import('@xterm/addon-webgl').then(
      ({ WebglAddon }) => {
        this.webglLoading = false;
        // Detached or disposed while it loaded: the next attach tries again.
        if (this.disposed || this.webgl || !this.el.isConnected) return;
        try {
          const gl = new WebglAddon();
          gl.onContextLoss(() => {
            gl.dispose();
            if (this.webgl === gl) this.webgl = null;
          });
          this.term.loadAddon(gl);
          this.webgl = gl;
          // Two frames on, the canvas has been sized: check it before trusting it.
          requestAnimationFrame(() => requestAnimationFrame(() => this.checkWebgl(gl)));
        } catch {
          // No WebGL2 here: the DOM renderer carries on.
          this.webglFailed = true;
        }
      },
      () => {
        // The chunk didn't load (offline, or a new version replaced it): keep the DOM renderer.
        this.webglLoading = false;
      },
    );
  }

  /**
   * xterm sizes the WebGL canvas from the device-pixel box a ResizeObserver reports. Where that
   * disagrees with devicePixelRatio (headless Chrome's device emulation does), the text is drawn
   * doubled and blurry, so fall back to the DOM renderer for good.
   */
  private checkWebgl(gl: WebglAddon) {
    if (this.webgl !== gl || !this.el.isConnected) return;
    const canvas = this.term.element?.querySelector<HTMLCanvasElement>('.xterm-screen canvas:not(.xterm-link-layer)');
    if (!canvas || !canvas.clientWidth) return;
    if (Math.abs(canvas.width - canvas.clientWidth * window.devicePixelRatio) <= 2) return;
    this.webglFailed = true;
    this.disableWebgl();
    this.refit();
  }

  /** Browsers cap WebGL contexts per page, so only the terminal on screen holds one. */
  private disableWebgl() {
    this.webgl?.dispose();
    this.webgl = null;
  }

  attach(container: HTMLElement) {
    if (this.el.parentElement !== container) container.appendChild(this.el);
    if (this.releaseTimer) clearTimeout(this.releaseTimer);
    this.releaseTimer = null;
    if (!this.started) {
      this.paused = false;
      this.connect();
    } else this.resume();
    // Switched back to it: ask for the session (a socket still connecting asks as it opens).
    this.show();
    this.enableWebgl();
    this.resizeObs?.disconnect();
    this.resizeObs = new ResizeObserver(() => this.scheduleRefit());
    this.resizeObs.observe(container);
    requestAnimationFrame(() => {
      this.refit();
      if (!matches(TOUCH_QUERY) || this.typing) this.term.focus();
    });
  }

  detach() {
    this.hide();
    this.stopFling();
    this.disableWebgl();
    this.resizeObs?.disconnect();
    this.resizeObs = null;
    this.el.remove();
    // On a phone, let go of sessions no longer shown: they hold a connection each.
    if (matches(MOBILE_QUERY)) {
      if (this.releaseTimer) clearTimeout(this.releaseTimer);
      this.releaseTimer = setTimeout(() => {
        this.releaseTimer = null;
        if (!this.el.isConnected) this.pause();
      }, DETACH_RELEASE_MS);
    }
  }

  /** Refits at most once a frame; the on-screen keyboard resizes the page in many small steps. */
  private scheduleRefit() {
    if (!this.refitFrame) {
      this.refitFrame = requestAnimationFrame(() => {
        this.refitFrame = 0;
        this.refit();
      });
    }
  }

  refit() {
    const want = this.wantedSize();
    if (!want) return;
    // Only the active terminal fits its box; another shows the session at its size.
    if (this.active) {
      try {
        this.fit.fit();
      } catch {
        return;
      }
    }
    const { cols, rows } = want;
    if (cols !== this.lastSize.cols || rows !== this.lastSize.rows) {
      this.lastSize = { cols, rows };
      this.send({ type: 'resize', cols, rows });
      // The keyboard just opened or closed while typing here: keep the cursor line in view.
      if (this.typing && document.activeElement === this.term.textarea) this.term.scrollToBottom();
    }
  }

  /** Shows the newest output, where Claude's input line is. */
  scrollToBottom() {
    this.term.scrollToBottom();
  }

  /** Focuses the terminal (a click on it). Elsewhere it asks for the session even when it already had focus. */
  focus() {
    if (this.screen !== 'here') this.show();
    this.term.focus();
  }

  dispose() {
    this.disposed = true;
    if (this.timer) clearTimeout(this.timer);
    if (this.releaseTimer) clearTimeout(this.releaseTimer);
    if (this.noticeTimer) clearTimeout(this.noticeTimer);
    if (this.refitFrame) cancelAnimationFrame(this.refitFrame);
    if (this.flushFrame) cancelAnimationFrame(this.flushFrame);
    this.stopFling();
    this.disableWebgl();
    this.held.clear();
    this.resizeObs?.disconnect();
    this.ws?.close();
    this.term.dispose();
    this.el.remove();
  }
}

const cache = new Map<string, SessionTerminal>();

export function getTerminal(sessionId: string): SessionTerminal {
  let t = cache.get(sessionId);
  if (!t) {
    t = new SessionTerminal(sessionId);
    cache.set(sessionId, t);
  }
  return t;
}

/**
 * Whether any session's terminal holds images for Claude. They live only in the page, on terminals
 * that outlast their pane (switching sessions keeps them), so an update's reload waits on this.
 */
export function hasHeldImages(): boolean {
  for (const t of cache.values()) if (t.held.size) return true;
  return false;
}

export function peekTerminal(sessionId: string): SessionTerminal | undefined {
  return cache.get(sessionId);
}

export function disposeTerminal(sessionId: string) {
  cache.get(sessionId)?.dispose();
  cache.delete(sessionId);
}

/** Drop terminals whose sessions no longer exist or were archived. */
export function pruneTerminals(liveIds: Set<string>) {
  for (const id of [...cache.keys()]) if (!liveIds.has(id)) disposeTerminal(id);
}

export function disposeAllTerminals() {
  for (const id of [...cache.keys()]) disposeTerminal(id);
}

export function terminalFontSize(): number {
  return storedFont() ?? defaultFont();
}

/** Sets the font of every terminal, remembered in this browser. */
export function setTerminalFontSize(size: number) {
  const v = clampFont(size);
  try {
    localStorage.setItem(FONT_KEY, String(v));
  } catch {
    /* storage blocked: the size just isn't remembered */
  }
  for (const t of cache.values()) t.setFontSize(v);
}

/**
 * On phones, let go of the terminals once the page has been hidden a while (and of each terminal
 * taken off screen: see detach()), so a phone in a pocket holds no connections open.
 */
const HIDDEN_RELEASE_MS = 30_000;
let hiddenTimer: ReturnType<typeof setTimeout> | null = null;
/** When a phone's page was hidden, or 0. */
let hiddenAt = 0;
if (typeof document !== 'undefined') {
  document.addEventListener('visibilitychange', () => {
    if (hiddenTimer) clearTimeout(hiddenTimer);
    hiddenTimer = null;
    if (document.hidden) {
      // The screen that has a session hands it to the next screen that asks, at once.
      for (const t of cache.values()) if (t.el.isConnected) t.hide();
      if (!matches(MOBILE_QUERY)) return;
      hiddenAt = Date.now();
      hiddenTimer = setTimeout(() => {
        for (const t of cache.values()) t.pause();
      }, HIDDEN_RELEASE_MS);
    } else {
      // A phone that suspends a hidden page runs no timers, so the release above may never have
      // happened, and the server has likely dropped the sockets since. Measured by the clock.
      if (hiddenAt && Date.now() - hiddenAt >= HIDDEN_RELEASE_MS) for (const t of cache.values()) t.pause();
      hiddenAt = 0;
      // Only terminals on screen: ones taken off screen on a phone stay released until shown.
      for (const t of cache.values()) if (t.el.isConnected) t.resume();
      // Shown again: each on screen asks for its session (one still connecting asks as it opens).
      for (const t of cache.values()) if (t.el.isConnected) t.show();
    }
  });
}

export type { SessionTerminal };
