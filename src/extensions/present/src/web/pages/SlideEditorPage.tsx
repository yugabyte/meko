// The slide editor: every slide of every scene in the rail, the slide as the room sees it, its blocks
// to edit by hand, its variants, notes, sources and assets, and the Ask bar for changes by asking.

import { ArrowDown, ArrowUp, Plus, Sparkles, Trash2, Upload } from 'lucide-react';
import { useEffect, useRef, useState } from 'react';
import { BLOCK_TYPES, LAYOUTS, newId, type Block, type BlockType, type Deck, type Layout, type Slide } from '../../shared/deck.js';
import type { AskScope } from '../../shared/types.js';
import { api } from '../api.js';
import { AskBar, ChangeSetPanel, DeckBar, JobActivity } from '../components/DeckChrome.js';
import { SlideView } from '../components/SlideView.js';
import { Button, cx, ErrorLine, inputClass, Label, Pill } from '../components/ui.js';
import { navigate } from '../router.js';
import { useDeck, type DeckHandle } from '../useDeck.js';

function emptyBlock(t: BlockType, deck: Deck): Block {
  const asset = Object.keys(deck.assets)[0] ?? '';
  switch (t) {
    case 'heading': return { type: 'heading', text: 'Heading' };
    case 'text': return { type: 'text', markdown: 'Text' };
    case 'bullets': return { type: 'bullets', items: ['First point'], build: false };
    case 'metric': return { type: 'metric', value: '99.99%', label: 'What it measures' };
    case 'quote': return { type: 'quote', text: 'Quote', by: '' };
    case 'table': return { type: 'table', header: ['Column', 'Column'], rows: [['', '']] };
    case 'image': return { type: 'image', asset, alt: '' };
    case 'video': return { type: 'video', asset, caption: '' };
    case 'diagram': return { type: 'diagram', mermaid: 'flowchart LR\n  A --> B' };
    case 'animation': return { type: 'animation', ref: 'present/anim/area:name', params: {} };
    case 'code': return { type: 'code', code: '', language: '' };
  }
}

const area = cx(inputClass, 'min-h-16 font-mono text-[13px]');

/** One block's fields. */
function BlockFields({ b, deck, set }: { b: Block; deck: Deck; set: (b: Block) => void }) {
  const assets = Object.keys(deck.assets);
  switch (b.type) {
    case 'heading': return <input aria-label="Heading" className={inputClass} value={b.text} onChange={(e) => set({ ...b, text: e.target.value })} />;
    case 'text': return <textarea aria-label="Markdown" className={area} value={b.markdown} onChange={(e) => set({ ...b, markdown: e.target.value })} />;
    case 'bullets':
      return (
        <div className="space-y-1">
          <textarea aria-label="Bullets, one a line" className={area} value={b.items.join('\n')} onChange={(e) => set({ ...b, items: e.target.value.split('\n') })} />
          <label className="flex items-center gap-1.5 text-xs"><input type="checkbox" checked={!!b.build} onChange={(e) => set({ ...b, build: e.target.checked })} />Show one at a time</label>
        </div>
      );
    case 'metric':
      return (
        <div className="flex gap-2">
          <input aria-label="Value" className={inputClass} value={b.value} onChange={(e) => set({ ...b, value: e.target.value })} />
          <input aria-label="Label" className={inputClass} value={b.label} onChange={(e) => set({ ...b, label: e.target.value })} />
        </div>
      );
    case 'quote':
      return (
        <div className="space-y-1">
          <textarea aria-label="Quote" className={area} value={b.text} onChange={(e) => set({ ...b, text: e.target.value })} />
          <input aria-label="By" placeholder="Who said it" className={inputClass} value={b.by ?? ''} onChange={(e) => set({ ...b, by: e.target.value })} />
        </div>
      );
    case 'table':
      return (
        <textarea aria-label="Table: a row a line, cells split by |; the first line is the header" className={area}
          value={[b.header, ...b.rows].map((r) => r.join(' | ')).join('\n')}
          onChange={(e) => {
            const rows = e.target.value.split('\n').map((l) => l.split('|').map((c) => c.trim()));
            set({ ...b, header: rows[0] ?? [], rows: rows.slice(1) });
          }} />
      );
    case 'image':
    case 'video':
      return (
        <div className="flex gap-2">
          <select aria-label="Asset" className={inputClass} value={b.asset} onChange={(e) => set({ ...b, asset: e.target.value })}>
            {!assets.includes(b.asset) && <option value={b.asset}>{b.asset || 'Add a file first'}</option>}
            {assets.map((a) => <option key={a}>{a}</option>)}
          </select>
          {b.type === 'image'
            ? <input aria-label="Alt text" placeholder="What it shows" className={inputClass} value={b.alt} onChange={(e) => set({ ...b, alt: e.target.value })} />
            : <input aria-label="Caption" placeholder="Caption" className={inputClass} value={b.caption ?? ''} onChange={(e) => set({ ...b, caption: e.target.value })} />}
        </div>
      );
    case 'diagram': return <textarea aria-label="Mermaid" className={area} value={b.mermaid} onChange={(e) => set({ ...b, mermaid: e.target.value })} />;
    case 'animation':
      return (
        <div className="space-y-1">
          <input aria-label="Animation" className={inputClass} value={b.ref} onChange={(e) => set({ ...b, ref: e.target.value })} />
          <textarea aria-label="Parameters (JSON)" className={area} defaultValue={JSON.stringify(b.params, null, 1)}
            onBlur={(e) => {
              try {
                set({ ...b, params: JSON.parse(e.target.value) as Record<string, unknown> });
              } catch {
                /* kept as typed until it's JSON */
              }
            }} />
        </div>
      );
    case 'code':
      return (
        <div className="space-y-1">
          <textarea aria-label="Code" className={area} value={b.code} onChange={(e) => set({ ...b, code: e.target.value })} />
          <input aria-label="Language" placeholder="Language" className={inputClass} value={b.language ?? ''} onChange={(e) => set({ ...b, language: e.target.value })} />
        </div>
      );
  }
}

