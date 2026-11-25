import type { DatapackInvite, ShareGrant } from '../shared/types.js';
import { isProjectDatapack, remoteFromDescription } from '../shared/datapacks.js';
import type { Core } from './core.js';
import { all, get, kvDelete, kvGet, kvSet, run, tx } from './db.js';
import { errMsg } from './log.js';
import type { MekoDatapackRow } from './meko.js';
import { asGrant, MekoApiError, type MekoApi } from './mekoapi.js';
import { HttpError } from './projects.js';

type InviteState = 'invited' | 'shared' | 'later' | 'used' | 'baseline';

interface InviteRow {
  datapack_id: string;
  name: string;
  invite_id: string | null;
  grant_role: string | null;
  owner_name: string | null;
  owner_email: string | null;
  repo_remote: string | null;
  state: InviteState;
  first_seen_at: number;
  resolved_at: number | null;
}

/** Set once the first sync has recorded the shares that already existed, so they don't all pop up. */
const BASELINE_KEY = 'invites.baselined';

/** How long a share accepted here may be missing from datapack_list (Meko catching up) before it counts as gone. */
const ACCEPT_GRACE_MS = 10 * 60 * 1000;

function toInvite(r: InviteRow): DatapackInvite {
  return {
    datapackId: r.datapack_id,
    name: r.name,
    inviteId: r.invite_id,
    grant: asGrant(r.grant_role),
    ownerName: r.owner_name,
    ownerEmail: r.owner_email,
    repoRemote: r.repo_remote,
    state: r.state as DatapackInvite['state'],
    firstSeenAt: r.first_seen_at,
  };
}

/**
 * Project datapacks (named `adlc/proj/…`) shared with us: invites waiting in Meko
 * and shares already accepted that aren't a project here yet. Synced from Meko periodically.
 */
export class InviteService {
  /** Our Meko account id, learned from the datapacks we own. */
  private selfUserId: string | null = null;
  /**
   * Bumped by every change made here (accept, decline, later, used, a new Meko account): a sync whose
   * Meko data was read before one is thrown away and read again, so it can't undo the change.
   */
  private gen = 0;
  /** The sync running now; a sync asked for meanwhile waits for it instead of starting another. */
  private running: Promise<void> | null = null;
  /** Datapacks whose invite was accepted here, and when: not taken for gone while datapack_list catches up. */
  private acceptedAt = new Map<string, number>();
  /**
   * The last failure to list the invites waiting in Meko, logged once: a key the
   * share routes refuse fails every sync until the key or URL changes. Cleared by a success.
   */
  private incomingError: string | null = null;

  constructor(private core: Core, readonly api: MekoApi) {}

  private get db() {
    return this.core.db;
  }

  /**
   * Refuses when there's no Meko to share through. Whether that Meko shares (one not linked to Meko
   * Cloud doesn't) is Meko's to say: its refusal is the error (`apiFailure`). `what` finishes
   * "Kode can't …".
   */
  requireSharing(what: string): void {
    if (!this.core.mekoStatus.configured) throw new HttpError(409, `Meko isn't running on this computer, so Kode can't ${what}. Start it with meko start.`);
  }

  /**
   * A Meko API failure as the HTTP error to answer with. Meko's own refusals (a bad email, an invite
   * that's already there, no permission) keep their status; a refused key and anything else (Meko
   * down, a server error) is a 502.
   */
  apiFailure(e: unknown, prefix: string): Error {
    if (!(e instanceof MekoApiError)) return e instanceof Error ? e : new Error(String(e));
    if (e.status === 401) return new HttpError(502, `${prefix}: Kode's token wasn't accepted (${e.message}). Run meko restart, then try again.`);
    const status = e.status === 422 ? 400 : [400, 403, 404, 409].includes(e.status) ? e.status : 502;
    return new HttpError(status, `${prefix}: ${e.message}`);
  }

  /** The Meko server or key changed, so maybe the account: forget what came from the old one. */
  forgetAccount(): void {
    this.selfUserId = null;
    this.acceptedAt.clear();
    this.gen++;
    // Datapacks used by a project here stay done with; the rest is read again, and the first sync
    // on the new account records its existing shares quietly, as after an upgrade.
    run(this.db, `DELETE FROM datapack_invites WHERE state != 'used'`);
    kvDelete(this.db, BASELINE_KEY);
    this.core.changed();
  }

