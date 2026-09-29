// The two bits of markdown a review comment actually uses, rendered inline.
//
// The agent writes its findings as one prose line with `**emphasis**` and
// `` `identifiers` ``, which landed on screen as literal asterisks and
// backticks. MarkdownMessage is the right tool for a whole message and the
// wrong one here: it emits block elements with prose styling, which cannot
// go inside a heading or a badge row. This handles only bold and inline
// code, and passes anything else through as text.

import type { ReactNode } from "react";

/** Captures `**bold**` and `` `code` `` as whole tokens so split() keeps
 *  them, with the plain text between as the other entries. */
const TOKEN = /(\*\*[^*]+\*\*|`[^`]+`)/g;

export function InlineMarkdown({ text }: { text: string }) {
  const parts: ReactNode[] = text.split(TOKEN).map((part, i) => {
    if (part.startsWith("**") && part.endsWith("**") && part.length > 4) {
      return (
        <strong key={i} className="font-semibold">
          {part.slice(2, -2)}
        </strong>
      );
    }
    if (part.startsWith("`") && part.endsWith("`") && part.length > 2) {
      return (
        <code key={i} className="rounded bg-muted px-1 py-0.5 font-mono text-[0.9em]">
          {part.slice(1, -1)}
        </code>
      );
    }
    return part;
  });
  return <>{parts}</>;
}
