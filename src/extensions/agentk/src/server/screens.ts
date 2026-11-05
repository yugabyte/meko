import { SCREEN_IDLE_MS, type ScreenName, type ScreenState } from '../shared/types.js';

/**
 * One screen's terminal socket on a session: a browser tab, window or phone. `id` is the page's
 * (one per page load), so a reconnect from the same page is the same screen.
 */
export interface Screen {
  readonly id: string;
  /** The browser's id, or '' when it has none. */
  readonly browser: string;
  /** Its browser and system ("Chrome on macOS"). */
  readonly name: string;
  /** Told its state whenever it changes. */
  tell(state: ScreenState): void;
}

/** Why a session moved to a screen, for the log. */
export type MoveReason = 'first' | 'same screen' | 'Move to this screen' | 'other screen idle' | 'other screen not showing it' | 'other screen closed';

type Entry = { screen: Screen; showing: boolean; told: string };

/**
 * The rules for which screen has a session (docs/design/architecture.md, "One screen per session").
 * Only that screen gets the session's output and sends it input and its size. Nobody has it until a
 * screen that shows it asks. Another screen that shows the session asks for it, and gets it at once
 * when the screen that has it is closed, isn't showing it, or hasn't been used for SCREEN_IDLE_MS;
 * otherwise it waits until it has been that long (each use restarts the wait). `move` takes it at
 * once. A screen that lost the session asks again only when it shows it again. No sockets or host
 * calls here: terminal.ts acts on what screens are told.
 */
export class ScreenBoard {
  private entries: Entry[] = [];
  private owner: Entry | null = null;
  /** The id of the screen that has the session, kept after it closes so its reconnect keeps it. */
  private ownerId: string | null = null;
  private ownerShowing = false;
  private lastUse = 0;
  private waiting: Entry | null = null;
  private timer: NodeJS.Timeout | null = null;

  constructor(private readonly onMove: (to: Screen, from: Screen | null, why: MoveReason) => void = () => {}) {}

  get empty(): boolean {
    return !this.entries.length;
  }

  /** A screen connects; `ask` when it shows the session. */
  join(screen: Screen, ask: boolean): void {
    const e: Entry = { screen, showing: ask, told: '' };
    this.entries.push(e);
    if (screen.id === this.ownerId) this.give(e, 'same screen');
    else if (ask) this.ask(e);
    this.sync();
  }

  leave(screen: Screen): void {
    const e = this.find(screen);
    if (!e) return;
    this.entries = this.entries.filter((x) => x !== e);
    if (this.waiting === e) this.stopWaiting();
    if (this.owner === e) {
      this.owner = null;
      if (this.waiting) this.give(this.waiting, 'other screen closed');
    }
    if (this.empty) this.stopWaiting();
    this.sync();
  }

  /** The screen shows the session: the screen that has it is back, any other asks for it. */
  show(screen: Screen): void {
    const e = this.find(screen);
    if (!e) return;
    e.showing = true;
    if (this.owner === e) this.ownerShowing = true;
    else this.ask(e);
    this.sync();
  }

  /** The screen stopped showing the session: one that has it hands it to a waiting screen. */
  hide(screen: Screen): void {
    const e = this.find(screen);
    if (!e) return;
    e.showing = false;
    if (this.owner === e) {
      this.ownerShowing = false;
      if (this.waiting) this.give(this.waiting, 'other screen not showing it');
    } else if (this.waiting === e) this.stopWaiting();
    this.sync();
  }

  /** Move to this screen. */
  move(screen: Screen): void {
    const e = this.find(screen);
    if (!e || this.owner === e) return;
    e.showing = true;
    this.give(e, 'Move to this screen');
    this.sync();
  }

  /** The screen that has the session was used: it keeps it SCREEN_IDLE_MS longer. */
  used(screen: Screen): void {
    if (this.owner?.screen !== screen) return;
    this.lastUse = Date.now();
    if (this.waiting) {
      this.schedule();
      this.sync();
    }
  }

