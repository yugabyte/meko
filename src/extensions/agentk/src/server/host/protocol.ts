/**
 * The session host's protocol on `host.sock`, shared by the host, the server's client and
 * `me_koder attach`. Each connection opens with one JSON line (a request). A request is
 * answered with one JSON line and the connection closes; `attach` is answered with one JSON
 * line and the connection then carries frames both ways (see `encodeFrame`).
 */

/** Bumped when a request or a frame changes in a way an older peer can't read. */
export const PROTOCOL = 1;
/** The oldest host protocol this server still works with. A host older than that must be restarted. */
export const MIN_PROTOCOL = 1;

export interface PaneState {
  name: string;
  dead: boolean;
  pid: number;
  exitStatus: number | null;
  cols: number;
  rows: number;
}

export type HostRequest =
  | { op: 'hello' }
  | { op: 'list' }
  /** Starts `argv` in a new pane, or in place of a dead one; never replaces a live pane. */
  | { op: 'spawn'; name: string; cwd: string; argv: string[]; cols?: number; rows?: number }
  | { op: 'kill'; name: string }
  /** Raw input, as typed. */
  | { op: 'write'; name: string; data: string }
  /** A paste (bracketed when the program turned bracketed paste on), then Enter after a pause. */
  | { op: 'paste'; name: string; text: string; enter: boolean }
  /** The visible screen as plain text. */
  | { op: 'capture'; name: string }
  /** Ends every pane and exits. */
  | { op: 'stop' }
  | { op: 'attach'; name: string; cols?: number; rows?: number };

export interface HelloReply {
  protocol: number;
  version: string;
  pid: number;
  startedAt: number;
}

/** Frame types. Host → viewer: output bytes, or a JSON control message. Viewer → host: input bytes, or JSON control. */
export const FRAME_DATA = 1;
export const FRAME_CONTROL = 2;

/** Control messages the host sends a viewer. `active`: this viewer's size is the pane's. */
export type HostControl =
  | { type: 'size'; cols: number; rows: number; active: boolean }
  | { type: 'exit'; reason: 'killed' | 'stopped' };

/** Control messages a viewer sends. `active`: the viewer was used (typed, focused) and takes over the size. */
export type ViewerControl = { type: 'resize'; cols: number; rows: number } | { type: 'active' };

/** Frames larger than this are refused (a broken or hostile peer). */
export const MAX_FRAME = 8 * 1024 * 1024;

/** A request line longer than this is refused. Prompts go in one line, so it is generous. */
export const MAX_REQUEST = 16 * 1024 * 1024;

export const clampCols = (n: number) => Math.max(20, Math.min(500, Math.floor(n)));
export const clampRows = (n: number) => Math.max(5, Math.min(300, Math.floor(n)));

/** One frame: a type byte, the payload length (4 bytes, big-endian), then the payload. */
export function encodeFrame(type: number, payload: Buffer | string): Buffer {
  const body = typeof payload === 'string' ? Buffer.from(payload, 'utf8') : payload;
  const head = Buffer.alloc(5);
  head.writeUInt8(type, 0);
  head.writeUInt32BE(body.length, 1);
  return Buffer.concat([head, body]);
}

/** Splits a byte stream into frames, whatever chunks it arrives in. */
export class FrameReader {
  private buf: Buffer = Buffer.alloc(0);

  constructor(private onFrame: (type: number, payload: Buffer) => void) {}

  /** Throws on a frame over MAX_FRAME; the caller drops the connection. */
  push(chunk: Buffer): void {
    this.buf = this.buf.length ? Buffer.concat([this.buf, chunk]) : chunk;
    while (this.buf.length >= 5) {
      const len = this.buf.readUInt32BE(1);
      if (len > MAX_FRAME) throw new Error(`frame of ${len} bytes is too large`);
      if (this.buf.length < 5 + len) return;
      const type = this.buf.readUInt8(0);
      const payload = this.buf.subarray(5, 5 + len);
      this.buf = this.buf.subarray(5 + len);
      this.onFrame(type, payload);
    }
  }
}

/**
 * Reads the first line of a connection, then hands the rest over. Returns a function to feed
 * chunks to; `onLine` gets the line (without the newline) and whatever bytes followed it.
 */
export function lineReader(max: number, onLine: (line: string, rest: Buffer) => void, onTooLong: () => void): (chunk: Buffer) => void {
  let buf: Buffer = Buffer.alloc(0);
  let done = false;
  return (chunk) => {
    if (done) return;
    buf = buf.length ? Buffer.concat([buf, chunk]) : chunk;
    const nl = buf.indexOf(0x0a);
    if (nl < 0) {
      if (buf.length > max) {
        done = true;
        onTooLong();
      }
      return;
    }
    done = true;
    onLine(buf.subarray(0, nl).toString('utf8'), buf.subarray(nl + 1));
  };
}

/** A parsed control message, or null when the payload isn't one. */
export function parseControl<T extends { type: string }>(payload: Buffer): T | null {
  try {
    const m = JSON.parse(payload.toString('utf8')) as unknown;
    return m && typeof m === 'object' && typeof (m as { type?: unknown }).type === 'string' ? (m as T) : null;
  } catch {
    return null;
  }
}
