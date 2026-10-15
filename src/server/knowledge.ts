import { KB_TYPES_TEXT, isKnowledgeFile } from '../shared/knowledge.js';
import { MAX_KB_UPLOAD_BYTES, type KnowledgeUpload } from '../shared/types.js';
import type { Core } from './core.js';
import { errMsg } from './log.js';
import { MekoApiError, type KnowledgeUploadReply, type MekoApi } from './mekoapi.js';
import { HttpError } from './projects.js';
import { dedicatedDatapack } from './store.js';

/** How often an upload's indexing is checked: every 5 s for its first minute, then every 30 s. */
const FAST_POLL_MS = 5000;
const FAST_FOR_MS = 60_000;
const SLOW_POLL_MS = 30_000;
/** When AgentK stops following an upload that's still indexing. */
const GIVE_UP_MS = 15 * 60_000;

export interface KnowledgeTiming {
  fastPollMs: number;
  fastForMs: number;
  slowPollMs: number;
  giveUpMs: number;
}

const TIMING: KnowledgeTiming = { fastPollMs: FAST_POLL_MS, fastForMs: FAST_FOR_MS, slowPollMs: SLOW_POLL_MS, giveUpMs: GIVE_UP_MS };

/** An upload Meko is still indexing, followed until it's done. Kept in memory only: a restart drops them. */
interface Followed {
  sessionId: string;
  datapackId: string;
  documentKey: string;
  filename: string;
  startedAt: number;
  timer: NodeJS.Timeout | null;
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
  // Never a 401 to the browser: that signs it out.
  if (e.status === 401 || /SCOPE/i.test(e.code)) return new HttpError(502, 'Meko refused AgentK\'s key. Check it on the Meko tab in Settings.');
  if (e.status === 403) {
    return new HttpError(403, `You can't add files to ${datapackName}: only its owner and maintainers can. Ask the owner for maintainer access.`);
  }
  // A 404 without Meko's envelope: the route itself isn't there (an older Meko).
  if (e.status === 404 && e.code === 'HTTP_404') return new HttpError(502, "This Meko doesn't take uploads from AgentK yet.");
  if (e.status === 400 || e.status === 413 || e.status === 415) return new HttpError(e.status, e.message);
  return new HttpError(502, e.message);
}

/**
 * Files added to a session's knowledge base (POST /api/sessions/:id/knowledge): checked, sent to
 * the project's dedicated datapack through Meko's upload route (the key stays on the server), put
 * in the session's Meko call log, and followed while Meko indexes them, with a `knowledge` event
 * when it's done.
 */
export class KnowledgeService {
  private followed = new Map<string, Followed>();
  private stopped = false;

  constructor(private core: Core, private api: MekoApi, private timing: KnowledgeTiming = TIMING) {}

  async upload(id: string, name: unknown, body: unknown): Promise<KnowledgeUpload> {
    const row = this.core.sessions.row(id);
    if (row.desired !== 'running') throw new HttpError(409, 'This session is archived.');
    if (!this.core.mekoStatus.configured) throw new HttpError(409, 'Meko is off. Turn it on on the Meko tab in Settings to add files to a knowledge base.');
    const dp = dedicatedDatapack(this.core.db, row.project_id);
    if (!dp) throw new HttpError(409, "This session's project has no datapack. Give it one on the project's home page.");
    const filename = knowledgeName(name);
    // A JSON body is parsed before it gets here: only raw bytes are a file.
    if (!Buffer.isBuffer(body)) throw new HttpError(415, "Send the file's bytes as application/octet-stream.");
    if (!body.length) throw new HttpError(400, `${filename} is empty.`);
    if (body.length > MAX_KB_UPLOAD_BYTES) throw new HttpError(413, `${filename} is over ${MAX_KB_UPLOAD_BYTES / 1024 / 1024} MB.`);

    const at = Date.now();
    let reply: KnowledgeUploadReply | null = null;
    let error: unknown = null;
    try {
      reply = await this.api.uploadKnowledge(dp.id, filename, body);
    } catch (e) {
      error = e;
    }
    this.core.mekoLog.record({
      tool: 'knowledge_base_upload', at, ms: Date.now() - at, ok: !error, attempts: 1,
      args: { datapack_id: dp.id, filename, size: body.length },
      text: reply ? JSON.stringify(reply) : null, error: error ? errMsg(error) : null, tag: null,
      session: { id: row.id, createdAt: row.created_at },
    });
    if (!reply) throw uploadError(error, dp.name);
    const status = reply.status === 'already_uploaded' || reply.status === 'already_processed' ? reply.status : 'processing';
    if (status === 'processing' && reply.documentKey) this.follow(row.id, dp.id, reply.documentKey, filename);
    return { filename, documentKey: reply.documentKey, datapackName: dp.name, status };
  }

  /** Stops following every upload (the server is stopping). */
  stop(): void {
    this.stopped = true;
    for (const f of this.followed.values()) if (f.timer) clearTimeout(f.timer);
    this.followed.clear();
  }

  private follow(sessionId: string, datapackId: string, documentKey: string, filename: string): void {
    if (this.stopped) return;
    const key = `${datapackId}:${documentKey}`;
    if (this.followed.has(key)) return;
    const f: Followed = { sessionId, datapackId, documentKey, filename, startedAt: Date.now(), timer: null };
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
    let status = '';
    try {
      status = await this.api.documentStatus(f.datapackId, f.documentKey);
    } catch (e) {
      // A 404 while Meko is still registering the document, or a passing failure: keep waiting.
      if (!(e instanceof MekoApiError && e.status === 404)) this.core.log.warn(`checking how ${f.filename} is indexing failed`, errMsg(e));
    }
    if (this.stopped || this.followed.get(key) !== f) return;
    const result = status === 'COMPLETED' ? 'indexed' : status === 'FAILED' ? 'failed' : Date.now() - f.startedAt >= this.timing.giveUpMs ? 'unknown' : null;
    if (!result) {
      this.schedule(key, f);
      return;
    }
    this.followed.delete(key);
    this.core.notifyKnowledge({ sessionId: f.sessionId, filename: f.filename, result });
  }
}
