import type { MekoGaps } from '../shared/types.js';
import { errMsg, type Logger } from './log.js';
import { mekoApiBase, type LocalMeko } from './mekolocal.js';

/**
 * Meko's Context Gaps: the searches Meko found nothing, or nothing relevant, for, which Meko keeps
 * per datapack as questions (its docs/api.md). Meko judges every search itself; AgentK only reads
 * the questions and marks them, on the Meko on this computer's API listener with the client token
 * (`GET`, `POST …/mark`, `DELETE …/asks` under `/api/v1/datapacks/:id/questions`; `meko`: null
 * while none runs). That covers a linked Meko Cloud's datapacks too: their searches pass through this
 * Meko, which keeps their questions. The project home's line
 * counts the open ones and links to them, a learn step answers them, and a learn session's searches while it seeds are
 * forgotten. Nothing here fails what calls it: a Meko that's down, or too old to keep questions
 * (404), is no list.
 */

/** An open question as a learn step is offered it. */
export interface MekoQuestion {
  key: string;
  query: string;
  /** The conversation the list was read for asked it. */
  own: boolean;
}

/** What a mark says: dismissed (with the memory that already said it), taught (what was saved, and its id when known), or open again. */
export type QuestionMark =
  | { state: 'dismissed'; memoryId?: string | null }
  | { state: 'taught'; text: string; memoryId?: string | null }
  | { state: null };

type Fetch = (url: string, init: { method?: string; headers: Record<string, string>; body?: string; signal: AbortSignal }) =>
  Promise<{ ok: boolean; status: number; json(): Promise<unknown> }>;

const TIMEOUT_MS = 5000;
/** The most pages of open questions read for a learn step (100 a page). */
const MAX_PAGES = 10;

function isRecord(v: unknown): v is Record<string, unknown> {
  return typeof v === 'object' && v !== null && !Array.isArray(v);
}

const count = (v: unknown) => (typeof v === 'number' && Number.isFinite(v) && v >= 0 ? v : 0);

export class MekoQuestions {
  constructor(private o: { meko: () => LocalMeko | null; log: Logger; fetch?: Fetch }) {}

  /** The open questions' counts for the project home's line (both 0 with none open), or null with no list. */
  async openCounts(datapackId: string): Promise<MekoGaps | null> {
    const data = await this.request('GET', datapackId, '', { state: 'open' });
    if (!isRecord(data) || !isRecord(data.open)) return null;
    return { datapackId, empty: count(data.open.empty), weak: count(data.open.weak) };
  }

  /**
   * The datapack's open questions, latest asked first, each with whether `conversationId` asked it;
   * null with no list. Reads up to MAX_PAGES pages.
   */
  async openQuestions(datapackId: string, conversationId: string | null): Promise<MekoQuestion[] | null> {
    const out: MekoQuestion[] = [];
    let after: { at: string; key: string } | null = null;
    for (let page = 0; page < MAX_PAGES; page++) {
      const data = await this.request('GET', datapackId, '', {
        state: 'open', ...(conversationId ? { conversation_id: conversationId } : {}), ...(after ? { after: after.at, after_key: after.key } : {}),
      });
      if (!isRecord(data) || !Array.isArray(data.questions)) return page ? out : null;
      for (const q of data.questions) {
        if (!isRecord(q) || typeof q.key !== 'string' || typeof q.query !== 'string') continue;
        out.push({ key: q.key, query: q.query, own: q.asked_by_conversation === true });
      }
      const last = data.questions.at(-1);
      // The next page starts after the last one shown, by its time and key, as Meko gave them.
      if (data.more !== true || !isRecord(last) || (typeof last.last_at !== 'string' && typeof last.last_at !== 'number') || typeof last.key !== 'string') break;
      after = { at: String(last.last_at), key: last.key };
    }
    return out;
  }

  /** Marks a question for an agent's learn step in `conversationId`; false when Meko didn't take it. */
  async mark(datapackId: string, key: string, mark: QuestionMark, conversationId: string | null): Promise<boolean> {
    const body = {
      key, state: mark.state,
      ...(mark.state === 'taught' ? { text: mark.text } : {}),
      ...(mark.state !== null && mark.memoryId ? { memory_id: mark.memoryId } : {}),
      ...(conversationId ? { conversation_id: conversationId } : {}),
    };
    return (await this.request('POST', datapackId, '/mark', {}, body)) !== undefined;
  }

  /** Forgets the asks one conversation made before `before` (a learn session's while it seeded); false when Meko didn't. */
  async forgetAsks(datapackId: string, conversationId: string, before: number): Promise<boolean> {
    const data = await this.request('DELETE', datapackId, '/asks', { conversation_id: conversationId, before: new Date(before).toISOString() });
    return data !== undefined;
  }

  /** Meko's `data` (null for a 204), or undefined when there's no Meko to ask or the call failed. */
  private async request(method: string, datapackId: string, path: string, query: Record<string, string>, body?: unknown): Promise<unknown> {
    const local = this.o.meko();
    const base = mekoApiBase(local);
    if (!local || !base) return undefined;
    const qs = new URLSearchParams(query).toString();
    const url = `${base}/datapacks/${encodeURIComponent(datapackId)}/questions${path}${qs ? `?${qs}` : ''}`;
    try {
      const res = await (this.o.fetch ?? fetch)(url, {
        method,
        headers: { Authorization: `Bearer ${local.token}`, ...(body === undefined ? {} : { 'Content-Type': 'application/json' }) },
        ...(body === undefined ? {} : { body: JSON.stringify(body) }),
        signal: AbortSignal.timeout(TIMEOUT_MS),
      });
      // 404: an unknown datapack or question, or a Meko too old to keep questions.
      if (!res.ok) {
        if (res.status !== 404) this.o.log.warn(`Meko's Context Gaps answered ${res.status} (${method} ${path || 'list'})`);
        return undefined;
      }
      if (res.status === 204) return null;
      const json = (await res.json().catch(() => null)) as { data?: unknown } | null;
      return json?.data ?? null;
    } catch (e) {
      this.o.log.warn(`Meko's Context Gaps didn't answer (${method} ${path || 'list'})`, errMsg(e));
      return undefined;
    }
  }
}
