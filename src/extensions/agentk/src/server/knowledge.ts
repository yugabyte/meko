import { KB_TYPES_TEXT, isKnowledgeFile } from '../shared/knowledge.js';
import { MAX_KB_UPLOAD_BYTES, type KnowledgeUpload } from '../shared/types.js';
import type { Core } from './core.js';
import { errMsg } from './log.js';
import { MekoApiError, type MekoApi, type PresignedUpload } from './mekoapi.js';
import { HttpError } from './projects.js';
import { dedicatedDatapack } from './store.js';

/** How often an upload's indexing is checked: every 5 s for its first minute, then every 30 s. */
const FAST_POLL_MS = 5000;
const FAST_FOR_MS = 60_000;
const SLOW_POLL_MS = 30_000;
/** When AgentK stops following an upload that's still indexing. */
const GIVE_UP_MS = 15 * 60_000;
/** How an upload AgentK followed ended. */
export type KnowledgeResult = 'indexed' | 'failed' | 'unknown';

/** Meko's document states that end an upload without indexing it. */
const FAILED_STATES = new Set(['failed', 'abandoned', 'oversized']);

export interface KnowledgeTiming {
  fastPollMs: number;
  fastForMs: number;
  slowPollMs: number;
  giveUpMs: number;
}

const TIMING: KnowledgeTiming = { fastPollMs: FAST_POLL_MS, fastForMs: FAST_FOR_MS, slowPollMs: SLOW_POLL_MS, giveUpMs: GIVE_UP_MS };

/** An upload Meko is still indexing, followed until it's done. Kept in memory only: a restart drops them. */
interface Followed {
  /** The session to tell (a `knowledge` event), or null for one AgentK uploaded itself (`onDone`). */
  sessionId: string | null;
  onDone: ((result: KnowledgeResult) => void) | null;
  datapackId: string;
  documentKey: string;
  filename: string;
  startedAt: number;
  timer: NodeJS.Timeout | null;
  /** A passing failure was logged: later ones aren't, so a Meko that's down logs once per upload. */
  warned: boolean;
}

/**
 * Whether a failed status check is worth trying again: Meko unreachable or busy, or a 404 in
 * Meko's envelope while it's still registering the document. A refused key, a Meko without the
 * route (a 404 without the envelope) or no key at all won't change by waiting.
 */
function passing(e: unknown): boolean {
  if (!(e instanceof MekoApiError)) return true;
  if (e.status === 0) return e.code === 'UNREACHABLE';
  if (e.status === 404) return e.code !== 'HTTP_404';
  return e.status === 429 || e.status >= 500;
}

/**
 * Checks a file's name for the knowledge base: its base name, with an extension Meko takes. Returns
 * the name, or throws a 400 that says what's wrong.
 */
export function knowledgeName(name: unknown): string {
  if (typeof name !== 'string' || !name.trim()) throw new HttpError(400, 'Name the file: name is missing.');
  const n = name.trim();
  if (/[/\\]/.test(n) || n === '.' || n === '..' || n.includes('\0')) throw new HttpError(400, `"${n}" isn't a file name: send only its base name.`);
  if (!isKnowledgeFile(n)) throw new HttpError(400, `The knowledge base takes ${KB_TYPES_TEXT} files, not ${n}.`);
  return n;
}

