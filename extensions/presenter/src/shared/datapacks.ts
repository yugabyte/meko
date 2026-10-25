// Presenter's own datapacks have names; content comes from any other datapack, with no naming rule.

export const TEMPLATE_PREFIX = 'presenter/template/';
export const ANIM_PREFIX = 'presenter/anim/';
export const DECK_PREFIX = 'presenter/deck/';

/** Meko's datapack name limit (`datapack_create`: 3 to 60 characters). */
export const DATAPACK_NAME_MAX = 60;

export interface DatapackRow {
  id: string;
  name: string;
  description: string;
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

/** A deck's datapack name from its title: presenter/deck/<slug>, within Meko's limit. */
export function deckDatapackName(title: string): string {
  const slug = title.toLowerCase().normalize('NFKD').replace(/[^a-z0-9]+/g, '-').replace(/^-+|-+$/g, '');
  return DECK_PREFIX + (slug.slice(0, DATAPACK_NAME_MAX - DECK_PREFIX.length).replace(/-+$/, '') || 'deck');
}
