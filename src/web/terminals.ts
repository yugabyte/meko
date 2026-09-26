// One xterm.js Terminal (and its WebSocket) per session, cached across tab
// switches so that returning to a tab is instant and keeps its scrollback.

import { Terminal } from '@xterm/xterm';
import { FitAddon } from '@xterm/addon-fit';
import { WebLinksAddon } from '@xterm/addon-web-links';
import type { WebglAddon } from '@xterm/addon-webgl';
import type { TermClientMessage, TermServerMessage } from '../shared/types';
import { wsUrl } from './api';
import { copyText, decodeOsc52 } from './clipboard';
import { MOBILE_QUERY, TOUCH_QUERY, matches } from './hooks/useMedia';
import {
  clampFont, decayVelocity, flingVelocity, keySequence, pasteText, wheelReport, wheelSteps, FLING_WINDOW_MS,
  type BarKey, type TouchSample,
} from './keys';

const IS_MAC = /Mac|iPhone|iPad/.test(navigator.platform);

export type TermConn = 'connecting' | 'open' | 'closed' | 'ended';

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
 * Finger travel, in rows, per mouse-wheel step. Claude Code runs full screen and takes the mouse,
 * so tmux hands it the steps and Claude Code decides how far each one scrolls.
 */
const SWIPE_ROWS = 2.5;
/** A swipe let go faster than this (px/ms) coasts; momentum stops below FLING_STOP. */
const FLING_MIN = 0.3;
const FLING_STOP = 0.05;
/** Pause between a compose paste and the Enter that submits it, as the server does for prompts. */
const SUBMIT_DELAY = 300;