/** Meko's refusal of an upload, in words the user can act on, with the status to answer with. */
export function uploadError(e: unknown, datapackName: string): HttpError {
  if (!(e instanceof MekoApiError)) return new HttpError(502, `Couldn't add the file to the knowledge base: ${errMsg(e)}`);
  // Meko's file storage refusing the post itself (postPresigned): a 403 is its link expiring.
  if (e.code.startsWith('STORAGE_')) {
    return new HttpError(502, e.status === 403 ? "Meko's file storage refused the upload: its link expired or was refused. Try again."
      : `Meko's file storage refused the upload: ${e.message} Try again in a minute; if it keeps failing, add it from Meko's web app.`);
  }
  // Never a 401 to the browser: that signs it out.
  if (e.status === 401 || /SCOPE/i.test(e.code)) return new HttpError(502, "Meko didn't accept AgentK's token. Run meko restart, then try again.");
  if (e.status === 403) {
    return new HttpError(403, `You can't add files to ${datapackName}: only its owner and maintainers can. Ask the owner for maintainer access.`);
  }
  // A 404 without Meko's envelope: the route itself isn't there (an older Meko).
  if (e.status === 404 && e.code === 'HTTP_404') {
    return new HttpError(502, "This Meko doesn't take uploads from AgentK yet. Add the file from Meko's web app, or ask whoever runs that Meko server to update it.");
  }
  if (e.status === 404) return new HttpError(502, `Meko couldn't find ${datapackName}: ${e.message}. Check the project's datapack on its home page.`);
  if (e.status === 400 || e.status === 413 || e.status === 415) return new HttpError(e.status, e.message);
  // No room left on the owner's plan: Meko's message says which plan to move to.
  if (e.status === 402) return new HttpError(413, e.message);
  if (e.code === 'S3_DISABLED') {
    return new HttpError(502, `This Meko isn't set up to take files (${e.message}). Ask whoever runs that Meko server to set up its file storage.`);
  }
  if (e.status === 0) return new HttpError(502, e.message);
  return new HttpError(502, `Meko couldn't add the file: ${e.message} Try again in a minute; if it keeps failing, add it from Meko's web app.`);
}

/**
 * Files added to a session's knowledge base (POST /api/sessions/:id/knowledge): checked, sent to
 * the project's dedicated datapack through Meko's presigned upload (the key stays on the server), and
 * followed while Meko indexes them, with a `knowledge` event when it's done.
 */
export class KnowledgeService {
  private followed = new Map<string, Followed>();
  private stopped = false;
  /** Bumped by forget(): an upload that started before it doesn't follow its document on the new Meko. */
  private generation = 0;

  constructor(private core: Core, private api: MekoApi, private timing: KnowledgeTiming = TIMING) {}

  async upload(id: string, name: unknown, body: unknown): Promise<KnowledgeUpload> {
    const row = this.core.sessions.row(id);
    if (row.desired !== 'running') throw new HttpError(409, 'This session is archived.');
    if (!this.core.mekoStatus.configured) throw new HttpError(409, "Meko isn't running on this computer, so files can't go to a knowledge base. Start it with meko start.");
    const dp = dedicatedDatapack(this.core.db, row.project_id);
    if (!dp) throw new HttpError(409, "This session's project has no datapack. Give it one on the project's home page.");
    const filename = knowledgeName(name);
    // A JSON body is parsed before it gets here: only raw bytes are a file.
    if (!Buffer.isBuffer(body)) throw new HttpError(415, "Send the file's bytes as application/octet-stream.");
    if (!body.length) throw new HttpError(400, `${filename} is empty.`);
    if (body.length > MAX_KB_UPLOAD_BYTES) throw new HttpError(413, `${filename} is over ${MAX_KB_UPLOAD_BYTES / 1024 / 1024} MB.`);

    const generation = this.generation;
    let upload: PresignedUpload | null = null;
    let error: unknown = null;
    try {
      // Meko signs a post for these bytes, they go to its storage, and Meko indexes them from there.
      upload = await this.api.requestUpload(dp.id, filename, body);
      if (!upload.alreadyUploaded) await this.api.postPresigned(upload, filename, body);
    } catch (e) {
      error = e;
    }
    if (error || !upload) throw uploadError(error, dp.name);
    const status = upload.alreadyUploaded ? 'already_uploaded' : 'processing';
    // Meko's settings changed while it uploaded: its document may not be on the Meko AgentK asks now.
    if (status === 'processing' && generation === this.generation) this.follow(row.id, null, dp.id, upload.key, filename);
    return { filename, documentKey: upload.key, datapackName: dp.name, status };
  }

