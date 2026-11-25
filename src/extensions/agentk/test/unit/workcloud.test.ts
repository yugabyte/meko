import { createHash } from 'node:crypto';
import { describe, expect, it } from 'vitest';
import type { PGlite } from '@electric-sql/pglite';
import { WORK_QUEUE_DATAPACK } from '../../src/shared/types.js';
import { kvGet, openDb } from '../../src/server/db.js';
import { checkCidr, endpointOf, readRecord, WorkCloud, WORK_CLUSTER, type WorkCloudDeps, type WorkLink, type WorkMeko } from '../../src/server/workcloud.js';
import { WorkQueue } from '../../src/server/workqueue.js';
import type { WorkConn } from '../../src/server/workstore.js';
import { freshPglite, pgliteSql } from '../pglite.js';

type Call = { tool: string; args: Record<string, unknown> };

/**
 * Meko Cloud as the work queue's set-up sees it: datapacks with descriptions and files, and AMP's clusters
 * and endpoints, in replies shaped as AMP's tools give them. An endpoint gets its address on the second ask.
 */
function fakeCloud(o: { used?: number; max?: number; refuseName?: boolean } = {}) {
  const datapacks = new Map<string, { id: string; description: string | null; files: Map<string, string> }>();
  const clusters: { id: string; name: string; datapack: string; password: string }[] = [];
  const computes: { id: string; cluster: string; allowed: string[]; asked: number; password: string }[] = [];
  const calls: Call[] = [];
  let n = 0;
  const budget = { used: o.used ?? 0, max: o.max ?? 2 };
  const meko: WorkMeko = {
    async call(tool, args) {
      calls.push({ tool, args });
      const host = (c: { id: string }) => `${c.id}.amp.example.com`;
      switch (tool) {
        case 'amp_core_budget':
          return { tier: 'free', next_tier: 'standard', used_cores: budget.used, max_cores: budget.max, at_limit: budget.used >= budget.max };
        case 'amp_cluster_list':
          return clusters.filter((c) => c.datapack === args.datapack_id).map((c) => ({ id: c.id, name: c.name }));
        case 'amp_cluster_create': {
          const c = { id: `cl-${++n}`, name: String(args.name), datapack: String(args.datapack_id), password: String(args.password) };
          clusters.push(c);
          computes.push({ id: `ep-${++n}`, cluster: c.id, allowed: args.allowed_cidrs as string[], asked: 0, password: c.password });
          budget.used += 0.5;
          return { cluster: { cluster_id: c.id, name: c.name } };
        }
        case 'amp_branch_list':
          return { branches: [{ branch_id: `br-${args.cluster_id}`, name: 'main', is_default: true }] };
        case 'amp_compute_list':
          return computes.filter((c) => c.cluster === args.cluster_id).map((c) => ({ compute_id: c.id, role: 'read_write' }));
        case 'amp_compute_get': {
          const c = computes.find((x) => x.id === args.compute_id);
          if (!c) throw new Error('compute not found');
          return ++c.asked > 1 ? { compute_id: c.id, state: 'running', host: host(c), port: 5432 } : { compute_id: c.id, state: 'provisioning' };
        }
        case 'amp_compute_delete':
          computes.splice(computes.findIndex((c) => c.id === args.compute_id), 1);
          budget.used -= 0.5;
          return { ok: true };
        case 'amp_compute_create': {
          // A second writer on a root branch keeps the cluster's first password, whatever it's given.
          const cl = clusters.find((c) => c.id === args.cluster_id)!;
          computes.push({ id: `ep-${++n}`, cluster: cl.id, allowed: args.allowed_cidrs as string[], asked: 0, password: cl.password });
          budget.used += 0.5;
          return { compute_id: `ep-${n}` };
        }
      }
      throw new Error(`no tool ${tool}`);
    },
    async findDatapack(name) {
      const d = datapacks.get(name);
      return d ? { id: d.id, description: d.description } : null;
    },
    async createDatapack(name) {
      calls.push({ tool: 'datapack_create', args: { name } });
      if (o.refuseName) throw new Error('A datapack with that name already exists');
      const d = { id: `dp-${++n}`, description: null, files: new Map<string, string>() };
      datapacks.set(name, d);
      return d.id;
    },
    async setDescription(id, description) {
      for (const d of datapacks.values()) if (d.id === id) d.description = description;
    },
    async putFile(id, _name, content) {
      const hash = createHash('sha256').update(content).digest('hex');
      for (const d of datapacks.values()) if (d.id === id) d.files.set(hash, content);
      return hash;
    },
    async getFile(id, hash) {
      for (const d of datapacks.values()) if (d.id === id) return d.files.get(hash) ?? null;
      return null;
    },
  };
  return { meko, calls, clusters, computes, datapacks, budget, tools: () => calls.map((c) => c.tool) };
}