class SessionTerminal {
  readonly term: Terminal;
  readonly el: HTMLDivElement;
  private fit = new FitAddon();
  private ws: WebSocket | null = null;
  private attempt = 0;
  private timer: ReturnType<typeof setTimeout> | null = null;
  private disposed = false;
  private resizeObs: ResizeObserver | null = null;
  private lastSize = { cols: 0, rows: 0 };
  private refitFrame = 0;
  /** Disconnected on purpose while the page is hidden; reconnects when it's shown again. */
  private paused = false;
  private touch: { x: number; y: number; travel: number } | null = null;
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
  conn: TermConn = 'connecting';
  message: string | null = null;
  /** A short-lived note after copying ("Copied", or why it failed). */
  notice: string | null = null;
  private noticeTimer: ReturnType<typeof setTimeout> | null = null;
  private listeners = new Set<() => void>();

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
      // tmux has the mouse, so a plain drag selects in tmux (copied through OSC 52 below);
      // Shift-drag, or Option-drag on a Mac, selects in the browser instead.
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
    this.el.addEventListener('touchstart', this.touchStart, { capture: true, passive: true });
    this.el.addEventListener('touchmove', this.touchMove, { capture: true, passive: false });
    this.el.addEventListener('touchend', this.touchEnd, { capture: true, passive: true });
    this.el.addEventListener('touchcancel', this.touchCancel, { capture: true, passive: true });
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
    this.term.onData((data) => this.send({ type: 'input', data }));
    this.term.onBinary((data) => this.send({ type: 'input', data }));
    // tmux sends a mouse selection as OSC 52 (set-clipboard on); put it on the browser clipboard.
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
      if (mod && (e.key === 'k' || e.key === 'K')) return false;
      if (mod && /^[1-9]$/.test(e.key)) return false;
      if (e.ctrlKey && e.key === 'Tab') return false;
      return true;
    });
    this.connect();
  }

  subscribe(fn: () => void): () => void {
    this.listeners.add(fn);
    return () => this.listeners.delete(fn);
  }

  private async copy(text: string) {
    const ok = await copyText(text);
    this.notice = ok
      ? 'Copied'
      : `Couldn’t copy: the browser blocked the clipboard. Select with ${IS_MAC ? 'Option' : 'Shift'}-drag and press ${IS_MAC ? '⌘C' : 'Ctrl+C'}.`;
    this.listeners.forEach((l) => l());
    if (this.noticeTimer) clearTimeout(this.noticeTimer);
    this.noticeTimer = setTimeout(() => {
      this.notice = null;
      this.listeners.forEach((l) => l());
    }, ok ? 1500 : 6000);
  }

  private setConn(c: TermConn, message: string | null = null) {
    this.conn = c;
    this.message = message;
    this.listeners.forEach((l) => l());
  }

  private send(msg: TermClientMessage) {
    if (this.ws?.readyState === WebSocket.OPEN) this.ws.send(JSON.stringify(msg));
  }

  private connect() {
    if (this.disposed) return;
    this.setConn('connecting');
    const ws = new WebSocket(wsUrl(`/ws/term/${encodeURIComponent(this.sessionId)}`));
    ws.binaryType = 'arraybuffer';
    this.ws = ws;
    ws.onopen = () => {
      this.attempt = 0;
      this.setConn('open');
      this.lastSize = { cols: 0, rows: 0 };
      this.refit();
    };
    ws.onmessage = (ev) => {
      if (typeof ev.data === 'string') {
        let msg: TermServerMessage;
        try {
          msg = JSON.parse(ev.data);
        } catch {
          return;
        }
        if (msg.type === 'exit') this.setConn('ended', msg.reason);
        else if (msg.type === 'error') this.setConn('ended', msg.error);
        return;
      }
      this.term.write(new Uint8Array(ev.data as ArrayBuffer));
    };
    ws.onclose = () => {
      if (this.ws === ws) this.ws = null;
      if (this.disposed) return;
      if (this.conn !== 'ended') this.setConn('closed');
      if (this.paused) return;
      const delay = Math.min(15000, 500 * 2 ** this.attempt++) + Math.random() * 300;
      this.timer = setTimeout(() => this.connect(), delay);
    };
    ws.onerror = () => ws.close();
  }

  /** Reconnect immediately (used after Resume). */
  reconnect() {
    if (this.timer) clearTimeout(this.timer);
    this.paused = false;
    this.attempt = 0;
    this.ws?.close();
    if (!this.ws) this.connect();
  }

  /** Drops the connection until resume(), so this viewer stops holding the tmux window at its size. */
  pause() {
    if (this.paused || this.disposed) return;
    this.paused = true;
    if (this.timer) clearTimeout(this.timer);
    this.timer = null;
    this.ws?.close();
  }

  resume() {
    if (!this.paused || this.disposed) return;
    this.paused = false;
    this.attempt = 0;
    if (!this.ws) this.connect();
  }

  /** Presses a key from the key bar. */
  sendKey(key: BarKey) {
    this.send({ type: 'input', data: keySequence(key, this.term.modes.applicationCursorKeysMode) });
  }

  /** Types text from the compose box and submits it with Enter. */
  submit(text: string) {
    this.send({ type: 'input', data: pasteText(text, this.term.modes.bracketedPasteMode) });
    setTimeout(() => this.send({ type: 'input', data: '\r' }), SUBMIT_DELAY);
  }

  /**
   * On touch screens, focusing the terminal only opens the keyboard in typing mode, so taps and
   * swipes don't keep bringing it up. Everywhere else the terminal always takes typing.
   */
  setTyping(on: boolean) {
    this.typing = on;
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

  setFontSize(size: number) {
    if (this.term.options.fontSize === size) return;
    this.term.options.fontSize = size;
    this.refit();
  }

  private touchStart = (e: TouchEvent) => {
    this.stopFling();
    const t = e.touches[0];
    this.touch = e.touches.length === 1 && t ? { x: t.clientX, y: t.clientY, travel: 0 } : null;
    this.samples = t ? [{ y: t.clientY, t: e.timeStamp }] : [];
    if (t) this.point = { x: t.clientX, y: t.clientY };
  };

  private touchMove = (e: TouchEvent) => {
    const t = e.touches[0];
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
    const wasSwiping = !!this.touch;
    this.touch = null;
    const v = wasSwiping ? flingVelocity(this.samples, e.timeStamp) : 0;
    this.samples = [];
    if (Math.abs(v) >= FLING_MIN) this.startFling(v);
  };

  private touchCancel = () => {
    this.touch = null;
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
    this.stopFling();
    this.disableWebgl();
    this.resizeObs?.disconnect();
    this.resizeObs = null;
    this.el.remove();
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
    if (!this.el.isConnected || this.el.clientWidth === 0) return;
    try {
      this.fit.fit();
    } catch {
      return;
    }
    const { cols, rows } = this.term;
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

  focus() {
    this.term.focus();
  }

  dispose() {
    this.disposed = true;
    if (this.timer) clearTimeout(this.timer);
    if (this.noticeTimer) clearTimeout(this.noticeTimer);
    if (this.refitFrame) cancelAnimationFrame(this.refitFrame);
    if (this.flushFrame) cancelAnimationFrame(this.flushFrame);
    this.stopFling();
    this.disableWebgl();
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
 * tmux draws a window at the size of its smallest viewer, so a phone left attached narrows the same
 * session on a desktop. On phones, let go of the terminals once the page has been hidden a while.
 */
const HIDDEN_RELEASE_MS = 30_000;
let hiddenTimer: ReturnType<typeof setTimeout> | null = null;
if (typeof document !== 'undefined') {
  document.addEventListener('visibilitychange', () => {
    if (hiddenTimer) clearTimeout(hiddenTimer);
    hiddenTimer = null;
    if (document.hidden) {
      if (!matches(MOBILE_QUERY)) return;
      hiddenTimer = setTimeout(() => {
        for (const t of cache.values()) t.pause();
      }, HIDDEN_RELEASE_MS);
    } else {
      for (const t of cache.values()) t.resume();
    }
  });
}

export type { SessionTerminal };
