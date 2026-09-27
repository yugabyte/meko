import type { DatapackMember, DatapackPeople, DatapackPendingInvite, ShareGrant, SharingMode } from '../shared/types.js';
import { SHARE_GRANTS } from '../shared/types.js';
import { strOrNull } from './meko.js';

/** A share invite waiting for us, from GET /datapacks/incoming-shares. */
export interface IncomingShare {
  inviteId: string;
  datapackId: string;
  datapackName: string;
  grant: ShareGrant | null;
  ownerName: string | null;
  ownerEmail: string | null;
}

export class MekoApiError extends Error {
  constructor(public status: number, public code: string, message: string) {
    super(message);
  }
}

type Json = Record<string, unknown>;
type Fetch = typeof fetch;

const TIMEOUT_MS = 30000;

/** Statuses that mean this Meko won't let our API key use its share routes (link mode). */
const NO_SHARING_FOR_KEYS = new Set([401, 403, 404, 405]);

export function asGrant(v: unknown): ShareGrant | null {
  const g = typeof v === 'string' ? v.trim().toLowerCase() : '';
  return (SHARE_GRANTS as string[]).includes(g) ? (g as ShareGrant) : null;
}

const str = (v: unknown): string => (typeof v === 'string' ? v : v == null ? '' : String(v));

/**
 * Whether a URL answers like Meko's REST API: its share route replies with Meko's JSON envelope
 * (a 401 without a key). Asked without the key, so a mistyped host never gets it.
 */
export async function looksLikeMekoApi(baseUrl: string, fetchImpl: Fetch = fetch): Promise<{ ok: true } | { ok: false; error: string }> {
  let res: Response;
  try {
    res = await fetchImpl(`${baseUrl.replace(/\/+$/, '')}/datapacks/incoming-shares`, {
      headers: { Accept: 'application/json' },
      signal: AbortSignal.timeout(10000),
    });
  } catch (e) {
    const why = (e as Error)?.name === 'TimeoutError' ? 'it timed out' : (e as Error)?.message || String(e);
    return { ok: false, error: `couldn't reach it (${why})` };
  }
  let body: unknown;
  try {
    body = JSON.parse(await res.text());
  } catch {
    body = null;
  }
  const env = body && typeof body === 'object' && !Array.isArray(body) ? (body as Json) : null;
  // Meko's envelope: `{ data }`, or `{ error: { code, message } }`. A bare `{ error: "…" }` is any API.
  const err = env?.error as Json | undefined;
  if (env && ('data' in env || (err && typeof err === 'object' && typeof err.code === 'string'))) return { ok: true };
  return { ok: false, error: `it answered ${res.status}, not like Meko's API` };
}

/**
 * Client for Meko's REST API (the share and invite routes), with the same API key as MCP. Replies
 * come as `{ data }` or `{ error: { code, message } }`.
 */
export class MekoApi {
  constructor(private creds: () => { baseUrl: string | null; apiKey: string }, private fetchImpl: Fetch = fetch) {}

  private async request<T>(method: string, path: string, body?: unknown): Promise<T> {
    const { baseUrl, apiKey } = this.creds();
    if (!baseUrl) throw new MekoApiError(0, 'NO_API_URL', "Meko's API URL isn't known. Set it on the Meko tab in Settings.");
    if (!apiKey) throw new MekoApiError(0, 'NO_KEY', 'No Meko API key is configured.');
    let res: Response;
    try {
      res = await this.fetchImpl(`${baseUrl}${path}`, {
        method,
        headers: {
          Authorization: `Bearer ${apiKey}`,
          Accept: 'application/json',
          ...(body !== undefined ? { 'Content-Type': 'application/json' } : {}),
        },
        body: body !== undefined ? JSON.stringify(body) : undefined,
        signal: AbortSignal.timeout(TIMEOUT_MS),
      });
    } catch (e) {
      const why = (e as Error)?.name === 'TimeoutError' ? 'timed out' : (e as Error)?.message || String(e);
      throw new MekoApiError(0, 'UNREACHABLE', `Couldn't reach Meko's API at ${baseUrl}: ${why}`);
    }
    const text = await res.text();
    let parsed: unknown = undefined;
    try {
      parsed = text ? JSON.parse(text) : undefined;
    } catch {
      /* not JSON */
    }
    const env = (parsed && typeof parsed === 'object' ? parsed : {}) as Json;
    const err = env.error;
    if (!res.ok || err) {
      const e = (err && typeof err === 'object' ? err : {}) as Json;
      const message = strOrNull(e.message) ?? (typeof err === 'string' ? err : null) ?? `Meko answered ${res.status}.`;
      throw new MekoApiError(res.status, str(e.code) || `HTTP_${res.status}`, message);
    }
    return ('data' in env ? env.data : parsed) as T;
  }