/** One of the user's computers: its own state.db, a queue and its WorkCloud, on the shared fake Meko Cloud and database. */
function computer(cloud: ReturnType<typeof fakeCloud>, pg: PGlite, o: { link?: WorkLink; db?: ReturnType<typeof openDb>; waitMs?: number } = {}) {
  const db = o.db ?? openDb(':memory:');
  const conns: WorkConn[] = [];
  const link = { value: o.link ?? { state: 'linked', account: 'acme' } as WorkLink };
  const queue: WorkQueue = new WorkQueue({
    db, changed: () => undefined, warn: () => undefined, reviewRequests: async () => [], readSlack: async () => [], claudeAvailable: () => true,
    githubTitle: async () => 'x', settings: () => ({ github: false, slack: false, syncMinutes: 0 }),
    notReady: () => wc.notReady(), unreachable: () => void wc.recheck(),
  });
  // waitMs 0 gives up at the first ask for the endpoint's address; the tests set it as they need.
  const deps: WorkCloudDeps = {
    db, meko: cloud.meko, link: () => link.value, queue, changed: () => undefined, warn: () => undefined,
    connect: (c) => (conns.push(c), pgliteSql(pg)), sleep: async () => undefined, waitMs: o.waitMs ?? 60_000,
  };
  const wc: WorkCloud = new WorkCloud(deps);
  return { db, queue, wc, conns, link, deps };
}

