import { lazy, Suspense } from 'react';
import { appUrl } from './base.js';
import { AudiencePage } from './pages/AudiencePage.js';
import { ConsolePage } from './pages/ConsolePage.js';
import { DecksPage } from './pages/DecksPage.js';
import { NewDeckPage } from './pages/NewDeckPage.js';
import { SlideEditorPage } from './pages/SlideEditorPage.js';
import { ErrorBoundary } from './components/ErrorBoundary.js';
import { routePath, useRoute, type Route } from './router.js';

// The canvas library is the biggest; only the map loads it.
const MapPage = lazy(() => import('./pages/MapPage.js').then((m) => ({ default: m.MapPage })));

function Header() {
  return (
    <header className="flex items-center gap-2 border-b bg-card px-3 py-1.5">
      <img src={appUrl('logo.png')} alt="" className="size-7" />
      <span className="font-semibold">Present</span>
      <span className="flex-1" />
      {/* Meko's own pages, on the same web listener. */}
      <a href="/" className="text-sm text-muted-foreground hover:underline">Meko</a>
    </header>
  );
}

export function App() {
  const r = useRoute();
  return <ErrorBoundary key={routePath(r)}><Page r={r} /></ErrorBoundary>;
}

function Page({ r }: { r: Route }) {
  if (r.view === 'audience') return <AudiencePage runId={r.run} />;
  if (r.view === 'console') return <ConsolePage runId={r.run} />;
  return (
    <div className="flex h-full flex-col">
      <Header />
      <div className="min-h-0 flex-1">
        {r.view === 'decks' && <DecksPage />}
        {r.view === 'new' && <NewDeckPage />}
        {r.view === 'map' && <Suspense fallback={<p className="p-6 text-muted-foreground">Loading the map…</p>}><MapPage deckId={r.deck} /></Suspense>}
        {r.view === 'slides' && <SlideEditorPage deckId={r.deck} slide={r.slide} />}
      </div>
    </div>
  );
}
