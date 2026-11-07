// What the map and the slide editor share: the deck's bar (save, undo, history, present), the Ask
// bar, Claude's jobs, and the change sets to review before they land.

import { Check, History, Loader2, Play, Redo2, Save, Sparkles, Undo2, X } from 'lucide-react';
import { useEffect, useMemo, useState, type ReactNode } from 'react';
import { defaultTicks, describeOp, tryOps } from '../../shared/ops.js';
import type { AskScope, ChangeSet, DeckVersion, Job } from '../../shared/types.js';
import { api } from '../api.js';
import { navigate } from '../router.js';
import type { DeckHandle } from '../useDeck.js';
import { Button, cx, ErrorLine, inputClass, Pill } from './ui.js';

const ago = (iso: string | null) => {
  if (!iso) return 'never saved';
  const s = Math.max(0, (Date.now() - Date.parse(iso)) / 1000);
  if (s < 60) return 'saved just now';
  if (s < 3600) return `saved ${Math.round(s / 60)} min ago`;
  return `saved ${new Date(iso).toLocaleString()}`;
};

export function DeckBar({ h, view, children }: { h: DeckHandle; view: 'map' | 'slides'; children?: ReactNode }) {
  const s = h.state!;
  const [history, setHistory] = useState(false);
  useEffect(() => {
    const on = (e: KeyboardEvent) => {
      const t = e.target as HTMLElement;
      if (t.closest('input, textarea, select, [contenteditable="true"]')) return;
      if ((e.metaKey || e.ctrlKey) && e.key.toLowerCase() === 'z') {
        e.preventDefault();
        void h.act(() => (e.shiftKey ? api.redo(s.id) : api.undo(s.id)));
      }
    };
    addEventListener('keydown', on);
    return () => removeEventListener('keydown', on);
  }, [h, s.id]);
  const present = async () => {
    const run = await h.act(() => api.startRun(s.id));
    if (run) navigate({ view: 'console', run: run.id });
  };
  return (
    <div className="flex flex-wrap items-center gap-2 border-b bg-muted/60 px-3 py-2">
      <button type="button" className="text-sm text-muted-foreground hover:underline" onClick={() => navigate({ view: 'decks' })}>Decks</button>
      <span className="text-muted-foreground">/</span>
      <span className="font-semibold">{s.deck.title}</span>
      {s.saving ? <Pill tone="info">saving {s.saving.done}/{s.saving.total}</Pill> : s.dirty ? <Pill tone="warn">unsaved changes</Pill> : <Pill tone="ok">{ago(s.savedAt)}</Pill>}
      <div className="mx-2 flex rounded-md border bg-card p-0.5 text-sm">
        <button type="button" className={cx('rounded px-2', view === 'map' && 'bg-accent font-semibold')} onClick={() => navigate({ view: 'map', deck: s.id })}>Map</button>
        <button type="button" className={cx('rounded px-2', view === 'slides' && 'bg-accent font-semibold')} onClick={() => navigate({ view: 'slides', deck: s.id, slide: null })}>Slides</button>
      </div>
      {children}
      <span className="flex-1" />
      <Button variant="ghost" title="Undo (⌘Z)" disabled={!s.canUndo} onClick={() => void h.act(() => api.undo(s.id))}><Undo2 className="size-4" /></Button>
      <Button variant="ghost" title="Redo (⌘⇧Z)" disabled={!s.canRedo} onClick={() => void h.act(() => api.redo(s.id))}><Redo2 className="size-4" /></Button>
      <Button onClick={() => setHistory(true)}><History className="size-4" />History</Button>
      <Button disabled={!!s.saving || !s.dirty} onClick={() => void h.act(() => api.save(s.id))}><Save className="size-4" />Save</Button>
      <Button variant="primary" onClick={() => void present()}><Play className="size-4" />Present</Button>
      {history && <HistoryDialog h={h} onClose={() => setHistory(false)} />}
    </div>
  );
}