  private find(screen: Screen): Entry | undefined {
    return this.entries.find((e) => e.screen === screen);
  }

  private ask(e: Entry) {
    if (!this.owner) return this.give(e, this.ownerId ? 'other screen closed' : 'first');
    if (!this.ownerShowing) return this.give(e, 'other screen not showing it');
    if (Date.now() - this.lastUse >= SCREEN_IDLE_MS) return this.give(e, 'other screen idle');
    // The latest screen to ask waits; an earlier one goes back to seeing it elsewhere.
    this.waiting = e;
    this.schedule();
  }

  private give(e: Entry, why: MoveReason) {
    const from = this.owner;
    this.stopWaiting();
    this.owner = e;
    this.ownerId = e.screen.id;
    this.ownerShowing = e.showing;
    // A fresh wait, so two screens that both just opened it don't swap at once.
    this.lastUse = Date.now();
    if (from !== e) this.onMove(e.screen, from?.screen ?? null, why);
  }

  private schedule() {
    if (this.timer) clearTimeout(this.timer);
    this.timer = setTimeout(() => {
      this.timer = null;
      if (!this.waiting) return;
      if (Date.now() - this.lastUse >= SCREEN_IDLE_MS) this.give(this.waiting, 'other screen idle');
      else this.schedule();
      this.sync();
    }, Math.max(0, this.lastUse + SCREEN_IDLE_MS - Date.now()));
  }

  private stopWaiting() {
    this.waiting = null;
    if (this.timer) clearTimeout(this.timer);
    this.timer = null;
  }

  private nameFor(e: Entry, other: Entry): ScreenName {
    return { name: other.screen.name, sameBrowser: !!e.screen.browser && e.screen.browser === other.screen.browser };
  }

  private stateOf(e: Entry): ScreenState {
    if (e === this.owner) return this.waiting ? { state: 'here', waiting: this.nameFor(e, this.waiting) } : { state: 'here' };
    if (e === this.waiting && this.owner)
      return { state: 'waiting', other: this.nameFor(e, this.owner), moveInMs: Math.max(0, this.lastUse + SCREEN_IDLE_MS - Date.now()) };
    return { state: 'elsewhere', other: this.owner ? this.nameFor(e, this.owner) : null };
  }

  /** Tells each screen its state when it changed. Losers first, so a host stream closes before the next opens. */
  private sync() {
    const order = [...this.entries].sort((a, b) => Number(a === this.owner) - Number(b === this.owner));
    for (const e of order) {
      const state = this.stateOf(e);
      const told = JSON.stringify(state);
      if (told === e.told) continue;
      e.told = told;
      e.screen.tell(state);
    }
  }
}

/** Browsers and systems by user agent, first match wins (Edge and Opera say Chrome too, Chrome says Safari). */
const BROWSERS: [RegExp, string][] = [
  [/Edg(e|A|iOS)?\//, 'Edge'],
  [/OPR\/|Opera/, 'Opera'],
  [/Firefox\/|FxiOS\//, 'Firefox'],
  [/Chrome\/|CriOS\/|Chromium\//, 'Chrome'],
  [/Safari\//, 'Safari'],
];
const SYSTEMS: [RegExp, string][] = [
  [/iPhone/, 'iPhone'],
  [/iPad/, 'iPad'],
  [/Android/, 'Android'],
  [/CrOS/, 'ChromeOS'],
  [/Mac OS X|Macintosh/, 'macOS'],
  [/Windows/, 'Windows'],
  [/Linux/, 'Linux'],
];

/** A browser and system from a user agent, to name another screen ("Chrome on macOS"). */
export function screenName(userAgent = ''): string {
  const browser = BROWSERS.find(([re]) => re.test(userAgent))?.[1];
  const system = SYSTEMS.find(([re]) => re.test(userAgent))?.[1];
  if (browser && system) return `${browser} on ${system}`;
  return browser ?? (system ? `a browser on ${system}` : 'unknown browser');
}
