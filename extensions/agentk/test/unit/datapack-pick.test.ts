import { createElement } from 'react';
import { renderToStaticMarkup } from 'react-dom/server';
import { describe, expect, it } from 'vitest';
import type { DatapackInvite, MekoDatapack, Project } from '../../src/shared/types.js';
import { NewProjectDialog } from '../../src/web/components/NewProjectDialog.js';
import { type DpOption, datapackOptions, detectDatapack, searchDatapacks, suggestProjects } from '../../src/web/datapackpick.js';

const dp = (name: string, o: Partial<MekoDatapack> = {}): MekoDatapack => ({
  id: name, name, grant: 'owner', createdAt: null, ownerName: null, ownerEmail: null, description: null, ...o,
});
const invite: DatapackInvite = {
  datapackId: 'adlc/proj/meko/web-ui', name: 'adlc/proj/meko/web-ui', inviteId: 'inv-1', grant: 'contributor', ownerName: 'A teammate',
  ownerEmail: null, repoRemote: null, state: 'invited', firstSeenAt: 0,
};
const projects = [{ name: 'infra', datapacks: [{ id: 'adlc/proj/agent-k-local/infra' }] }] as unknown as Project[];
const options: DpOption[] = datapackOptions(
  [
    dp('adlc/proj/agent-k-local/infra'),
    dp('adlc/proj/agent-k-local/init', { grant: 'contributor', ownerName: 'A teammate' }),
    dp('adlc/proj/agent-k-local/intake', { grant: 'viewer', ownerName: 'A teammate' }),
    dp('adlc/proj/yugabyte-db/old-name'),
    dp('team-notes'),
  ],
  [invite],
  projects,
  [{ datapackId: 'adlc/proj/yugabyte-db/old-name', id: 'p-closed', name: 'Index docs', repoPath: '/code/yugabyte-db' }],
);
const byName = (name: string) => options.find((o) => o.name === name)!;

describe('the Open project dialog: which datapack a project gets', () => {
  const base = { mekoUsable: true, noDatapack: false, picked: null, name: 'init', repoName: 'agent-k-local', options };
  it.each([
    ['opens without one with no Meko, whatever else is picked', { mekoUsable: false, picked: byName('team-notes') }, 'no-meko', { mode: 'none' }],
    ['opens without one with No datapack ticked, over a pick', { noDatapack: true, picked: byName('team-notes') }, 'none', { mode: 'none' }],
    ['uses a picked datapack over the one the name finds', { picked: byName('team-notes') }, 'picked', { mode: 'existing', id: 'team-notes', name: 'team-notes' }],
    ['uses the datapack the name and repo name', {}, 'found', { mode: 'existing', id: 'adlc/proj/agent-k-local/init', name: 'adlc/proj/agent-k-local/init' }],
    ['accepts the invite for the datapack the name and repo name', { name: 'Web UI', repoName: 'meko' }, 'found',
      { mode: 'existing', id: 'adlc/proj/meko/web-ui', name: 'adlc/proj/meko/web-ui', inviteId: 'inv-1' }],
    ['creates the datapack the name and repo name when there is none', { name: 'New thing' }, 'new', { mode: 'create', name: 'adlc/proj/agent-k-local/new-thing' }],
    ['takes the next free name when we can only read the one by that name', { name: 'intake' }, 'new', { mode: 'create', name: 'adlc/proj/agent-k-local/intake-2' }],
    ["tries the name when the list couldn't be read", { name: 'intake', options: [] }, 'new', { mode: 'create', name: 'adlc/proj/agent-k-local/intake' }],
    ['waits for the datapack list', { options: null }, 'loading', null],
    ['waits for a name', { name: ' ' }, 'no-name', null],
    ['waits for a repo', { repoName: '' }, 'no-repo', null],
  ] as const)('%s', (_, o, kind, choice) => {
    const got = detectDatapack({ ...base, ...o });
    expect([got.kind, got.choice]).toEqual([kind, choice]);
  });

  it('names the datapack a repo will give the project before one is picked', () => {
    expect(detectDatapack({ ...base, name: 'My Project', repoName: '' })).toMatchObject({ name: 'adlc/proj/<repo>/my-project' });
  });
});

describe("the Open project dialog's lists", () => {
  it.each([
    ["Project name's dropdown: every project datapack for an empty name", () => suggestProjects('', options),
      ['adlc/proj/meko/web-ui', 'adlc/proj/agent-k-local/init', 'adlc/proj/agent-k-local/intake', 'adlc/proj/yugabyte-db/old-name', 'adlc/proj/agent-k-local/infra']],
    ['the dropdown narrows by project name, a closed project by its own name', () => suggestProjects('in', options),
      ['adlc/proj/agent-k-local/init', 'adlc/proj/agent-k-local/intake', 'adlc/proj/yugabyte-db/old-name', 'adlc/proj/agent-k-local/infra']],
    ['the dropdown matches the project part, not the repo part', () => suggestProjects('agent-k', options), []],
    ['Search all datapacks: by any part of the name, other datapacks included', () => searchDatapacks('team', options), ['team-notes']],
    ['the search matches the repo part too', () => searchDatapacks('yugabyte', options), ['adlc/proj/yugabyte-db/old-name']],
  ])('%s', (_, list, names) => {
    expect(list().map((o) => o.name)).toEqual(names);
  });
});

describe('the Open project dialog without Meko', () => {
  it('has no project dropdown or datapack search, and opens the project without a datapack', () => {
    const html = renderToStaticMarkup(createElement(NewProjectDialog, {
      mekoConfigured: false, projects: [], onClose: () => undefined, onCreated: () => undefined,
    }));
    expect(html).not.toMatch(/id="np-suggest"|Search all datapacks|Create a new one|Use an existing one/);
    expect(html).toMatch(/<input id="np-no-dp" type="checkbox" disabled="" checked=""\/>/);
    expect(html).toContain('Meko isn’t set up, so the project opens without a datapack.');
  });
});