  /** Invites and shares to show as banners. */
  pending(): DatapackInvite[] {
    return all<InviteRow>(this.db, `SELECT * FROM datapack_invites WHERE state IN ('invited', 'shared') ORDER BY first_seen_at, name`).map(toInvite);
  }

  /** Every open one, the ones put off with Later included. */
  open(): DatapackInvite[] {
    return all<InviteRow>(this.db, `SELECT * FROM datapack_invites WHERE state IN ('invited', 'shared', 'later') ORDER BY first_seen_at, name`).map(toInvite);
  }

  /** Our access to a datapack shared with us, as far as the shares table knows; null when it doesn't. */
  grantOf(datapackId: string): ShareGrant | null {
    return asGrant(this.row(datapackId)?.grant_role);
  }

  private row(datapackId: string): InviteRow | undefined {
    return get<InviteRow>(this.db, 'SELECT * FROM datapack_invites WHERE datapack_id = ?', datapackId);
  }

  /** Datapacks some live project here already uses. */
  private usedIds(): Set<string> {
    return new Set(all<{ datapack_id: string }>(this.db,
      `SELECT DISTINCT d.datapack_id FROM project_datapacks d JOIN projects p ON p.id = d.project_id WHERE p.deleted_at IS NULL`,
    ).map((r) => r.datapack_id));
  }

  /** Take before reading datapack_list, and pass back with it to sync(): tells whether the list is stale. */
  stamp(): number {
    return this.gen;
  }

  /**
   * Brings the table in line with Meko. `listed` is a datapack_list result the caller already has,
   * with the stamp taken before it was read; without one (or when something changed here since the
   * stamp) it's fetched. Sends one `invite` event per datapack newly shared with us. Only one runs
   * at a time: a call while one is running waits for that one.
   */
  sync(listed?: { rows: MekoDatapackRow[]; stamp: number }): Promise<void> {
    if (this.running) return this.running;
    const task = (async () => {
      // Meko is read, then the table written in one go. When a change here lands in between, the
      // read is stale: read again (once), rather than write over the change.
      for (let attempt = 0; attempt < 2; attempt++) {
        if (await this.syncOnce(attempt === 0 ? listed : undefined)) return;
      }
      this.core.log.info('datapack invite sync skipped: shares changed here while it read Meko');
    })().finally(() => {
      if (this.running === task) this.running = null;
    });
    this.running = task;
    return task;
  }