function HistoryDialog({ h, onClose }: { h: DeckHandle; onClose: () => void }) {
  const id = h.state!.id;
  const { act } = h;
  const [versions, setVersions] = useState<DeckVersion[] | null>(null);
  // Once per open, on act (stable) rather than h, which is new at every render of the page.
  useEffect(() => {
    void act(() => api.versions(id)).then((v) => setVersions(v ?? []));
  }, [act, id]);
  return (
    <div role="dialog" aria-modal="true" aria-label="History" className="fixed inset-0 z-50 grid place-items-center bg-black/40 p-4" onClick={onClose}>
      <div className="w-full max-w-md rounded-lg border bg-card p-4 shadow-xl" onClick={(e) => e.stopPropagation()}>
        <div className="mb-3 flex items-center justify-between"><h2 className="font-semibold">Saved versions</h2><Button variant="ghost" onClick={onClose}><X className="size-4" /></Button></div>
        {!versions ? <p className="text-sm text-muted-foreground">Loading…</p> : versions.length === 0 ? <p className="text-sm text-muted-foreground">Nothing saved yet.</p> : (
          <ul className="max-h-80 space-y-1 overflow-auto">
            {versions.map((v, i) => (
              <li key={v.hash} className="flex items-center justify-between gap-2 rounded px-2 py-1 hover:bg-accent">
                <span className="text-sm">{new Date(v.savedAt).toLocaleString()}{i === 0 && ' (newest)'}</span>
                <Button onClick={() => void h.act(() => api.restore(id, v.hash)).then((s) => s && onClose())}>Restore</Button>
              </li>
            ))}
          </ul>
        )}
        <p className="mt-3 text-xs text-muted-foreground">Restoring makes that version the draft; Undo goes back, and Save makes it the newest.</p>
      </div>
    </div>
  );
}

const SCOPE_LABEL: Record<AskScope['kind'], string> = { block: 'This block', slide: 'This slide', scene: 'This scene', deck: 'Whole deck' };

/** Ask Claude for a change, small or large, about the selection; the answer comes back as a change set. */
export function AskBar({ h, scope, scopes, onScope, placeholder }: {
  h: DeckHandle; scope: AskScope; scopes: AskScope[]; onScope: (s: AskScope) => void; placeholder: string;
}) {
  const [text, setText] = useState('');
  const send = async () => {
    if (!text.trim()) return;
    const job = await h.act(() => api.ask(h.state!.id, text.trim(), scope));
    if (job) setText('');
  };
  return (
    <form className="flex flex-wrap items-center gap-2 border-t bg-muted/60 px-3 py-2" onSubmit={(e) => (e.preventDefault(), void send())}>
      <label className="sr-only" htmlFor="ask-scope">What to ask about</label>
      <select id="ask-scope" className="rounded-md border border-primary bg-primary/10 px-2 py-1 text-sm font-semibold text-primary" value={scopes.indexOf(scopes.find((s) => s.kind === scope.kind) ?? scopes[0]!)}
        onChange={(e) => onScope(scopes[Number(e.target.value)]!)}>
        {scopes.map((s, i) => <option key={s.kind} value={i}>{SCOPE_LABEL[s.kind]}</option>)}
      </select>
      <label className="sr-only" htmlFor="ask-text">Ask Claude</label>
      <input id="ask-text" className={cx(inputClass, 'min-w-48 flex-1')} placeholder={placeholder} value={text} onChange={(e) => setText(e.target.value)} />
      <Button type="submit" variant="primary" disabled={!text.trim()}><Sparkles className="size-4" />Ask</Button>
    </form>
  );
}

