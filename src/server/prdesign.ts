import { mkdirSync, writeFileSync } from 'node:fs';
import { join } from 'node:path';
import type { Core } from './core.js';
import { get, run } from './db.js';
import { MAX_DESIGN_HTML, MAX_DESIGN_MARKDOWN, parseDesignLine, sha256Hex, type PrDesignRef } from './design.js';
import { errMsg } from './log.js';
import { launchedAgentId, withMekoScope } from './meko.js';
import { datapackIdsIn } from './prreview.js';
import { projectDatapacks, type SessionRow } from './store.js';

/**
 * A PR's design, for a Review a PR session: the PR's description names it with a `meko_design:`
 * line (its Meko name and the latest version's hash, which its author's AgentK keeps current), and
 * AgentK fetches that version from Meko by its hash. The copy becomes the review session's design,
 * shown in its Design panel and read by Claude from the session's designs folder. It's never sent
 * to Meko again.
 */

/** How long fetching a PR's design may take. */
export const PR_DESIGN_TIMEOUT_MS = 15000;

/** A PR's design as fetched. */
export interface FetchedPrDesign {
  ref: PrDesignRef;
  ext: 'md' | 'html';
  text: string;
}

/** What a review session's copy of its PR's design came from (`designs.source`), or null for none. */
export function prDesignSource(core: Core, sessionId: string): { ref: PrDesignRef; told: string | null } | null {
  const r = get<{ source: string | null; source_told: string | null }>(core.db, 'SELECT source, source_told FROM designs WHERE session_id = ?', sessionId);
  if (!r?.source) return null;
  try {
    const v = JSON.parse(r.source) as Partial<PrDesignRef>;
    if (typeof v.name !== 'string' || typeof v.sha256 !== 'string') return null;
    return { ref: { name: v.name, sha256: v.sha256 }, told: r.source_told };
  } catch {
    return null;
  }
}

/** Where Claude reads a review session's copy of its PR's design. */
export function prDesignFile(core: Core, row: SessionRow, name: string): string {
  return join(core.paths.designs, row.id, name.endsWith('.html') ? 'design.html' : 'design.md');
}

/**
 * Fetches the design a PR's description names from Meko: from the datapacks its Meko lines name
 * that the review's project has (or, when it names none, from each of the project's), checked
 * against the hash. Null when the PR names no design, Meko is off, or no datapack has that version
 * (an expired one too); a Meko failure is logged and gives null.
 */
export async function fetchPrDesign(core: Core, row: SessionRow, body: string): Promise<FetchedPrDesign | null> {
  const ref = parseDesignLine(body);
  if (!ref || !core.mekoStatus.configured) return null;
  const dps = projectDatapacks(core.db, row.project_id);
  const named = datapackIdsIn(body);
  const tries = named.length ? dps.filter((d) => named.includes(d.id)) : dps;
  if (!tries.length) return null;
  const ext = ref.name.endsWith('.html') ? 'html' : 'md';
  const max = ext === 'html' ? MAX_DESIGN_HTML : MAX_DESIGN_MARKDOWN;
  return withMekoScope(row, async () => {
    const conv = await core.projects.ensureConversation(row.project_id, PR_DESIGN_TIMEOUT_MS);
    for (const dp of tries) {
      let text: string | null;
      try {
        text = await core.meko.getArtifact(ref.sha256, dp.id, launchedAgentId(row), conv, max, PR_DESIGN_TIMEOUT_MS);
      } catch (e) {
        core.log.warn(`fetching PR #${row.pr_number}'s design ${ref.name} from ${dp.name} failed for ${row.id}`, errMsg(e));
        continue;
      }
      if (text === null) continue;
      if (sha256Hex(text) !== ref.sha256) {
        core.log.warn(`PR #${row.pr_number}'s design ${ref.name} from ${dp.name} doesn't match its hash; not used`);
        continue;
      }
      return { ref, ext, text };
    }
    return null;
  });
}

/**
 * Keeps a fetched PR design as the review session's design: the row the Design panel shows
 * (marked sent, so it's never sent to Meko) and the file Claude reads.
 */
export function savePrDesign(core: Core, row: SessionRow, d: FetchedPrDesign): void {
  const file = prDesignFile(core, row, d.ref.name);
  mkdirSync(join(core.paths.designs, row.id), { recursive: true });
  writeFileSync(file, d.text);
  const markdown = d.ext === 'md' ? d.text : '';
  const html = d.ext === 'html' ? d.text : null;
  run(core.db, `INSERT INTO designs(session_id, markdown, html, artifact_url, updated_at, md_sent, html_sent, source)
                VALUES (?, ?, ?, NULL, ?, 1, 1, ?)
                ON CONFLICT(session_id) DO UPDATE SET markdown = excluded.markdown, html = excluded.html,
                  artifact_url = NULL, updated_at = excluded.updated_at, md_sent = 1, html_sent = 1, source = excluded.source`,
  row.id, markdown, html, Date.now(), JSON.stringify(d.ref));
  core.changed();
}

/** Notes that Claude has been pointed at this version of the PR's design. */
export function markPrDesignTold(core: Core, sessionId: string, sha256: string): void {
  run(core.db, 'UPDATE designs SET source_told = ? WHERE session_id = ?', sha256, sessionId);
}
