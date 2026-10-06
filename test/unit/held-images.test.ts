import { describe, expect, it } from 'vitest';
import { HeldImages } from '../../src/web/heldImages.js';

/** Held images whose uploads the test finishes by hand (`finish`, `fail`), recording what happened. */
function setup() {
  const pending = new Map<string, { resolve: (path: string) => void; reject: (e: Error) => void }>();
  const revoked: string[] = [];
  const errors: unknown[] = [];
  let changes = 0;
  const held = new HeldImages<string>({
    upload: (name) => new Promise((resolve, reject) => pending.set(name, { resolve, reject })),
    thumb: (name) => `blob:${name}`,
    revoke: (url) => revoked.push(url),
    onChange: () => changes++,
    onError: (e) => errors.push(e),
  });
  const settle = () => new Promise((r) => setTimeout(r, 0));
  return {
    held,
    revoked,
    errors,
    changes: () => changes,
    finish: async (name: string) => {
      pending.get(name)!.resolve(`/pastes/${name}`);
      await settle();
    },
    fail: async (name: string) => {
      pending.get(name)!.reject(new Error(`${name} is not an image`));
      await settle();
    },
  };
}

describe('held images', () => {
  it('upload as they are added, and show which are done', async () => {
    const t = setup();
    t.held.add(['a.png', 'b.png']);
    expect(t.held.list.map((i) => [i.thumb, i.path])).toEqual([
      ['blob:a.png', null],
      ['blob:b.png', null],
    ]);
    expect(t.held.uploading).toBe(true);
    await t.finish('b.png');
    await t.finish('a.png');
    expect(t.held.list.map((i) => i.path)).toEqual(['/pastes/a.png', '/pastes/b.png']);
    expect(t.held.uploading).toBe(false);
  });

  it('are ready in the order they were added, once every upload is done', async () => {
    const t = setup();
    t.held.add(['a.png', 'b.png']);
    let ready: string[] | null = null;
    const waiting = t.held.ready().then((r) => (ready = r?.map((i) => i.path!) ?? null));
    await t.finish('b.png');
    expect(ready).toBeNull();
    await t.finish('a.png');
    await waiting;
    expect(ready).toEqual(['/pastes/a.png', '/pastes/b.png']);
  });

  it('drop a failed upload, report it, and leave the rest held', async () => {
    const t = setup();
    t.held.add(['a.png', 'b.png']);
    const waiting = t.held.ready();
    await t.finish('a.png');
    await t.fail('b.png');
    expect(await waiting).toBeNull();
    expect(t.errors).toHaveLength(1);
    expect(t.held.list.map((i) => i.path)).toEqual(['/pastes/a.png']);
    expect(t.revoked).toEqual(['blob:b.png']);
    expect((await t.held.ready())?.map((i) => i.path)).toEqual(['/pastes/a.png']);
  });

  it('leave out an image removed while they wait, and ignore its upload', async () => {
    const t = setup();
    t.held.add(['a.png', 'b.png']);
    const waiting = t.held.ready();
    t.held.remove(t.held.list[1].id);
    await t.finish('a.png');
    await t.fail('b.png');
    expect((await waiting)?.map((i) => i.path)).toEqual(['/pastes/a.png']);
    expect(t.errors).toEqual([]);
  });

  it('let go of only the images sent, keeping ones added meanwhile', async () => {
    const t = setup();
    t.held.add(['a.png']);
    await t.finish('a.png');
    const sent = (await t.held.ready())!;
    t.held.add(['b.png']);
    t.held.release(sent);
    expect(t.held.list.map((i) => i.thumb)).toEqual(['blob:b.png']);
    expect(t.revoked).toEqual(['blob:a.png']);
  });

  it('revoke their previews when removed or cleared', () => {
    const t = setup();
    t.held.add(['a.png', 'b.png', 'c.png']);
    t.held.remove(t.held.list[0].id);
    t.held.clear();
    expect(t.revoked).toEqual(['blob:a.png', 'blob:b.png', 'blob:c.png']);
    expect(t.held.size).toBe(0);
  });

  it('report each change: added, uploaded, removed', async () => {
    const t = setup();
    t.held.add(['a.png', 'b.png']);
    expect(t.changes()).toBe(1);
    await t.finish('a.png');
    expect(t.changes()).toBe(2);
    t.held.remove(t.held.list[1].id);
    expect(t.changes()).toBe(3);
    t.held.add([]);
    t.held.remove(999);
    expect(t.changes()).toBe(3);
  });

  it('are ready at once with nothing held', async () => {
    expect(await setup().held.ready()).toEqual([]);
  });
});