  /**
   * Adds a file AgentK has (a workshop's gotchas) to a datapack's knowledge base, and follows its
   * indexing: <onDone> gets the result once (`indexed` at once when Meko already had these bytes,
   * `unknown` when AgentK stops following it: Meko changed or went off). A refused upload throws,
   * in uploadError's words. While the server stops, nothing more is called.
   */
  async uploadFile(datapack: { id: string; name: string }, filename: string, bytes: Uint8Array, onDone: (result: KnowledgeResult) => void): Promise<void> {
    const generation = this.generation;
    let upload: PresignedUpload;
    try {
      upload = await this.api.requestUpload(datapack.id, filename, bytes);
      if (!upload.alreadyUploaded) await this.api.postPresigned(upload, filename, bytes);
    } catch (e) {
      throw uploadError(e, datapack.name);
    }
    if (upload.alreadyUploaded) onDone('indexed');
    else if (generation !== this.generation) this.dropped(onDone);
    else this.follow(null, onDone, datapack.id, upload.key, filename);
  }

  /** Stops following every upload (the server is stopping). */
  stop(): void {
    this.stopped = true;
    this.forget();
  }

  /** An upload with a callback that AgentK stopped following: it ends as `unknown`, unless the server is stopping. */
  private dropped(onDone: Followed['onDone']): void {
    if (onDone && !this.stopped) onDone('unknown');
  }

  /**
   * Stops following every upload, telling no session: Meko was turned off, or its URL or key
   * changed, so their datapack and document keys may not be on the Meko AgentK talks to now. An
   * upload with a callback ends as `unknown`.
   */
  forget(): void {
    this.generation++;
    const gone = [...this.followed.values()];
    for (const f of gone) if (f.timer) clearTimeout(f.timer);
    this.followed.clear();
    for (const f of gone) this.dropped(f.onDone);
  }

  private follow(sessionId: string | null, onDone: Followed['onDone'], datapackId: string, documentKey: string, filename: string): void {
    const key = `${datapackId}:${documentKey}`;
    if (this.stopped || this.followed.has(key)) {
      this.dropped(onDone);
      return;
    }
    const f: Followed = { sessionId, onDone, datapackId, documentKey, filename, startedAt: Date.now(), timer: null, warned: false };
    this.followed.set(key, f);
    this.schedule(key, f);
  }

  private schedule(key: string, f: Followed): void {
    const t = this.timing;
    const ms = Date.now() - f.startedAt < t.fastForMs ? t.fastPollMs : t.slowPollMs;
    f.timer = setTimeout(() => void this.check(key, f), ms);
    f.timer.unref?.();
  }

  private async check(key: string, f: Followed): Promise<void> {
    // Meko turned off: no Meko calls at all.
    if (!this.core.mekoStatus.configured) {
      this.followed.delete(key);
      this.dropped(f.onDone);
      return;
    }
    let status = '';
    let stuck = false;
    try {
      status = await this.api.documentStatus(f.datapackId, f.documentKey);
    } catch (e) {
      stuck = !passing(e);
      const quiet = e instanceof MekoApiError && e.status === 404 && !stuck;
      if (!quiet && (stuck || !f.warned)) this.core.log.warn(`checking how ${f.filename} is indexing failed${stuck ? '; AgentK stopped checking' : ''}`, errMsg(e));
      if (!quiet) f.warned = true;
    }
    if (this.stopped || this.followed.get(key) !== f) return;
    // An upload whose link expired unused, or bigger than it was signed for, never gets indexed.
    const result: KnowledgeResult | null = status === 'completed' ? 'indexed' : FAILED_STATES.has(status) ? 'failed'
      : stuck || Date.now() - f.startedAt >= this.timing.giveUpMs ? 'unknown' : null;
    if (!result) {
      this.schedule(key, f);
      return;
    }
    this.followed.delete(key);
    if (f.onDone) f.onDone(result);
    else if (f.sessionId) this.core.notifyKnowledge({ sessionId: f.sessionId, filename: f.filename, result });
  }
}
