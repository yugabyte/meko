// New deck: pick the datapacks to draw from (content can be any datapack; templates and animations
// are Presenter's own), write the brief, and Claude drafts the story map.

import { ArrowRight } from 'lucide-react';
import { useEffect, useState } from 'react';
import type { DatapackGroups, DatapackRow } from '../../shared/datapacks.js';
import { emptyBrief, SHAPES, type Brief } from '../../shared/deck.js';
import { api } from '../api.js';
import { Button, cx, ErrorLine, inputClass, Label } from '../components/ui.js';
import { navigate } from '../router.js';

function PackList({ rows, picked, toggle, single }: { rows: DatapackRow[]; picked: Set<string>; toggle: (name: string) => void; single?: boolean }) {
  if (!rows.length) return <p className="px-2 text-sm text-muted-foreground">None.</p>;
  return (
    <ul className="space-y-0.5">
      {rows.map((r) => (
        <li key={r.id}>
          <label className={cx('flex cursor-pointer items-center gap-2 rounded px-2 py-1 text-sm', picked.has(r.name) && 'bg-info/10')}>
            <input type={single ? 'radio' : 'checkbox'} name={single ? 'template' : undefined} checked={picked.has(r.name)} onChange={() => toggle(r.name)} />
            <span className="min-w-0 flex-1 truncate font-mono text-[13px]">{r.name}</span>
            {r.description && <span className="hidden max-w-[40%] truncate text-xs text-muted-foreground md:inline">{r.description}</span>}
          </label>
        </li>
      ))}
    </ul>
  );
}

export function NewDeckPage() {
  const [groups, setGroups] = useState<DatapackGroups | null>(null);
  const [q, setQ] = useState('');
  const [content, setContent] = useState<Set<string>>(new Set());
  const [template, setTemplate] = useState<string | null>(null);
  const [anims, setAnims] = useState<Set<string>>(new Set());
  const [title, setTitle] = useState('');
  const [brief, setBrief] = useState<Brief>(emptyBrief);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);

  useEffect(() => {
    api.datapacks().then(setGroups, (e: Error) => setError(e.message));
  }, []);
  const match = (r: DatapackRow) => !q || r.name.toLowerCase().includes(q.toLowerCase()) || r.description.toLowerCase().includes(q.toLowerCase());
  const filtered = groups && { content: groups.content.filter(match), templates: groups.templates.filter(match), animations: groups.animations.filter(match) };
  const flip = (set: Set<string>, name: string) => {
    const n = new Set(set);
    if (n.has(name)) n.delete(name);
    else n.add(name);
    return n;
  };

  const create = async () => {
    setBusy(true);
    setError(null);
    try {
      const s = await api.createDeck({ title, brief, sources: { content: [...content], template, animations: [...anims] } });
      await api.startJob(s.id, { kind: 'map' });
      navigate({ view: 'map', deck: s.id }, true);
    } catch (e) {
      setError((e as Error).message);
      setBusy(false);
    }
  };

  return (
    <main className="flex h-full flex-col">
      <div className="flex flex-wrap items-center gap-2 border-b bg-muted/60 px-3 py-2">
        <button type="button" className="text-sm text-muted-foreground hover:underline" onClick={() => navigate({ view: 'decks' })}>Decks</button>
        <span className="text-muted-foreground">/</span>
        <span className="font-semibold">New deck</span>
        <span className="flex-1" />
        <Button onClick={() => navigate({ view: 'decks' })}>Cancel</Button>
        <Button variant="primary" disabled={busy || !title.trim() || content.size === 0} onClick={() => void create()}>
          {busy ? 'Creating…' : 'Draft story map'}<ArrowRight className="size-4" />
        </Button>
      </div>
      <div className="grid min-h-0 flex-1 overflow-auto md:grid-cols-[minmax(0,1.3fr)_minmax(0,1fr)]">
        <section className="space-y-4 p-4" aria-label="Datapacks">
          <label className="sr-only" htmlFor="dp-search">Search datapacks</label>
          <input id="dp-search" className={inputClass} placeholder="Search datapacks…" value={q} onChange={(e) => setQ(e.target.value)} />
          {!filtered ? <p className="text-sm text-muted-foreground">Loading datapacks…</p> : (
            <>
              <div className="space-y-1"><Label>Content (any datapack)</Label><PackList rows={filtered.content} picked={content} toggle={(n) => setContent((s) => flip(s, n))} /></div>
              <div className="space-y-1">
                <Label>Template (one)</Label>
                <label className="flex cursor-pointer items-center gap-2 rounded px-2 py-1 text-sm">
                  <input type="radio" name="template" checked={template === null} onChange={() => setTemplate(null)} />The built-in template
                </label>
                <PackList single rows={filtered.templates} picked={new Set(template ? [template] : [])} toggle={setTemplate} />
              </div>
              <div className="space-y-1"><Label>Animations</Label><PackList rows={filtered.animations} picked={anims} toggle={(n) => setAnims((s) => flip(s, n))} /></div>
            </>
          )}
        </section>
        <section className="space-y-3 border-t bg-muted/40 p-4 md:border-l md:border-t-0" aria-label="Brief">
          <Label>Brief</Label>
          <label className="block space-y-1 text-sm"><span>Title</span><input className={inputClass} value={title} onChange={(e) => setTitle(e.target.value)} placeholder="YugabyteDB for Acme" /></label>
          <label className="block space-y-1 text-sm"><span>Audience</span><input className={inputClass} value={brief.audience} onChange={(e) => setBrief({ ...brief, audience: e.target.value })} placeholder="Acme platform team, CTO in the room" /></label>
          <label className="block space-y-1 text-sm"><span>Goal</span><input className={inputClass} value={brief.goal} onChange={(e) => setBrief({ ...brief, goal: e.target.value })} placeholder="Pick one product to trial this quarter" /></label>
          <div className="flex flex-wrap items-center gap-3 text-sm">
            <label className="flex items-center gap-2">Length
              <input type="number" min={5} max={600} className={cx(inputClass, 'w-20')} value={brief.minutes} onChange={(e) => setBrief({ ...brief, minutes: Number(e.target.value) || 30 })} /> min
            </label>
            <span>Shape</span>
            <div role="radiogroup" aria-label="Shape" className="inline-flex overflow-hidden rounded-md border">
              {SHAPES.map((s) => (
                <button key={s} type="button" role="radio" aria-checked={brief.shape === s} className={cx('px-3 py-1 capitalize', brief.shape === s ? 'bg-foreground text-background' : 'bg-card')} onClick={() => setBrief({ ...brief, shape: s })}>{s}</button>
              ))}
            </div>
          </div>
          <label className="block space-y-1 text-sm"><span>Anything else</span>
            <textarea className={cx(inputClass, 'min-h-24')} value={brief.notes} onChange={(e) => setBrief({ ...brief, notes: e.target.value })} placeholder="They run Postgres in us-east and want an EU region by spring. Keep pricing as a detour." />
          </label>
          <p className="text-xs text-muted-foreground">Claude reads these datapacks and can use your other tools (the web, files, MCP servers) while drafting.</p>
          <ErrorLine error={error} />
        </section>
      </div>
    </main>
  );
}
