/**
 * A repo's MEKO.md: the Meko datapacks that hold context for the code beside it, in a Markdown
 * table with a Datapack, an ID and a What's-in-it column (any folder may have one). The local code
 * review lists them for its passes, and the gotchas datapack is the one whose name ends in
 * `/gotchas` (docs/design/code-review.md).
 */

export interface MekoMdDatapack {
  name: string;
  id: string;
  about: string;
}

/** A datapack id as Meko gives them: a UUID, or at least something hex-like that long. */
const DATAPACK_ID = /^[0-9a-f]{8}-?[0-9a-f]{4}-?[0-9a-f]{4}-?[0-9a-f]{4}-?[0-9a-f]{12}$/i;

/** One table row's cells, with the outer pipes, backticks and padding gone. */
function cells(line: string): string[] {
  let t = line.trim();
  if (t.startsWith('|')) t = t.slice(1);
  if (t.endsWith('|')) t = t.slice(0, -1);
  return t.split('|').map((c) => c.trim().replace(/^`+|`+$/g, '').trim());
}

/** The column each header names, by what it says rather than where it is. */
function columns(header: string[]): { name: number; id: number; about: number } | null {
  const find = (re: RegExp) => header.findIndex((h) => re.test(h.toLowerCase()));
  const name = find(/^datapack/);
  const id = find(/^id$/);
  const about = find(/what/);
  return name < 0 || id < 0 ? null : { name, id, about };
}

/** Every datapack a MEKO.md's tables list, in order, once each. Rows without an id that looks like one are left out. */
export function parseMekoMd(text: string): MekoMdDatapack[] {
  const out: MekoMdDatapack[] = [];
  const seen = new Set<string>();
  const lines = text.split('\n');
  for (let i = 0; i + 1 < lines.length; i++) {
    if (!lines[i].trim().startsWith('|') || !/^\s*\|?[\s:|-]+$/.test(lines[i + 1]) || !lines[i + 1].includes('-')) continue;
    const cols = columns(cells(lines[i]));
    if (!cols) continue;
    let j = i + 2;
    for (; j < lines.length && lines[j].trim().startsWith('|'); j++) {
      const row = cells(lines[j]);
      const name = row[cols.name] ?? '';
      const id = row[cols.id] ?? '';
      if (!name || !DATAPACK_ID.test(id) || seen.has(id)) continue;
      seen.add(id);
      out.push({ name, id, about: cols.about >= 0 ? row[cols.about] ?? '' : '' });
    }
    i = j - 1;
  }
  return out;
}

/** The gotchas datapack: the cloud's miner names them `adlc/repo/<repo>/gotchas`. */
export function isGotchasDatapack(name: string): boolean {
  return name.trim().endsWith('/gotchas');
}