function Editor({ h, slideId }: { h: DeckHandle; slideId: string | null }) {
  const s = h.state!;
  const deck = s.deck;
  const scenes = Object.values(deck.scenes);
  const ordered = [deck.start, ...scenes.map((x) => x.id).filter((id) => id !== deck.start)];
  const current = slideId && deck.slides[slideId] ? slideId : (ordered.map((id) => deck.scenes[id]!.slides[0]).find(Boolean) ?? null);
  const scene = scenes.find((x) => current && x.slides.includes(current));
  const [variant, setVariant] = useState(deck.variants[0]!);
  const [block, setBlock] = useState<number | null>(null);
  const [scope, setScope] = useState<AskScope>({ kind: 'slide', slide: current ?? undefined });
  const [draft, setDraft] = useState<Slide | null>(current ? deck.slides[current]! : null);
  const pending = useRef<number | null>(null);
  const fileRef = useRef<HTMLInputElement>(null);

  // The server's slide, unless an edit of ours is on its way.
  const serverSlide = current ? deck.slides[current] : undefined;
  useEffect(() => {
    if (pending.current === null) setDraft(serverSlide ?? null);
  }, [serverSlide]);
  useEffect(() => {
    setBlock(null);
    setScope({ kind: 'slide', slide: current ?? undefined, scene: scene?.id });
  }, [current, scene?.id]);

  const commit = (next: Slide) => {
    setDraft(next);
    if (pending.current !== null) clearTimeout(pending.current);
    pending.current = window.setTimeout(() => {
      pending.current = null;
      void h.edit([{ op: 'updateSlide', slide: { ...next, handEdited: true } }]);
    }, 600);
  };

  if (!current || !draft || !scene) {
    return (
      <div className="flex h-full flex-col">
        <DeckBar h={h} view="slides" />
        <div className="p-6 text-muted-foreground">No slides yet. Build them from the map, or open a scene and add one.</div>
      </div>
    );
  }
  const hasVariant = !!draft.variants[variant];
  const blocks = (draft.variants[variant] ?? { blocks: [] }).blocks;
  const setBlocks = (bs: Block[]) => commit({ ...draft, variants: { ...draft.variants, [variant]: { blocks: bs } } });
  const unsourced = draft.sources.length === 0 && blocks.some((b) => b.type !== 'heading');

  const addSlide = () => {
    const slide: Slide = { id: newId('s'), layout: 'bullets', variants: { [deck.variants[0]!]: { blocks: [{ type: 'heading', text: 'New slide' }] } }, notes: '', sources: [], handEdited: true };
    void h.edit([{ op: 'addSlide', scene: scene.id, slide, index: scene.slides.indexOf(current) + 1 }]).then((ok) => ok && navigate({ view: 'slides', deck: s.id, slide: slide.id }, true));
  };
  const move = (by: number) => {
    const at = scene.slides.indexOf(current) + by;
    if (at >= 0 && at < scene.slides.length) void h.edit([{ op: 'moveSlide', id: current, scene: scene.id, index: at }]);
  };
  const upload = async (f: File) => {
    const name = f.name.replace(/[^A-Za-z0-9._-]+/g, '-').replace(/^[^A-Za-z0-9]+/, '').slice(0, 100) || 'file';
    await h.act(() => api.addAsset(s.id, name, f));
  };

  return (
    <div className="flex h-full flex-col">
      <DeckBar h={h} view="slides">{unsourced && <Pill tone="warn">no sources on this slide</Pill>}</DeckBar>
      <div className="px-3 pt-2"><ErrorLine error={h.error} /></div>
      <div className="grid min-h-0 flex-1 md:grid-cols-[10rem_minmax(0,1fr)_16rem]">
        <nav className="space-y-2 overflow-auto border-r bg-muted/50 p-2" aria-label="Slides">
          {ordered.map((id) => deck.scenes[id]!).map((sc) => (
            <div key={sc.id} className="space-y-1">
              <div className="truncate text-xs font-semibold text-muted-foreground">{sc.title}</div>
              {sc.slides.map((sl, i) => (
                <button key={sl} type="button" aria-label={`${sc.title}, slide ${i + 1}`} aria-current={sl === current}
                  className={cx('block w-full rounded border-2', sl === current ? 'border-primary' : 'border-transparent')}
                  onClick={() => navigate({ view: 'slides', deck: s.id, slide: sl }, true)}>
                  <SlideView deck={deck} deckId={s.id} slide={deck.slides[sl]} variant={deck.variants[0]!} className="pointer-events-none" />
                </button>
              ))}
            </div>
          ))}
          <Button className="w-full justify-center" onClick={addSlide}><Plus className="size-4" />Slide</Button>
        </nav>
        <section className="space-y-3 overflow-auto bg-muted/30 p-4" aria-label="Slide">
          <div className="flex flex-wrap items-center gap-1" role="tablist" aria-label="Variants">
            {deck.variants.map((v) => (
              <button key={v} type="button" role="tab" aria-selected={v === variant} className={cx('rounded-t-md border border-b-0 px-3 py-1 text-sm capitalize', v === variant ? 'bg-card font-semibold shadow-[inset_0_2px_0_var(--color-primary)]' : 'text-muted-foreground')} onClick={() => (setVariant(v), setBlock(null), setScope({ kind: 'slide', slide: current, scene: scene.id }))}>{v}</button>
            ))}
          </div>
          <SlideView deck={deck} deckId={s.id} slide={draft} variant={variant} className="mx-auto max-w-3xl shadow" />
          {!hasVariant ? (
            <div className="rounded-md border border-dashed p-3 text-sm">
              This slide has no “{variant}” variant; the room sees “{deck.variants[0]}”.{' '}
              <Button onClick={() => commit({ ...draft, variants: { ...draft.variants, [variant]: structuredClone(draft.variants[deck.variants[0]!] ?? { blocks: [] }) } })}>Start one from {deck.variants[0]}</Button>
            </div>
          ) : (
            <ol className="mx-auto max-w-3xl space-y-2">
              {blocks.map((b, i) => (
                <li key={i} className={cx('space-y-1 rounded-md border bg-card p-2', block === i && 'border-primary')} onFocus={() => (setBlock(i), setScope({ kind: 'block', slide: current, block: i, scene: scene.id, variant }))}>
                  <div className="flex items-center gap-1">
                    <Pill>{b.type}</Pill>
                    <span className="flex-1" />
                    <Button variant="ghost" aria-label="Move up" disabled={i === 0} onClick={() => setBlocks(blocks.map((x, j) => (j === i - 1 ? b : j === i ? blocks[i - 1]! : x)))}><ArrowUp className="size-4" /></Button>
                    <Button variant="ghost" aria-label="Move down" disabled={i === blocks.length - 1} onClick={() => setBlocks(blocks.map((x, j) => (j === i + 1 ? b : j === i ? blocks[i + 1]! : x)))}><ArrowDown className="size-4" /></Button>
                    <Button variant="ghost" aria-label="Ask about this block" onClick={() => (setBlock(i), setScope({ kind: 'block', slide: current, block: i, scene: scene.id, variant }), document.getElementById('ask-text')?.focus())}><Sparkles className="size-4 text-primary" /></Button>
                    <Button variant="ghost" aria-label="Delete block" onClick={() => setBlocks(blocks.filter((_, j) => j !== i))}><Trash2 className="size-4" /></Button>
                  </div>
                  <BlockFields b={b} deck={deck} set={(nb) => setBlocks(blocks.map((x, j) => (j === i ? nb : x)))} />
                </li>
              ))}
              <li>
                <label className="sr-only" htmlFor="add-block">Add a block</label>
                <select id="add-block" className={inputClass} value="" onChange={(e) => e.target.value && setBlocks([...blocks, emptyBlock(e.target.value as BlockType, deck)])}>
                  <option value="">+ Add a block…</option>
                  {BLOCK_TYPES.map((t) => <option key={t} value={t}>{t}</option>)}
                </select>
              </li>
            </ol>
          )}
        </section>
        <aside className="space-y-4 overflow-auto border-l bg-card p-3" aria-label="Slide settings">
          <label className="block space-y-1 text-sm"><Label>Layout</Label>
            <select className={inputClass} value={draft.layout} onChange={(e) => commit({ ...draft, layout: e.target.value as Layout })}>
              {LAYOUTS.map((l) => <option key={l}>{l}</option>)}
            </select>
          </label>
          <label className="block space-y-1 text-sm"><Label>Notes</Label>
            <textarea className={cx(inputClass, 'min-h-24')} value={draft.notes} onChange={(e) => commit({ ...draft, notes: e.target.value })} />
          </label>
          <div className="space-y-1">
            <Label>Sources</Label>
            {draft.sources.length === 0 ? <p className="text-xs text-warning">No source: check it before it reaches a customer.</p> : (
              <ul className="space-y-1">
                {draft.sources.map((src, i) => (
                  <li key={i} className="break-words font-mono text-[11px] text-muted-foreground">
                    ✓ {src.kind === 'web' ? src.url : `${src.datapack ?? ''} · ${src.kind}${src.excerpt ? ` “${src.excerpt.slice(0, 80)}”` : ''}`}
                  </li>
                ))}
              </ul>
            )}
          </div>
          <div className="space-y-1">
            <Label>Files</Label>
            <ul className="space-y-0.5 text-xs text-muted-foreground">{Object.entries(deck.assets).map(([n, a]) => <li key={n} className="truncate font-mono">{n} · {(a.size / 1024 / 1024).toFixed(1)} MiB</li>)}</ul>
            <input ref={fileRef} type="file" className="hidden" accept="image/*,video/mp4,video/webm" onChange={(e) => (e.target.files?.[0] && void upload(e.target.files[0]), (e.target.value = ''))} />
            <Button onClick={() => fileRef.current?.click()}><Upload className="size-4" />Add a file</Button>
            <p className="text-[11px] text-muted-foreground">Up to 100 MiB each, 1 GiB a deck.</p>
          </div>
          <div className="flex flex-wrap gap-1.5 border-t pt-3">
            <Button variant="ghost" aria-label="Move slide up" onClick={() => move(-1)}><ArrowUp className="size-4" /></Button>
            <Button variant="ghost" aria-label="Move slide down" onClick={() => move(1)}><ArrowDown className="size-4" /></Button>
            <Button variant="danger" onClick={() => void h.edit([{ op: 'removeSlide', id: current }]).then((ok) => ok && navigate({ view: 'slides', deck: s.id, slide: null }, true))}><Trash2 className="size-4" />Delete slide</Button>
          </div>
        </aside>
      </div>
      {h.changeSets.length > 0 && <div className="max-h-[45%] overflow-auto border-t bg-muted/40 p-3"><ChangeSetPanel key={h.changeSets[0]!.id} h={h} cs={h.changeSets[0]!} /></div>}
      <JobActivity h={h} />
      <AskBar h={h} scope={scope} onScope={setScope}
        scopes={[...(block !== null ? [{ kind: 'block' as const, slide: current, block, scene: scene.id, variant }] : []), { kind: 'slide', slide: current, scene: scene.id }, { kind: 'scene', scene: scene.id }, { kind: 'deck' }]}
        placeholder="Shorter, and say what happens to writes if us-east-1 goes down" />
    </div>
  );
}

export function SlideEditorPage({ deckId, slide }: { deckId: string; slide: string | null }) {
  const h = useDeck(deckId);
  if (!h.state) return <main className="p-6">{h.error ? <ErrorLine error={h.error} /> : <p className="text-muted-foreground">Loading the deck…</p>}</main>;
  // One editor per slide: its draft and pending edit belong to that slide.
  return <Editor key={slide ?? ''} h={h} slideId={slide} />;
}
