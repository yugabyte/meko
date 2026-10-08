// Default name offered for the datapack a lab run creates.
//
// Dated because a demo gets run repeatedly by the same person, and
// "adlc-code-review" alone collides with the one they made last week — the
// date is what makes the second run's datapack obviously a second run's.

/** `adlc-code-review-Sep23`, before any suffix. */
function datedBaseName(now: Date): string {
  const month = now.toLocaleString("en-US", { month: "short" });
  return `adlc-code-review-${month}${now.getDate()}`;
}

/**
 * e.g. `adlc-code-review-Sep23`, or `adlc-code-review-Sep23-2` when that name
 * is taken. Both arguments are injected so this is testable.
 *
 * The date alone separates runs on different days, but a user who restarts
 * the lab an hour later gets the same name back and `POST /datapacks` rejects
 * it as a duplicate — a dead end on step 2, in the one flow whose whole job is
 * to get someone moving. Suffixing counts the run instead.
 *
 * Comparison is case-insensitive and trimmed as a readability choice, not a
 * backend constraint: the uniqueness index is on a plain VARCHAR matched with
 * `=`, so `ADLC-Code-Review-Oct7` and `adlc-code-review-Oct7` would coexist
 * happily. Offering a name that differs from an existing one only by case is
 * just a confusing thing to suggest.
 */
export function suggestDatapackName(
  existingNames: readonly string[] = [],
  now: Date = new Date(),
): string {
  const taken = new Set(existingNames.map((name) => name.trim().toLowerCase()));

  const base = datedBaseName(now);
  if (!taken.has(base.toLowerCase())) return base;
  // Starts at 2: the unsuffixed name is the first run, so `-2` reads as the
  // second rather than leaving a gap where `-1` would be.
  for (let n = 2; ; n += 1) {
    const candidate = `${base}-${n}`;
    if (!taken.has(candidate.toLowerCase())) return candidate;
  }
}
