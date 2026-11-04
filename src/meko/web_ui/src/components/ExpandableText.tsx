// Long text cut to a few lines, with "Show more" to read all of it and "Show less" to cut it again.
// Short text shows whole, with no control.

import { useState } from "react";

// About how many characters fill the lines shown before "Show more".
const CHARS_PER_LINE = 90;

export function ExpandableText({ text, lines }: { text: string; lines: number }) {
  const [open, setOpen] = useState(false);
  const long = text.split("\n").length > lines || text.length > lines * CHARS_PER_LINE;
  return (
    <div>
      <p
        className="whitespace-pre-wrap text-sm"
        style={
          open || !long
            ? undefined
            : { display: "-webkit-box", WebkitLineClamp: lines, WebkitBoxOrient: "vertical", overflow: "hidden" }
        }
      >
        {text}
      </p>
      {long && (
        <button
          type="button"
          onClick={() => setOpen(!open)}
          className="mt-1 text-xs font-medium text-primary hover:underline"
        >
          {open ? "Show less" : "Show more"}
        </button>
      )}
    </div>
  );
}
