// Pre-processing for LLM-emitted markdown before it hits the renderer.
//
// Two known shapes the conversation API surfaces that need fixing:
//
//   1. Escaped newlines: the message body sometimes arrives as
//      "Foo.\\nBar" instead of "Foo.\nBar" (typically because the
//      generation was JSON-stringified somewhere upstream and didn't
//      get reversed). We unescape only when we actually see `\\n` to
//      avoid touching content that's already correct.
//
//   2. Broken ordered-list markers: models occasionally emit
//      "1.\n\n**Title**" instead of "1. **Title**". react-markdown
//      parses the marker as plain text, breaking the list. We rejoin
//      the marker with the next non-empty line.
//
// Returns the original string for empty/undefined input — the renderer
// handles falsy text with its own length check.

export function normalizeMarkdownText(value?: string): string {
  if (!value) return "";
  const unescaped = value.includes("\\n") ? value.replaceAll("\\n", "\n") : value;
  return unescaped.replace(/(^|\n)(\d+)\.\s*\n+\s*(?=\S)/g, "$1$2. ");
}
