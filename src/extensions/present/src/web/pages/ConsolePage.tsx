// The presenter's console: the slide now and what comes next, the fork's choices, depth, jump and
// return, blank, notes and sources, and the path taken so far. The audience window follows it.

import { ArrowLeft, ArrowRight, CornerUpLeft, ExternalLink, EyeOff, Layers, Search, X } from 'lucide-react';
import { useEffect, useMemo, useState } from 'react';
import { choices, own, type Deck } from '../../shared/deck.js';
import { afterScene, atChoice, navigate as navRun, slideAt, type RunState } from '../../shared/run.js';
import { appUrl } from '../base.js';
import { SlideView } from '../components/SlideView.js';
import { cx, ErrorLine, Pill } from '../components/ui.js';
import { navigate, routePath } from '../router.js';
import { navForKey, useRun } from '../useRun.js';

function Key({ k }: { k: string }) {
  return <span className="rounded border border-b-2 border-neutral-600 bg-neutral-900 px-1 font-mono text-[11px] text-neutral-400">{k}</span>;
}

function CBtn({ k, children, onClick, hot, disabled }: { k?: string; children: React.ReactNode; onClick: () => void; hot?: boolean; disabled?: boolean }) {
  return (
    <button type="button" disabled={disabled} onClick={onClick}
      className={cx('inline-flex items-center gap-1.5 rounded-md border px-2.5 py-1.5 text-sm disabled:opacity-40', hot ? 'border-primary text-orange-200' : 'border-neutral-700 bg-neutral-900 text-neutral-200 hover:bg-neutral-800')}>
      {k && <Key k={k} />}{children}
    </button>
  );
}

function Timer({ since }: { since: number }) {
  const [now, setNow] = useState(() => Date.now());
  useEffect(() => {
    const t = setInterval(() => setNow(Date.now()), 1000);
    return () => clearInterval(t);
  }, []);
  const s = Math.max(0, Math.floor((now - since) / 1000));
  return <span className="font-mono text-xl text-white tabular-nums">{Math.floor(s / 60)}:{String(s % 60).padStart(2, '0')}</span>;
}

/** The jump palette: every scene, detours first, by name. */
function JumpPalette({ deck, run, onJump, onClose }: { deck: Deck; run: RunState; onJump: (scene: string) => void; onClose: () => void }) {
  const [q, setQ] = useState('');
  const list = Object.values(deck.scenes)
    .filter((s) => s.id !== run.pos.scene && s.title.toLowerCase().includes(q.toLowerCase()))
    .sort((a, b) => Number(!!b.detour) - Number(!!a.detour) || a.title.localeCompare(b.title));
  return (
    <div role="dialog" aria-label="Jump to a scene" className="fixed inset-0 z-50 grid place-items-start justify-center bg-black/60 pt-[12vh]" onClick={onClose}>
      <div className="w-[min(90vw,32rem)] rounded-lg border border-neutral-700 bg-neutral-950 p-2 shadow-xl" onClick={(e) => e.stopPropagation()}>
        <div className="flex items-center gap-2 px-2"><Search className="size-4 text-neutral-500" />
          <input autoFocus aria-label="Scene" className="w-full bg-transparent py-2 text-white outline-none" placeholder="Jump to…" value={q} onChange={(e) => setQ(e.target.value)}
            onKeyDown={(e) => (e.key === 'Enter' && list[0] ? onJump(list[0].id) : e.key === 'Escape' && onClose())} />
          <button type="button" aria-label="Close" onClick={onClose}><X className="size-4 text-neutral-500" /></button>
        </div>
        <ul className="max-h-80 overflow-auto">
          {list.map((s) => (
            <li key={s.id}><button type="button" className="flex w-full items-center gap-2 rounded px-2 py-1.5 text-left text-neutral-200 hover:bg-neutral-800" onClick={() => onJump(s.id)}>
              <span className="flex-1 truncate">{s.title}</span>{s.detour && <Pill tone="warn">detour</Pill>}{run.visited.includes(s.id) && <span className="text-xs text-neutral-500">shown</span>}
            </button></li>
          ))}
        </ul>
      </div>
    </div>
  );
}