/** Claude's running and recent jobs on the deck, with what each is doing. */
export function JobActivity({ h }: { h: DeckHandle }) {
  const [open, setOpen] = useState<string | null>(null);
  const shown = h.jobs.filter((j) => j.status === 'queued' || j.status === 'running' || (j.endedAt && Date.now() - j.endedAt < 10 * 60_000)).slice(0, 4);
  if (!shown.length) return null;
  const tone = (j: Job) => (j.status === 'done' ? 'ok' : j.status === 'failed' ? 'bad' : j.status === 'cancelled' ? 'warn' : 'info');
  return (
    <div className="space-y-1 border-t bg-card px-3 py-2 text-sm">
      {shown.map((j) => (
        <div key={j.id}>
          <div className="flex items-center gap-2">
            {(j.status === 'running' || j.status === 'queued') && <Loader2 className="size-4 animate-spin text-primary" />}
            <button type="button" className="truncate text-left font-medium hover:underline" onClick={() => setOpen(open === j.id ? null : j.id)}>{j.title}</button>
            <Pill tone={tone(j)}>{j.status}</Pill>
            <span className="min-w-0 flex-1 truncate font-mono text-xs text-muted-foreground">{j.error ?? j.activity.at(-1) ?? ''}</span>
            {(j.status === 'running' || j.status === 'queued') && <Button variant="ghost" onClick={() => void h.act(() => api.cancelJob(j.id))}>Cancel</Button>}
          </div>
          {open === j.id && (
            <ol className="mt-1 max-h-40 overflow-auto rounded bg-muted p-2 font-mono text-xs text-muted-foreground">
              {j.activity.length ? j.activity.map((a, i) => <li key={i}>{a}</li>) : <li>Nothing yet.</li>}
            </ol>
          )}
        </div>
      ))}
    </div>
  );
}

/** A change set: each operation with a tick, a preview of whether the ticked ones fit, apply or discard. */
export function ChangeSetPanel({ h, cs, onAskAgain }: { h: DeckHandle; cs: ChangeSet; onAskAgain?: (text: string) => void }) {
  const deck = h.state!.deck;
  const [ticks, setTicks] = useState(() => defaultTicks(deck, cs.ops));
  const lines = useMemo(() => cs.ops.map((o) => describeOp(deck, o)), [deck, cs.ops]);
  const taken = ticks.filter(Boolean).length;
  const check = useMemo(() => tryOps(deck, cs.ops.filter((_, i) => ticks[i])), [deck, cs.ops, ticks]);
  return (
    <section aria-label="Changes from Claude" className="rounded-lg border bg-card shadow-lg">
      <div className="flex flex-wrap items-center gap-2 border-b px-3 py-2">
        <Sparkles className="size-4 text-primary" />
        <span className="font-semibold">Change set</span>
        <span className="min-w-0 flex-1 truncate text-sm text-muted-foreground">“{cs.title}”</span>
        <Button variant="ghost" onClick={() => void h.act(() => api.discardChangeSet(cs.id))}>Discard</Button>
        {onAskAgain && <Button onClick={() => onAskAgain(cs.title)}>Ask for changes</Button>}
        <Button variant="primary" disabled={!taken || !check.ok} onClick={() => void h.act(() => api.applyChangeSet(cs.id, ticks.flatMap((t, i) => (t ? [i] : []))))}>
          <Check className="size-4" />Apply {taken} of {cs.ops.length}
        </Button>
      </div>
      {cs.summary && <p className="px-3 pt-2 text-sm">{cs.summary}</p>}
      <ul className="max-h-72 space-y-1 overflow-auto p-3">
        {lines.map((l, i) => (
          <li key={i}>
            <label className="flex cursor-pointer items-start gap-2 rounded border px-2 py-1 text-sm">
              <input type="checkbox" className="mt-1" checked={ticks[i]} onChange={(e) => setTicks((t) => t.map((x, j) => (j === i ? e.target.checked : x)))} />
              <span className={cx('w-5 rounded text-center font-mono font-semibold', l.sym === '+' && 'bg-success/15 text-success', l.sym === '-' && 'bg-destructive/15 text-destructive', l.sym === '~' && 'bg-info/15 text-info')}>{l.sym}</span>
              <span className="min-w-0 flex-1">{l.text}{l.handEdited && <span className="block text-xs text-warning">Unticked: it overwrites your own edits. Tick it to take Claude's version anyway.</span>}</span>
            </label>
          </li>
        ))}
      </ul>
      {!check.ok && taken > 0 && <div className="px-3 pb-3"><ErrorLine error={`These changes together would break the deck: ${check.problems[0]}`} /></div>}
    </section>
  );
}
