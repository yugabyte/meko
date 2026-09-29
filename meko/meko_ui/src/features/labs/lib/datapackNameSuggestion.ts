// Default name offered for the datapack a lab run creates.
//
// Dated because a demo gets run repeatedly by the same person, and
// "adlc-code-review" alone collides with the one they made last week — the
// date is what makes the second run's datapack obviously a second run's.

/** e.g. `adlc-code-review-Sep23`. Takes the date so it is testable. */
export function suggestDatapackName(now: Date = new Date()): string {
  const month = now.toLocaleString("en-US", { month: "short" });
  return `adlc-code-review-${month}${now.getDate()}`;
}
