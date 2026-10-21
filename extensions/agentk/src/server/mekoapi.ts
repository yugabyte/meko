import type { DatapackMember, DatapackPeople, DatapackPendingInvite, LinkGrant, ShareGrant } from '../shared/types.js';
import { SHARE_GRANTS } from '../shared/types.js';
import { strOrNull } from './meko.js';

/** Meko's answer to a knowledge-base upload. */
export interface KnowledgeUploadReply {
  documentKey: string;
  filename: string;
  /** `processing`, or `already_uploaded` / `already_processed` when Meko had the same bytes. */
  status: string;
}

/** A share invite waiting for us, from GET /datapacks/incoming-shares. */
export interface IncomingShare {
  inviteId: string;
  datapackId: string;
  datapackName: string;
  grant: ShareGrant | null;
  ownerName: string | null;
  ownerEmail: string | null;
}

/** A datapack's share link as Meko keeps it. The token is a secret: never log it. */
export interface ShareLinkRow {
  active: boolean;
  /** Null until the link is first turned on. */
  token: string | null;
  grant: LinkGrant;
  joined: number;
  atLinkRole: number;
}

/** A change to a share link, in Meko's words: one per call. */
export type MekoShareLinkChange = { status: 'active' | 'off' } | { grant: LinkGrant } | { reset: true };

export interface RedeemedShareLink {
  datapackId: string;
  grant: LinkGrant | null;
  alreadyMember: boolean;
}

export class MekoApiError extends Error {
  constructor(public status: number, public code: string, message: string) {
    super(message);
  }
}

/**
 * Meko answered without an error but not in the shape AgentK reads: a 502, since Meko's API
 * changed (`apiFailure` passes on only Meko's own refusals).
 */
const badReply = (what: string) => new MekoApiError(200, 'BAD_REPLY', `Meko's answer to ${what} wasn't in a shape AgentK understands.`);

/** Meko answers like this when it has no such route (an older or self-hosted Meko): 405, 501, or a 404 with no error code. */
export function routeMissing(e: unknown): boolean {
  return e instanceof MekoApiError && (e.status === 405 || e.status === 501 || (e.status === 404 && e.code === 'HTTP_404'));
}

type Json = Record<string, unknown>;
type Fetch = typeof fetch;

const TIMEOUT_MS = 30000;
/** A knowledge-base upload: up to 50 MB on the way out. */
const UPLOAD_TIMEOUT_MS = 120000;

export function asGrant(v: unknown): ShareGrant | null {
  const g = typeof v === 'string' ? v.trim().toLowerCase() : '';
  return (SHARE_GRANTS as string[]).includes(g) ? (g as ShareGrant) : null;
}

const str = (v: unknown): string => (typeof v === 'string' ? v : v == null ? '' : String(v));

function asLinkGrant(v: unknown): LinkGrant | null {
  const g = asGrant(v);
  return g === 'viewer' || g === 'contributor' ? g : null;
}

const count = (v: unknown): number => (typeof v === 'number' && Number.isFinite(v) && v > 0 ? Math.floor(v) : 0);

function toShareLink(r: Json | undefined): ShareLinkRow {
  const l = r?.share_link;
  if (!l || typeof l !== 'object' || Array.isArray(l)) throw badReply('the share link');
  const link = l as Json;
  // No role yet (a link never turned on) is Meko's default, viewer, as its web app reads it; a role
  // AgentK doesn't know is Meko's API changing.
  const grant = link.grant == null || link.grant === '' ? 'viewer' : asLinkGrant(link.grant);
  if (!grant) throw badReply('the share link');
  return {
    active: link.status === 'active',
    token: strOrNull(link.token),
    grant,
    joined: count(link.redemption_count),
    atLinkRole: count(link.members_at_link_role),
  };
}

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
 * Client for Meko's REST API (the share and invite routes, and knowledge-base uploads), with the same API key as MCP. Replies
 * come as `{ data }` or `{ error: { code, message } }`.
 */
export class MekoApi {
  constructor(private creds: () => { baseUrl: string | null; apiKey: string }, private fetchImpl: Fetch = fetch) {}

  /** A FormData `body` goes as multipart (fetch sets its boundary); anything else as JSON. */
  private async request<T>(method: string, path: string, body?: unknown, timeoutMs = TIMEOUT_MS): Promise<T> {
    const { baseUrl, apiKey } = this.creds();
    if (!baseUrl) throw new MekoApiError(0, 'NO_API_URL', "Meko's API URL isn't known. Set it on the Meko tab in Settings.");
    if (!apiKey) throw new MekoApiError(0, 'NO_KEY', 'No Meko API key is configured.');
    const form = body instanceof FormData;
    let res: Response;
    try {
      res = await this.fetchImpl(`${baseUrl}${path}`, {
        method,
        headers: {
          Authorization: `Bearer ${apiKey}`,
          Accept: 'application/json',
          ...(body !== undefined && !form ? { 'Content-Type': 'application/json' } : {}),
        },
        body: form ? body : body !== undefined ? JSON.stringify(body) : undefined,
        signal: AbortSignal.timeout(timeoutMs),
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

  /** The datapack's share link (403 unless we're its owner or a maintainer). */
  async shareLink(datapackId: string): Promise<ShareLinkRow> {
    return toShareLink(await this.request<Json>('GET', `/datapacks/${encodeURIComponent(datapackId)}/share-link`));
  }

  /** One change to the share link: Meko takes one per call. */
  async setShareLink(datapackId: string, change: MekoShareLinkChange): Promise<ShareLinkRow> {
    return toShareLink(await this.request<Json>('PUT', `/datapacks/${encodeURIComponent(datapackId)}/share-link`, change));
  }

  /** Joins the datapack a share link's token is for. */
  async redeemShareLink(token: string): Promise<RedeemedShareLink> {
    const r = await this.request<Json>('POST', `/share/${encodeURIComponent(token)}/redeem`);
    const datapackId = str(r?.datapack_id);
    if (!datapackId) throw badReply('join by link');
    return { datapackId, grant: asLinkGrant(r.grant), alreadyMember: r.already_member === true };
  }

  /**
   * Adds a file to a datapack's knowledge base (Meko's single-call upload: Meko stores it and starts
   * indexing). Meko takes the type from the extension and checks it against the bytes.
   */
  async uploadKnowledge(datapackId: string, filename: string, bytes: Uint8Array): Promise<KnowledgeUploadReply> {
    const form = new FormData();
    // A request body's bytes: never a SharedArrayBuffer, which Blob can't take.
    form.append('file', new Blob([bytes as Uint8Array<ArrayBuffer>]), filename);
    const r = await this.request<Json>('POST', `/datapacks/${encodeURIComponent(datapackId)}/knowledge-bases/upload`, form, UPLOAD_TIMEOUT_MS);
    return { documentKey: str(r?.document_key), filename: str(r?.filename) || filename, status: str(r?.status) };
  }

  /** How far Meko got indexing a document: its `document_status` (PENDING, PROCESSING, COMPLETED, FAILED…), upper-cased. */
  async documentStatus(datapackId: string, documentKey: string): Promise<string> {
    const r = await this.request<Json>('GET',
      `/datapacks/${encodeURIComponent(datapackId)}/knowledge-bases/document-status?document_key=${encodeURIComponent(documentKey)}`);
    return str(r?.document_status).toUpperCase();
  }
}
