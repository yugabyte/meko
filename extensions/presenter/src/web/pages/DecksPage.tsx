import { Plus } from 'lucide-react';
import { useEffect, useState } from 'react';
import type { DeckSummary, Health } from '../../shared/types.js';
import { api } from '../api.js';
import { Button, ErrorLine, Pill } from '../components/ui.js';
import { navigate } from '../router.js';

export function DecksPage() {
  const [decks, setDecks] = useState<DeckSummary[] | null>(null);
  const [health, setHealth] = useState<Health | null>(null);
  const [error, setError] = useState<string | null>(null);
  useEffect(() => {
    api.decks().then(setDecks, (e: Error) => setError(e.message));
    api.health().then(setHealth, () => undefined);
  }, []);
  return (
    <main className="mx-auto w-full max-w-4xl space-y-4 px-4 py-6">
      <div className="flex items-center justify-between gap-2">
        <h1 className="text-2xl font-bold">Decks</h1>
        <Button variant="primary" onClick={() => navigate({ view: 'new' })}><Plus className="size-4" />New deck</Button>
      </div>
      {health && health.meko !== 'ready' && <ErrorLine error="Presenter can't reach Meko: decks are saved in Meko datapacks, so start it with meko start." />}
      {health && !health.claude && <p className="rounded-md border bg-card px-3 py-2 text-sm text-muted-foreground">Claude Code isn't installed here: you can play and edit decks, and building them needs it.</p>}
      <ErrorLine error={error} />
      {!decks ? <p className="text-muted-foreground">Loading…</p> : decks.length === 0 ? (
        <div className="rounded-lg border border-dashed p-8 text-center">
          <p className="font-medium">No decks yet.</p>
          <p className="mt-1 text-sm text-muted-foreground">A deck starts from datapacks: pick the ones to draw from, write a short brief, and Claude drafts the story map.</p>
        </div>
      ) : (
        <ul className="divide-y rounded-lg border bg-card">
          {decks.map((d) => (
            <li key={d.id}>
              <button type="button" className="flex w-full items-center gap-3 px-4 py-3 text-left hover:bg-accent" onClick={() => navigate({ view: 'map', deck: d.id })}>
                <span className="min-w-0 flex-1">
                  <span className="block truncate font-medium">{d.title}</span>
                  <span className="block truncate font-mono text-xs text-muted-foreground">{d.name}</span>
                </span>
                {d.draft && <Pill tone="info">draft here</Pill>}
                <span className="text-xs text-muted-foreground">{d.savedAt ? new Date(d.savedAt).toLocaleDateString() : ''}</span>
              </button>
            </li>
          ))}
        </ul>
      )}
    </main>
  );
}