  /** One read of Meko and one write of the table; false when a change here made the read stale. */
  private async syncOnce(listed?: { rows: MekoDatapackRow[]; stamp: number }): Promise<boolean> {
    if (!this.core.mekoStatus.configured) return true;
    const gen = this.gen;
    const list = listed && listed.stamp === gen ? listed.rows : await this.core.meko.listDatapacks();
    const self = list.find((d) => d.grant === 'owner' && d.ownerUserId)?.ownerUserId ?? null;
    const incoming = await this.api.incomingShares().catch((e: unknown) => {
      // A Meko that doesn't share (one not linked to Meko Cloud answers 409) has nothing to list.
      if (e instanceof MekoApiError && e.status === 409) return null;
      const why = errMsg(e);
      if (why !== this.incomingError) this.core.log.warn("couldn't list the datapack invites waiting in Meko (logged once until it changes)", why);
      this.incomingError = why;
      return null;
    });
    if (incoming) this.incomingError = null;
    if (gen !== this.gen) return false;
    // Only now: a read from before a change of account must not bring back the old account's id.
    this.selfUserId = self ?? this.selfUserId;
    const used = this.usedIds();
    const baseline = kvGet(this.db, BASELINE_KEY) === null;
    const now = Date.now();
    const notices: { datapackId: string; title: string; body: string }[] = [];
    let changed = false;
    const write = (sql: string, ...params: (string | number | null)[]) => {
      if (run(this.db, sql, ...params).changes) changed = true;
    };

    tx(this.db, () => {
      const accepted = list.filter((d) => d.grant && d.grant !== 'owner' && isProjectDatapack(d.name));
      const acceptedIds = new Set(accepted.map((d) => d.id));

      for (const s of incoming ?? []) {
        if (!isProjectDatapack(s.datapackName) || used.has(s.datapackId) || acceptedIds.has(s.datapackId)) continue;
        const r = this.row(s.datapackId);
        if (!r) {
          write(`INSERT INTO datapack_invites(datapack_id, name, invite_id, grant_role, owner_name, owner_email, state, first_seen_at)
                 VALUES (?, ?, ?, ?, ?, ?, 'invited', ?)`, s.datapackId, s.datapackName, s.inviteId, s.grant, s.ownerName, s.ownerEmail, now);
          notices.push(notice(s.datapackId, s.datapackName, s.ownerName ?? s.ownerEmail, s.grant, true));
          continue;
        }
        // Invited again after it was used or had gone: a new invite, told about once more.
        const again = r.state === 'used' || r.state === 'baseline';
        const state: InviteState = r.state === 'later' ? 'later' : 'invited';
        write(`UPDATE datapack_invites SET name = ?, invite_id = ?, grant_role = ?, owner_name = ?, owner_email = ?,
                 state = ?, resolved_at = NULL WHERE datapack_id = ? AND (name IS NOT ? OR invite_id IS NOT ? OR grant_role IS NOT ?
                 OR owner_name IS NOT ? OR owner_email IS NOT ? OR state IS NOT ?)`,
          s.datapackName, s.inviteId, s.grant, s.ownerName, s.ownerEmail, state,
          s.datapackId, s.datapackName, s.inviteId, s.grant, s.ownerName, s.ownerEmail, state);
        if (again) notices.push(notice(s.datapackId, s.datapackName, s.ownerName ?? s.ownerEmail, s.grant, true));
      }

      if (incoming) {
        // Invites no longer waiting in Meko and not accepted either: revoked, or declined elsewhere.
        const waiting = new Set(incoming.map((s) => s.inviteId));
        for (const r of all<InviteRow>(this.db, `SELECT * FROM datapack_invites WHERE invite_id IS NOT NULL AND state IN ('invited', 'later')`)) {
          if (!waiting.has(r.invite_id!) && !acceptedIds.has(r.datapack_id)) write('DELETE FROM datapack_invites WHERE datapack_id = ?', r.datapack_id);
        }
      }

      for (const d of accepted) {
        if (used.has(d.id)) continue;
        const remote = remoteFromDescription(d.description);
        const grant = asGrant(d.grant);
        const r = this.row(d.id);
        if (!r) {
          // Joined here through a share link: they know, so no banner or notification, but it's
          // offered in Open project (as after Later), even on the first sync.
          const joinedHere = this.acceptedAt.has(d.id);
          const state: InviteState = joinedHere ? 'later' : baseline ? 'baseline' : 'shared';
          write(`INSERT INTO datapack_invites(datapack_id, name, grant_role, owner_name, owner_email, repo_remote, state, first_seen_at)
                 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`, d.id, d.name, grant, d.ownerName, d.ownerEmail, remote, state, now);
          if (!baseline && !joinedHere) notices.push(notice(d.id, d.name, d.ownerName ?? d.ownerEmail, grant, false));
          continue;
        }
        // Accepted (here or in Meko): no invite left to act on. An invite already told about isn't told again.
        // An invite joined here through a share link instead: no banner for it either.
        const state = r.state === 'invited' ? (this.acceptedAt.has(d.id) ? 'later' : 'shared') : r.state;
        write(`UPDATE datapack_invites SET name = ?, invite_id = NULL, grant_role = ?, owner_name = ?, owner_email = ?, repo_remote = ?, state = ?
               WHERE datapack_id = ? AND (name IS NOT ? OR invite_id IS NOT NULL OR grant_role IS NOT ? OR owner_name IS NOT ?
               OR owner_email IS NOT ? OR repo_remote IS NOT ? OR state IS NOT ?)`,
          d.name, grant, d.ownerName, d.ownerEmail, remote, state, d.id, d.name, grant, d.ownerName, d.ownerEmail, remote, state);
      }

      // Shares that are gone from Meko (access removed, or the datapack deleted).
      for (const r of all<InviteRow>(this.db, `SELECT * FROM datapack_invites WHERE invite_id IS NULL AND state IN ('shared', 'later', 'baseline')`)) {
        if (acceptedIds.has(r.datapack_id)) {
          this.acceptedAt.delete(r.datapack_id);
          continue;
        }
        if (now - (this.acceptedAt.get(r.datapack_id) ?? 0) < ACCEPT_GRACE_MS) continue;
        write('DELETE FROM datapack_invites WHERE datapack_id = ?', r.datapack_id);
      }

      // Anything a project here now uses is done with.
      for (const id of used) {
        write(`UPDATE datapack_invites SET state = 'used', invite_id = NULL, resolved_at = ? WHERE datapack_id = ? AND state != 'used'`, now, id);
      }

      if (baseline) kvSet(this.db, BASELINE_KEY, String(now));
    });

    for (const n of notices) this.core.notifyInvite(n);
    if (changed) this.core.changed();
    return true;
  }

