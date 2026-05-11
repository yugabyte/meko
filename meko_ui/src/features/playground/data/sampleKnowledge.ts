// Sample knowledge content for the Playground "Add sample" flow.
// Inlined as a string so we can synthesize a File on demand without a public-asset fetch.

export const SAMPLE_KNOWLEDGE_FILENAME = "europe-travel-guide.md";

export const SAMPLE_KNOWLEDGE_CONTENT = `# Europe Travel Guide

A short reference for planning a 1–2 week European vacation under tight budgets.

## Spain

- **Best for:** food, beaches, nightlife, mild winters.
- **Typical 1-week cost (per person, mid-range):** ~$1,400 incl. flights from US.
- **Highlights:** Barcelona (Gaudí, Gothic Quarter), Madrid (Prado, tapas crawl), Seville (flamenco, Alcázar), Costa Brava beaches.
- **Best months:** April–June, September–October.
- **Trip pace:** relaxed; long lunches, late dinners.

## United Kingdom

- **Best for:** museums, theatre, countryside hikes, English-speaking ease.
- **Typical 1-week cost (per person, mid-range):** ~$1,800 incl. flights from US.
- **Highlights:** London (British Museum, West End), Edinburgh (Old Town, day-hike Arthur's Seat), Lake District, Cotswolds villages.
- **Best months:** May–September.
- **Trip pace:** moderate; lots of walking.

## Greece

- **Best for:** islands, ancient sites, beach hopping.
- **Typical 1-week cost (per person, mid-range):** ~$1,600 incl. flights from US.
- **Highlights:** Athens (Acropolis), Santorini (caldera views), Crete (Samaria gorge), Naxos (less touristed).
- **Best months:** May–June, September.
- **Trip pace:** flexible; ferry schedules drive the itinerary.

## Italy

- **Best for:** food, art, varied landscapes (Alps to Amalfi).
- **Typical 1-week cost (per person, mid-range):** ~$1,700 incl. flights from US.
- **Highlights:** Rome, Florence, Venice, Cinque Terre coastal hike, Dolomites.
- **Best months:** April–June, September–October.
- **Trip pace:** moderate; trains link most cities easily.

## Portugal

- **Best for:** value, coastline, port wine, walkable cities.
- **Typical 1-week cost (per person, mid-range):** ~$1,300 incl. flights from US.
- **Highlights:** Lisbon, Porto, Algarve beaches, Sintra day-trip.
- **Best months:** April–June, September–October.
- **Trip pace:** relaxed.

## Budget tips

- Book flights 6–10 weeks ahead for best fares from the US East Coast.
- Use point-to-point trains (Trainline / Renfe / Trenitalia) instead of car rentals in dense regions.
- Mid-range hotels run $100–180/night; hostels $30–60/night.
- Many top museums offer one free-entry day per month — plan around it.

## Hiking-friendly options

- **Spain:** Camino de Santiago segments, Picos de Europa.
- **Italy:** Cinque Terre coastal trail, Dolomites Alta Via 1.
- **UK:** Lake District, West Highland Way (Scotland).
- **Greece:** Samaria Gorge (Crete).

## Beach-friendly options

- **Spain:** Costa Brava, Costa del Sol, Mallorca.
- **Greece:** Naxos, Milos, Crete south coast.
- **Portugal:** Algarve (Lagos, Praia da Marinha).
`;

export function buildSampleKnowledgeFile(): File {
  return new File([SAMPLE_KNOWLEDGE_CONTENT], SAMPLE_KNOWLEDGE_FILENAME, {
    type: "text/markdown",
  });
}

/**
 * Open the bundled sample knowledge file as a preview in a new tab.
 *
 * Uses a temporary blob URL so we don't need a hosted asset. The URL is
 * revoked after a short delay — long enough for the browser to load the
 * tab, short enough to release the memory. Returns true if the new tab
 * was opened (the user may have blocked popups; we surface that to the
 * caller so it can fall back / toast).
 */
export function openSampleKnowledgePreview(): boolean {
  const blob = new Blob([SAMPLE_KNOWLEDGE_CONTENT], {
    type: "text/markdown;charset=utf-8",
  });
  const url = URL.createObjectURL(blob);
  const win = window.open(url, "_blank", "noopener,noreferrer");
  // Revoke after the browser has had time to load the URL. Anything below
  // ~1s risks the tab seeing an invalid URL on slow machines.
  setTimeout(() => URL.revokeObjectURL(url), 60_000);
  return Boolean(win);
}