export function ConsolePage({ runId }: { runId: string }) {
  const { deck, run, error, connected, nav } = useRun(runId);
  const [jump, setJump] = useState(false);

  useEffect(() => {
    const on = (e: KeyboardEvent) => {
      if (!run || jump || (e.target as HTMLElement).closest('input, textarea')) return;
      if (e.key === 'j' || e.key === 'J') return void (e.preventDefault(), setJump(true));
      if ((e.key === 'v' || e.key === 'V') && deck) {
        const i = deck.variants.indexOf(run.variant);
        return void nav({ action: 'variant', variant: deck.variants[(i + 1) % deck.variants.length]! });
      }
      if (e.key === 'r' || e.key === 'R') return void nav({ action: 'return' });
      const n = navForKey(e, run.blank);
      if (!n) return;
      e.preventDefault();
      nav(n);
    };
    addEventListener('keydown', on);
    return () => removeEventListener('keydown', on);
  }, [run, deck, nav, jump]);

  const next = useMemo(() => (deck && run ? navRun(deck, run, { action: 'next' }) : null), [deck, run]);
  // useRun fits the run to this deck, so its scene is there; a deck with no start scene can't be shown.
  const scene = deck && run ? own(deck.scenes, run.pos.scene) : undefined;
  if (!deck || !run || !scene) return <main className="grid h-full place-items-center bg-neutral-950 text-neutral-400">{error ? <ErrorLine error={error} /> : 'Connecting…'}</main>;

  const slideId = slideAt(deck, run.pos);
  const slide = slideId ? deck.slides[slideId] : undefined;
  const fork = atChoice(deck, run) ? choices(deck, scene.id) : [];
  const nextSlideId = next && !next.ended && (next.pos.scene !== run.pos.scene || next.pos.slide !== run.pos.slide) ? slideAt(deck, next.pos) : undefined;
  const after = afterScene(deck, scene.id);
  const unsourced = slide && slide.sources.length === 0;
  const audience = () => window.open(routePath({ view: 'audience', run: run.id }), `present-audience-${run.id}`, 'popup');

  return (
    <main className="grid min-h-full bg-neutral-950 text-neutral-200 lg:grid-cols-[minmax(0,1.6fr)_minmax(0,1fr)]">
      <section className="space-y-3 p-4" aria-label="Now">
        <div className="flex flex-wrap items-center gap-2">
          <img src={appUrl('logo.png')} alt="" className="size-6" />
          <span className="text-[11px] font-semibold uppercase tracking-wide text-neutral-400">Now · {scene.title}{scene.fork ? ' (fork)' : scene.detour ? ' (detour)' : ''}</span>
          {!connected && <Pill tone="warn">reconnecting</Pill>}
          {run.blank && <Pill tone="warn">audience blanked</Pill>}
          {run.ended && <Pill tone="ok">end of the talk</Pill>}
          <span className="flex-1" />
          <Timer since={run.startedAt} />
        </div>
        <SlideView deck={deck} deckId={run.deckId} slide={slide} variant={run.variant} step={run.pos.step} className="border border-neutral-800" />
        {fork.length > 0 && (
          <div className="flex flex-wrap gap-2" aria-label="Choices">
            {fork.map((c, i) => <CBtn key={c.id} k={c.key} hot={i === 0} onClick={() => nav({ action: 'choose', key: c.key! })}>{c.label}</CBtn>)}
          </div>
        )}
        <div className="flex flex-wrap gap-2">
          <CBtn k="←" onClick={() => nav({ action: 'back' })}><ArrowLeft className="size-4" />Back</CBtn>
          <CBtn k="→" onClick={() => nav({ action: 'next' })} disabled={fork.length > 0 && !after}>Next<ArrowRight className="size-4" /></CBtn>
          <CBtn k="V" onClick={() => nav({ action: 'variant', variant: deck.variants[(deck.variants.indexOf(run.variant) + 1) % deck.variants.length]! })}><Layers className="size-4" />Depth: {run.variant}</CBtn>
          <CBtn k="J" onClick={() => setJump(true)}><Search className="size-4" />Jump…</CBtn>
          <CBtn k="R" disabled={!run.trail.length} onClick={() => nav({ action: 'return' })}><CornerUpLeft className="size-4" />Return</CBtn>
          <CBtn k="B" onClick={() => nav({ action: 'blank', on: !run.blank })}><EyeOff className="size-4" />{run.blank ? 'Unblank' : 'Blank'}</CBtn>
          <CBtn onClick={audience}><ExternalLink className="size-4" />Open audience window</CBtn>
        </div>
        <div className="text-sm text-neutral-300"><b>Notes</b> · {slide?.notes || <span className="text-neutral-500">None.</span>}</div>
      </section>
      <aside className="space-y-4 border-neutral-800 p-4 lg:border-l" aria-label="Talk">
        <div className="space-y-1">
          <span className="text-[11px] font-semibold uppercase tracking-wide text-neutral-400">Path so far</span>
          <ol className="flex flex-wrap gap-1 text-xs">
            {run.visited.map((id) => <li key={id} className={cx('rounded px-1.5 py-0.5', id === scene.id ? 'bg-primary text-black' : 'bg-neutral-800 text-neutral-300')}>{deck.scenes[id]?.title ?? id}</li>)}
          </ol>
          <p className="text-xs text-neutral-500">{Object.keys(deck.scenes).length - run.visited.length} scenes not shown yet</p>
        </div>
        <div className="space-y-1">
          <span className="text-[11px] font-semibold uppercase tracking-wide text-neutral-400">Detours</span>
          <div className="flex flex-wrap gap-1.5">
            {Object.values(deck.scenes).filter((s) => s.detour).map((s) => <CBtn key={s.id} onClick={() => nav({ action: 'jump', scene: s.id })}>{s.title}</CBtn>)}
            {!Object.values(deck.scenes).some((s) => s.detour) && <span className="text-xs text-neutral-500">None in this deck.</span>}
          </div>
        </div>
        <div className="space-y-1">
          <span className="text-[11px] font-semibold uppercase tracking-wide text-neutral-400">Next</span>
          {fork.length > 0 ? <p className="text-xs text-neutral-500">The room picks: {fork.map((c) => `${c.key} ${c.label}`).join(' · ')}</p>
            : nextSlideId ? <SlideView deck={deck} deckId={run.deckId} slide={deck.slides[nextSlideId]} variant={run.variant} className="max-w-xs opacity-80" />
              : <p className="text-xs text-neutral-500">{next?.pos.step !== run.pos.step ? 'The next point on this slide.' : 'The end of the talk.'}</p>}
        </div>
        <div className="space-y-1">
          <span className="text-[11px] font-semibold uppercase tracking-wide text-neutral-400">Sources on screen</span>
          {unsourced ? <p className="text-xs text-orange-300">No source on this slide.</p> : (
            <ul className="space-y-0.5 font-mono text-[11px] text-neutral-400">
              {slide?.sources.map((s, i) => <li key={i} className="break-words">{s.kind === 'web' ? s.url : `${s.datapack ?? ''} · ${s.kind}`}</li>)}
            </ul>
          )}
        </div>
        <button type="button" className="text-xs text-neutral-500 hover:underline" onClick={() => navigate({ view: 'map', deck: run.deckId })}>Back to the deck</button>
      </aside>
      {jump && <JumpPalette deck={deck} run={run} onClose={() => setJump(false)} onJump={(s) => (nav({ action: 'jump', scene: s }), setJump(false))} />}
    </main>
  );
}