  private openRow(datapackId: string): InviteRow {
    const r = this.row(datapackId);
    if (!r || !['invited', 'shared', 'later'].includes(r.state)) throw new HttpError(404, 'That share is no longer waiting. Refresh and try again.');
    return r;
  }

  private async act(inviteId: string, action: 'accept' | 'decline'): Promise<void> {
    this.requireSharing(`${action} the invite`);
    try {
      await this.api.inviteAction(inviteId, action, this.selfUserId);
    } catch (e) {
      throw this.apiFailure(e, `Meko couldn't ${action} the invite`);
    }
  }

  /**
   * Accepts an invite in Meko before a project is created with its datapack. Throws (and nothing
   * changes) when Meko refuses. An invite already accepted (by an earlier try whose project then
   * failed, or in Meko) is left alone, so trying again works.
   */
  async acceptForProject(datapackId: string, inviteId: string): Promise<void> {
    const r = this.row(datapackId);
    if (r && !r.invite_id) return;
    if (r && r.invite_id !== inviteId) throw new HttpError(409, 'That invite has changed. Refresh and try again.');
    await this.act(inviteId, 'accept');
    this.gen++;
    this.acceptedAt.set(datapackId, Date.now());
    run(this.db, `UPDATE datapack_invites SET invite_id = NULL, state = 'shared' WHERE datapack_id = ?`, datapackId);
    // Shown now: if creating the project fails after this, the banner mustn't still offer Decline.
    this.core.changed();
  }

  /**
   * We joined the datapack through a share link here: the next sync offers it in Open project
   * without a banner or notification, and doesn't take it for gone while datapack_list catches up.
   * A sync that read Meko before this reads it again.
   */
  joinedHere(datapackId: string): void {
    this.gen++;
    this.acceptedAt.set(datapackId, Date.now());
  }

  /** The datapack's share or invite, while it's still open (not yet a project here, not declined). */
  openShare(datapackId: string): DatapackInvite | null {
    const r = this.row(datapackId);
    return r && ['invited', 'shared', 'later'].includes(r.state) ? toInvite(r) : null;
  }

  /** A project here now uses the datapack. */
  markUsed(datapackId: string): void {
    this.gen++;
    if (run(this.db, `UPDATE datapack_invites SET state = 'used', invite_id = NULL, resolved_at = ? WHERE datapack_id = ? AND state != 'used'`,
      Date.now(), datapackId).changes) this.core.changed();
  }

  /** Accepts an invite without making a project of it (viewer invites, to attach later). */
  async accept(datapackId: string): Promise<void> {
    const r = this.openRow(datapackId);
    if (!r.invite_id) throw new HttpError(409, 'That share is already accepted.');
    await this.act(r.invite_id, 'accept');
    this.gen++;
    this.acceptedAt.set(datapackId, Date.now());
    run(this.db, `UPDATE datapack_invites SET invite_id = NULL, state = 'later' WHERE datapack_id = ?`, datapackId);
    this.core.changed();
  }

  async decline(datapackId: string): Promise<void> {
    const r = this.openRow(datapackId);
    if (!r.invite_id) throw new HttpError(409, 'That share is already accepted. Leave it in Meko instead.');
    await this.act(r.invite_id, 'decline');
    this.gen++;
    run(this.db, 'DELETE FROM datapack_invites WHERE datapack_id = ?', datapackId);
    this.core.changed();
  }

  /** Hides it here; an invite stays waiting in Meko. */
  later(datapackId: string): void {
    this.openRow(datapackId);
    this.gen++;
    run(this.db, `UPDATE datapack_invites SET state = 'later' WHERE datapack_id = ?`, datapackId);
    this.core.changed();
  }
}

function notice(datapackId: string, name: string, from: string | null, grant: ShareGrant | null, invite: boolean) {
  const who = from ?? 'Someone';
  const as = grant ? ` as ${grant}` : '';
  return {
    datapackId,
    title: invite ? 'Project invite' : 'Project shared with you',
    body: invite ? `${who} invited you to ${name}${as}.` : `${who} shared ${name} with you${as}.`,
  };
}
