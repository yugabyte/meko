import { createHash } from 'node:crypto';
import type { DatapackMember, DatapackPeople, DatapackPendingInvite, LinkGrant, ShareGrant } from '../shared/types.js';
import { SHARE_GRANTS } from '../shared/types.js';
import { strOrNull } from './meko.js';
import { originOf } from './mekolocal.js';

/**
 * Meko's answer to a request to add a file to a knowledge base: where to post it, or that the
 * datapack already has the same bytes. The URL and fields are a credential for this one file until
 * they expire (5 minutes): never log them.
 */
export interface PresignedUpload {
  /** The storage URL the file is posted to; empty when `alreadyUploaded`. */
  url: string;
  /** The signed form fields that go before the file, as Meko gave them; empty when `alreadyUploaded`. */
  fields: [string, string][];
  /** The document's key in the datapack: what `documentStatus` takes. */
  key: string;
  alreadyUploaded: boolean;
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
 * Meko answered without an error but not in the shape Kode reads: a 502, since Meko's API
 * changed (`apiFailure` passes on only Meko's own refusals).
 */
const badReply = (what: string) => new MekoApiError(200, 'BAD_REPLY', `Meko's answer to ${what} wasn't in a shape Kode understands.`);

/** Meko answers like this when it has no such route (an older or self-hosted Meko): 405, 501, or a 404 with no error code. */
export function routeMissing(e: unknown): boolean {
  return e instanceof MekoApiError && (e.status === 405 || e.status === 501 || (e.status === 404 && e.code === 'HTTP_404'));
}

type Json = Record<string, unknown>;
type Fetch = typeof fetch;

const TIMEOUT_MS = 30000;
/** A file posted to Meko's storage: up to 50 MB on the way out. */
const UPLOAD_TIMEOUT_MS = 120000;
/** Says who asked for an upload URL, in Meko's logs. */
const UPLOAD_CLIENT = 'agent-k-local';

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
  // Kode doesn't know is Meko's API changing.
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
 * Client for Meko's REST API (the share and invite routes, and knowledge-base uploads), with the same API key as MCP. Replies
 * come as `{ data }` or `{ error: { code, message } }`. The one call that isn't to Meko's API, posting a file to the storage
 * URL Meko gives, goes without the key.
 */
export class MekoApi {
  /** `baseUrl`: Meko's REST API (`resolveMekoApiUrl`), null with no Meko. */
  constructor(private creds: () => { baseUrl: string | null; apiKey: string }, private fetchImpl: Fetch = fetch) {}

  /** A `body` goes as JSON. */
  private async request<T>(method: string, path: string, body?: unknown): Promise<T> {
    const { baseUrl, apiKey } = this.creds();
    if (!baseUrl || !apiKey) throw new MekoApiError(0, 'NO_MEKO', "Meko isn't running on this computer. Start it with meko start.");
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
   * Asks Meko where to put a file for a datapack's knowledge base (its presigned flow): Meko signs an
   * S3 POST for exactly these bytes (their size and SHA-256), and indexes the file once it lands, with
   * no call back. Meko takes the type from the extension and checks it against the bytes.
   */
  async requestUpload(datapackId: string, filename: string, bytes: Uint8Array): Promise<PresignedUpload> {
    const r = await this.request<Json>('POST', `/datapacks/${encodeURIComponent(datapackId)}/knowledge-bases/upload-url`, {
      filename,
      size: bytes.length,
      content_hash: createHash('sha256').update(bytes).digest('hex'),
      client: UPLOAD_CLIENT,
    });
    const key = str(r?.key);
    if (r?.already_uploaded === true && key) return { url: '', fields: [], key, alreadyUploaded: true };
    const url = str(r?.url);
    const f = r?.fields;
    if (!key || !url || !f || typeof f !== 'object' || Array.isArray(f)) throw badReply('the upload request');
    return { url, fields: Object.entries(f as Json).map(([k, v]) => [k, str(v)]), key, alreadyUploaded: false };
  }

  /**
   * Posts a file where `requestUpload` said: the signed fields, then the file last (S3 ignores
   * fields after it), without Meko's key. Only over https, or to the API's own origin (the Meko on
   * this computer taking a file for one of its own datapacks): that Meko is on http, but the storage
   * it hands out for Meko Cloud's datapacks isn't. Errors name the storage's host, never the URL or
   * fields.
   */
  async postPresigned(upload: PresignedUpload, filename: string, bytes: Uint8Array): Promise<void> {
    let target: URL;
    try {
      target = new URL(upload.url);
    } catch {
      throw badReply('the upload request');
    }
    const { baseUrl } = this.creds();
    const ownOrigin = !!baseUrl && originOf(baseUrl) === target.origin;
    if (target.protocol !== 'https:' && !(ownOrigin && target.protocol === 'http:')) {
      throw new MekoApiError(200, 'BAD_REPLY', `Meko gave an upload address Kode won't send a file to (${target.protocol}).`);
    }
    const form = new FormData();
    for (const [k, v] of upload.fields) form.append(k, v);
    // A request body's bytes: never a SharedArrayBuffer, which Blob can't take.
    form.append('file', new Blob([bytes as Uint8Array<ArrayBuffer>]), filename);
    let res: Response;
    try {
      res = await this.fetchImpl(upload.url, { method: 'POST', body: form, signal: AbortSignal.timeout(UPLOAD_TIMEOUT_MS) });
    } catch (e) {
      const why = (e as Error)?.name === 'TimeoutError' ? 'timed out' : (e as Error)?.message || String(e);
      throw new MekoApiError(0, 'UNREACHABLE', `Couldn't reach Meko's file storage at ${target.host}: ${why}`);
    }
    const text = await res.text();
    if (res.ok) return;
    // S3's refusal: `<Error><Code>…</Code><Message>…</Message></Error>`.
    const tag = (name: string) => new RegExp(`<${name}>([^<]*)</${name}>`).exec(text)?.[1]?.trim() || null;
    throw new MekoApiError(res.status, `STORAGE_${tag('Code') ?? `HTTP_${res.status}`}`, tag('Message') ?? `Meko's file storage answered ${res.status}.`);
  }

  /**
   * How far Meko got with a document, lower-cased: its `state` (awaiting_upload, abandoned, oversized,
   * indexing, completed, failed), or from a Meko without it, its indexer's `document_status`.
   */
  async documentStatus(datapackId: string, documentKey: string): Promise<string> {
    const r = await this.request<Json>('GET',
      `/datapacks/${encodeURIComponent(datapackId)}/knowledge-bases/document-status?document_key=${encodeURIComponent(documentKey)}`);
    return (str(r?.state) || str(r?.document_status)).toLowerCase();
  }
}
