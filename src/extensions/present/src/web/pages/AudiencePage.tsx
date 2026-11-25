// The audience window: only the slide, filling the screen, following the console. No controls, no
// notes, no hint of the branches not taken; keys still work for a presenter with one screen.

import { useEffect, useState } from 'react';
import { choices } from '../../shared/deck.js';
import { atChoice, slideAt } from '../../shared/run.js';
import { SlideView } from '../components/SlideView.js';
import { cx } from '../components/ui.js';
import { navForKey, useRun } from '../useRun.js';

export function AudiencePage({ runId }: { runId: string }) {
  const { deck, run, nav } = useRun(runId);
  const [idle, setIdle] = useState(false);

  useEffect(() => {
    const on = (e: KeyboardEvent) => {
      const n = run && navForKey(e, run.blank);
      if (!n) return;
      e.preventDefault();
      nav(n);
    };
    addEventListener('keydown', on);
    return () => removeEventListener('keydown', on);
  }, [run, nav]);
  // The cursor hides after two seconds without moving.
  useEffect(() => {
    let t = window.setTimeout(() => setIdle(true), 2000);
    const move = () => {
      setIdle(false);
      clearTimeout(t);
      t = window.setTimeout(() => setIdle(true), 2000);
    };
    addEventListener('mousemove', move);
    return () => (clearTimeout(t), removeEventListener('mousemove', move));
  }, []);

  const shell = cx('fixed inset-0 grid place-items-center bg-black', idle && 'cursor-none');
  if (!deck || !run) return <div className={shell}><span className="text-sm text-neutral-500">Waiting for the presenter…</span></div>;
  if (run.blank) return <div className={shell} aria-label="Blank" />;
  const slideId = slideAt(deck, run.pos);
  const scene = deck.scenes[run.pos.scene];
  const fork = scene && atChoice(deck, run) ? choices(deck, scene.id) : [];
  return (
    <div className={shell}>
      <div className="relative w-[min(100vw,calc(100vh*16/9))]">
        <SlideView deck={deck} deckId={run.deckId} slide={slideId ? deck.slides[slideId] : undefined} variant={run.variant} step={run.pos.step} className="rounded-none" />
        {scene?.detour && <span className="absolute left-[4%] top-[4%] font-mono text-[1.2vw] uppercase tracking-widest text-slide-accent">Your question</span>}
        {fork.length > 0 && (
          <div className="absolute inset-x-[6%] bottom-[7%] grid gap-[1.2vw]" style={{ gridTemplateColumns: `repeat(${Math.min(fork.length, 4)}, minmax(0, 1fr))` }}>
            {fork.map((c) => (
              <div key={c.id} className="rounded-[0.6vw] border border-slide-muted/50 p-[1vw] text-[1.6vw] text-slide-fg">
                <b className="mr-[0.6vw] font-mono text-slide-accent">{c.key}</b>{c.label}
              </div>
            ))}
          </div>
        )}
      </div>
    </div>
  );
}