describe('setting the work queue up in Meko Cloud', () => {
  it('sets up once, with a password Kode makes and keeps out of the state, and a second computer joins with its own address', async () => {
    const cloud = fakeCloud();
    const pg = await freshPglite();
    const a = computer(cloud, pg);
    await a.wc.lookup();
    expect(a.wc.state()).toMatchObject({ status: 'not-set-up', datapack: WORK_QUEUE_DATAPACK });
    a.wc.startSetUp('203.0.113.4', 'Laptop');
    await a.wc.settled();
    expect(a.wc.state()).toMatchObject({ status: 'ready', error: null, addresses: [{ cidr: '203.0.113.4/32', label: 'Laptop' }] });
    const made = cloud.calls.find((c) => c.tool === 'amp_cluster_create')!.args;
    expect(made).toMatchObject({ name: WORK_CLUSTER, with_writer: true, connectivity_mode: 'ingress_external', allowed_cidrs: ['203.0.113.4/32'] });
    const password = String(made.password);
    expect(password.length).toBeGreaterThanOrEqual(24);
    expect(JSON.stringify(a.wc.state())).not.toContain(password);
    expect(a.conns[0]).toMatchObject({ host: `${cloud.computes[0].id}.amp.example.com`, port: 5432, user: 'cloud_admin', password });
    await a.queue.create({ title: 'From A' });

    // The record's hash is in the datapack's description, where another computer finds it.
    const b = computer(cloud, pg);
    await b.wc.lookup();
    expect(b.wc.state().status).toBe('can-join');
    b.wc.startSetUp('198.51.100.0/24', 'Desktop');
    await b.wc.settled();
    expect(b.wc.state()).toMatchObject({ status: 'ready', addresses: [{ cidr: '203.0.113.4/32' }, { cidr: '198.51.100.0/24', label: 'Desktop' }] });
    expect(b.queue.list().map((i) => i.title)).toEqual(['From A']);
    expect(cloud.tools().filter((t) => t === 'amp_cluster_create').length).toBe(1);
    // AMP can't change an endpoint's list: the writer was replaced, with the same password.
    expect(cloud.computes).toEqual([expect.objectContaining({ allowed: ['203.0.113.4/32', '198.51.100.0/24'], password })]);
    expect(b.conns[0]).toMatchObject({ host: `${cloud.computes[0].id}.amp.example.com`, password });

    // A, still on the old endpoint, reads the record again once a query fails, and follows.
    await a.wc.recheck();
    expect(a.conns.at(-1)).toMatchObject({ host: `${cloud.computes[0].id}.amp.example.com` });
    expect(JSON.parse(kvGet(a.db, 'work-amp')!).allowed).toHaveLength(2);

    // Settings' Let it in and Remove replace the endpoint again, from the latest record.
    a.wc.startAddAddress('192.0.2.1', 'Phone');
    await a.wc.settled();
    expect(cloud.computes.map((c) => c.allowed)).toEqual([['203.0.113.4/32', '198.51.100.0/24', '192.0.2.1/32']]);
    expect(a.wc.state()).toMatchObject({ status: 'ready', error: null });
    expect(() => a.wc.startAddAddress('192.0.2.1', '')).toThrow(expect.objectContaining({ status: 409 }));
    expect(() => a.wc.startRemoveAddress('192.0.2.9')).toThrow(expect.objectContaining({ status: 404 }));
    for (const cidr of ['192.0.2.1/32', '198.51.100.0/24']) {
      a.wc.startRemoveAddress(cidr);
      await a.wc.settled();
    }
    expect(cloud.computes.map((c) => c.allowed)).toEqual([['203.0.113.4/32']]);
    expect(() => a.wc.startRemoveAddress('203.0.113.4')).toThrow(expect.objectContaining({ status: 409, message: expect.stringMatching(/at least one/) }));
  });

  it("refuses, in words, when the plan has no room for the database, and makes nothing", async () => {
    const cloud = fakeCloud({ used: 0.5, max: 0.5 });
    const a = computer(cloud, await freshPglite());
    await a.wc.lookup();
    a.wc.startSetUp('203.0.113.4', '');
    await a.wc.settled();
    expect(a.wc.state()).toMatchObject({ status: 'not-set-up', error: expect.stringMatching(/free plan allows 0.5 vCPU.*needs 0.5.*standard plan/) });
    expect(cloud.tools()).not.toContain('amp_cluster_create');
    expect(cloud.tools()).not.toContain('datapack_create');
  });

  it("says so when Meko Cloud won't make the datapack, whose name another account may hold", async () => {
    const a = computer(fakeCloud({ refuseName: true }), await freshPglite());
    await a.wc.lookup();
    a.wc.startSetUp('203.0.113.4', '');
    await a.wc.settled();
    expect(a.wc.state().error).toMatch(new RegExp(`didn't make the datapack ${WORK_QUEUE_DATAPACK}.*another account`));
  });

  it('picks a set-up a restart or a second press cut short up where it stopped, with the password it made, and makes nothing twice', async () => {
    const cloud = fakeCloud({ max: 0.5 });
    const pg = await freshPglite();
    const db = openDb(':memory:');
    const first = computer(cloud, pg, { db, waitMs: 0 });
    await first.wc.lookup();
    // The endpoint never gets its address this time: the set-up gives up after making the database.
    const get = cloud.meko.call;
    cloud.meko.call = async (tool, args) => (tool === 'amp_compute_get' ? { state: 'provisioning' } : get(tool, args));
    first.wc.startSetUp('203.0.113.4', 'Laptop');
    await first.wc.settled();
    expect(first.wc.state().error).toMatch(/within 10 minutes.*picks up where it stopped/);
    const madeSoFar = cloud.calls.length;
    // Restarted while Meko is linked to another account: that account's set-up isn't this one's.
    const other = computer(cloud, pg, { db, waitMs: 0, link: { state: 'linked', account: 'other' } });
    await other.wc.lookup();
    await other.wc.settled();
    expect(cloud.calls.slice(madeSoFar).map((c) => c.tool)).not.toContain('amp_cluster_list');
    // Restarted on the same account, it resumes; failing again, it's still offered in Settings.
    const again = computer(cloud, pg, { db, waitMs: 0 });
    await again.wc.lookup();
    await again.wc.settled();
    expect(again.wc.state()).toMatchObject({ status: 'not-set-up', error: expect.stringMatching(/within 10 minutes/) });
    // Pressed again: the plan is full with the database it made, which needs no more room, and keeps its password.
    cloud.meko.call = get;
    again.deps.waitMs = 60_000;
    again.wc.startSetUp('203.0.113.4', 'Laptop');
    await again.wc.settled();
    expect(again.wc.state()).toMatchObject({ status: 'ready', error: null });
    expect(cloud.tools().filter((t) => t === 'amp_cluster_create' || t === 'datapack_create')).toEqual(['datapack_create', 'amp_cluster_create']);
    expect(again.conns[0].password).toBe(String(cloud.calls.find((c) => c.tool === 'amp_cluster_create')!.args.password));
  });

  it("finishes a join whose new endpoint wasn't ready when the same press comes again", async () => {
    const cloud = fakeCloud();
    const pg = await freshPglite();
    const a = computer(cloud, pg);
    await a.wc.lookup();
    a.wc.startSetUp('203.0.113.4', '');
    await a.wc.settled();
    const b = computer(cloud, pg, { waitMs: 0 });
    await b.wc.lookup();
    const get = cloud.meko.call;
    const old = cloud.computes[0].id;
    cloud.meko.call = async (tool, args) => (tool === 'amp_compute_get' && args.compute_id !== old ? { state: 'provisioning' } : get(tool, args));
    b.wc.startSetUp('198.51.100.7', '');
    await b.wc.settled();
    expect(b.wc.state().error).toMatch(/removed the database's old endpoint.*do the same again in Settings › Work queue/);
    // The record still names the endpoint that's gone: the join makes a new one, its host from AMP.
    cloud.meko.call = get;
    b.deps.waitMs = 60_000;
    b.wc.startSetUp('198.51.100.7', '');
    await b.wc.settled();
    expect(b.wc.state()).toMatchObject({ status: 'ready', error: null });
    expect(cloud.computes).toEqual([expect.objectContaining({ allowed: ['203.0.113.4/32', '198.51.100.7/32'] })]);
    expect(b.conns.at(-1)).toMatchObject({ host: `${cloud.computes[0].id}.amp.example.com` });
  });

  it("won't join through a record whose host isn't the endpoint AMP gives", async () => {
    const cloud = fakeCloud();
    const pg = await freshPglite();
    const a = computer(cloud, pg);
    await a.wc.lookup();
    a.wc.startSetUp('203.0.113.4', '');
    await a.wc.settled();
    const dp = cloud.datapacks.get(WORK_QUEUE_DATAPACK)!;
    const rec = JSON.parse(dp.files.get(/sha256:([0-9a-f]{64})/.exec(dp.description!)![1])!);
    await cloud.meko.setDescription(dp.id, `Kode work queue. Connection: sha256:${await cloud.meko.putFile(dp.id, 'x', JSON.stringify({ ...rec, host: 'evil.example.com' }))}`);
    const b = computer(cloud, pg);
    await b.wc.lookup();
    b.wc.startSetUp('198.51.100.7', '');
    await b.wc.settled();
    expect(b.wc.state()).toMatchObject({ status: 'can-join', error: expect.stringMatching(/doesn't match the database/) });
    expect(b.conns).toEqual([]);
    // Nor does a computer already on the queue switch to it.
    await a.wc.recheck();
    expect(a.conns).toHaveLength(1);
  });

  it('empties the queue when Meko is unlinked, and forgets the record when it links to another account', async () => {
    const cloud = fakeCloud();
    const a = computer(cloud, await freshPglite());
    await a.wc.lookup();
    a.wc.startSetUp('203.0.113.4', '');
    await a.wc.settled();
    await a.queue.create({ title: 'A' });
    a.link.value = { state: 'unlinked', account: '' };
    await a.wc.lookup();
    expect([a.wc.state().status, a.queue.list()]).toEqual(['not-linked', []]);
    // Linked again to the same account: the record still opens the queue.
    a.link.value = { state: 'linked', account: 'acme' };
    await a.wc.lookup();
    expect(a.queue.list().map((i) => i.title)).toEqual(['A']);
    a.link.value = { state: 'linked', account: 'other' };
    await a.wc.lookup();
    expect(kvGet(a.db, 'work-amp')).toBeNull();
    expect(a.queue.list()).toEqual([]);
  });
});

describe("the work queue database's addresses and record", () => {
  it.each([
    ['203.0.113.4', '203.0.113.4/32'],
    [' 203.0.113.0/24 ', '203.0.113.0/24'],
    ['2001:DB8::1', '2001:db8::1/128'],
    ['2001:db8::/48', '2001:db8::/48'],
    ['203.0.113.0/23', null],
    ['0.0.0.0/32', null],
    ['::ffff:203.0.113.4', null],
    ['2001:db8::/47', null],
    ['example.com', null],
    ['203.0.113.4/32/1', null],
  ])('reads %j as %j', (input, want) => {
    expect(checkCidr(input)).toBe(want);
  });

  it.each([
    [{ host: 'ep-1.amp.example.com', port: 6543 }, { host: 'ep-1.amp.example.com', port: 6543, database: 'postgres' }],
    [{ endpoint: { address: 'ep-1.amp.example.com:5433' } }, { host: 'ep-1.amp.example.com', port: 5433, database: 'postgres' }],
    [{ connection_uri: 'postgresql://cloud_admin@EP-1.amp.example.com:5432/neondb?sslmode=require' }, { host: 'ep-1.amp.example.com', port: 5432, database: 'neondb' }],
    [{ state: 'provisioning' }, null],
    [{ host: 'bad host/x' }, null],
  ])("reads the endpoint's address from %j", (reply, want) => {
    expect(endpointOf(reply)).toEqual(want);
  });

  it('turns away a record that is not one', () => {
    const good = {
      version: 1, datapackId: 'd', clusterId: 'c', branchId: 'b', computeId: 'e', host: 'ep.example.com', port: 5432, database: 'postgres',
      user: 'cloud_admin', password: 'p', allowed: [{ cidr: '203.0.113.4/32', label: 'L' }], updatedAt: 1,
    };
    expect(readRecord(JSON.stringify(good))).toMatchObject({ host: 'ep.example.com' });
    for (const bad of [{ ...good, version: 2 }, { ...good, port: 0 }, { ...good, host: 'a b' }, { ...good, allowed: [] }, { ...good, allowed: [{ cidr: '0.0.0.0/0', label: '' }] }, { ...good, password: '' }]) {
      expect(readRecord(JSON.stringify(bad))).toBeNull();
    }
    expect(readRecord('not json')).toBeNull();
  });
});