  /**
   * Whether this Meko takes our key on its share routes: `api` if it does, `link` if it refuses
   * it (sharing then happens in Meko's web app). Throws when Meko can't be reached at all.
   */
  async sharingMode(): Promise<SharingMode> {
    try {
      await this.request('GET', '/datapacks/incoming-shares');
      return 'api';
    } catch (e) {
      if (e instanceof MekoApiError && (NO_SHARING_FOR_KEYS.has(e.status) || e.code === 'NO_API_URL')) return 'link';
      throw e;
    }
  }

  async incomingShares(): Promise<IncomingShare[]> {
    const rows = await this.request<unknown>('GET', '/datapacks/incoming-shares');
    return (Array.isArray(rows) ? (rows as Json[]) : [])
      .map((r) => ({
        inviteId: str(r.invite_id),
        datapackId: str(r.datapack_id),
        datapackName: str(r.datapack_name),
        grant: asGrant(r.grant),
        ownerName: strOrNull(r.owner_name),
        ownerEmail: strOrNull(r.owner_email),
      }))
      .filter((r) => r.inviteId && r.datapackId && r.datapackName);
  }

  /**
   * Accepts or declines an invite. `selfUserId` is our Meko account id when known; Meko uses the
   * caller's account when it's left out.
   */
  async inviteAction(inviteId: string, action: 'accept' | 'decline', selfUserId: string | null): Promise<void> {
    await this.request('POST', '/datapacks/invite-action', {
      invite_id: inviteId,
      user_action: action,
      ...(selfUserId ? { recipient_user_id: selfUserId } : {}),
    });
  }

  async share(datapackId: string, email: string, grant: ShareGrant): Promise<void> {
    await this.request('POST', `/datapacks/${encodeURIComponent(datapackId)}/share`, { recipient_email: email, grant });
  }

  async resend(datapackId: string, inviteId: string): Promise<void> {
    await this.request('POST', `/datapacks/${encodeURIComponent(datapackId)}/invite/${encodeURIComponent(inviteId)}/resend`);
  }

  async revoke(datapackId: string, inviteId: string): Promise<void> {
    await this.request('DELETE', `/datapacks/${encodeURIComponent(datapackId)}/invite/${encodeURIComponent(inviteId)}`);
  }

  /** Members (the owner first) and pending invites. Pending invites are hidden from contributors and viewers. */
  async people(datapackId: string): Promise<DatapackPeople> {
    const dp = `/datapacks/${encodeURIComponent(datapackId)}`;
    const [m, p] = await Promise.all([
      this.request<Json>('GET', `${dp}/member-list`),
      this.request<Json>('GET', `${dp}/outgoing-shares`).catch((e) => {
        if (e instanceof MekoApiError && e.status === 403) return {} as Json;
        throw e;
      }),
    ]);
    const members: DatapackMember[] = (Array.isArray(m?.members) ? (m.members as Json[]) : [])
      .map((r) => ({
        name: str(r.user_name),
        email: str(r.user_email_id),
        grant: r.is_owner === true || str(r.grant) === 'owner' ? ('owner' as const) : (asGrant(r.grant) ?? 'viewer'),
      }))
      .sort((a, b) => Number(b.grant === 'owner') - Number(a.grant === 'owner') || a.email.localeCompare(b.email));
    const invites: DatapackPendingInvite[] = (Array.isArray(p?.pending_invites) ? (p.pending_invites as Json[]) : [])
      .map((r) => ({ inviteId: str(r.invite_id), email: str(r.recipient_email_id), grant: asGrant(r.grant) ?? 'viewer' }))
      .filter((r) => r.inviteId);
    return { members, invites };
  }
}
