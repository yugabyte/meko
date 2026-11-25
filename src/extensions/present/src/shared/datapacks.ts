// Present's own datapacks have names; content comes from any other datapack, with no naming rule.

export const TEMPLATE_PREFIX = 'present/template/';
export const ANIM_PREFIX = 'present/anim/';
export const DECK_PREFIX = 'present/deck/';

/** Meko's datapack name limit (`datapack_create`: 3 to 60 characters). */
export const DATAPACK_NAME_MAX = 60;

export interface DatapackRow {
  id: string;
  name: string;
  description: string;
}

/**
 * The Meko a deck's datapack is in: the one on this computer, or the one Meko links to (Meko Cloud),
 * named by its web app's host ('' when Meko doesn't know it), or by its MCP host from a Meko that
 * handed out the link's key. A deck's id is its datapack's, so it exists only there.
 */
export interface MekoAt {
  kind: 'local' | 'cloud';
  host: string;
}

export function sameMeko(a: MekoAt, b: MekoAt): boolean {
  return a.kind === b.kind && (a.kind === 'local' || a.host === b.host);
}

/** The Meko on this computer: the only one a Present from before Meko's link used (its host doesn't matter). */
export const LOCAL_MEKO: MekoAt = { kind: 'local', host: '' };

/** A Meko as the pages name it: "Meko Cloud (host)", or "the Meko on this computer". */
export function mekoLabel(at: MekoAt): string {
  if (at.kind !== 'cloud') return 'the Meko on this computer';
  return at.host ? `Meko Cloud (${at.host})` : 'Meko Cloud';
}

/** What gets a deck saved in `at` back, while Present uses another Meko. */
export function reopenHint(at: MekoAt): string {
  return at.kind === 'cloud'
    ? `Link Meko to ${mekoLabel(at)} again in Meko's Settings › Meko Cloud to open it.`
    : "Unlink Meko Cloud in Meko's Settings › Meko Cloud to open it.";
}

export interface DatapackGroups {
  content: DatapackRow[];
  templates: DatapackRow[];
  animations: DatapackRow[];
  decks: DatapackRow[];
}

export function groupDatapacks(rows: DatapackRow[]): DatapackGroups {
  const g: DatapackGroups = { content: [], templates: [], animations: [], decks: [] };
  for (const r of [...rows].sort((a, b) => a.name.localeCompare(b.name))) {
    if (r.name.startsWith(TEMPLATE_PREFIX)) g.templates.push(r);
    else if (r.name.startsWith(ANIM_PREFIX)) g.animations.push(r);
    else if (r.name.startsWith(DECK_PREFIX)) g.decks.push(r);
    else g.content.push(r);
  }
  return g;
}

/** A deck's datapack name from its title: present/deck/<slug>, within Meko's limit. */
export function deckDatapackName(title: string): string {
  const slug = title.toLowerCase().normalize('NFKD').replace(/[^a-z0-9]+/g, '-').replace(/^-+|-+$/g, '');
  return DECK_PREFIX + (slug.slice(0, DATAPACK_NAME_MAX - DECK_PREFIX.length).replace(/-+$/, '') || 'deck');
}
