// A markdown design turned into HTML, with its Mermaid diagrams taken out to be drawn apart.
//
// Each ```mermaid fence becomes an empty slot (`<div class="mermaid-slot" data-mermaid="N">`)
// and its source goes to `diagrams[N]`; `MarkdownView` draws the diagram into the slot. The HTML
// still has to be sanitized: this module has no DOM, so it can be tested in Node.

import { Marked } from 'marked';

export interface RenderedMarkdown {
  /** Unsanitized HTML. */
  html: string;
  /** Each diagram's source, by its slot's `data-mermaid` index. */
  diagrams: string[];
}

/** A fence's language: the first word of its info string (```mermaid title → mermaid). */
function fenceLang(lang: string | undefined): string {
  return (lang ?? '').trim().split(/\s+/)[0].toLowerCase();
}

export function renderMarkdown(markdown: string): RenderedMarkdown {
  const diagrams: string[] = [];
  const md = new Marked({
    renderer: {
      code({ text, lang, codeBlockStyle }) {
        // false falls back to marked's own code block.
        if (codeBlockStyle === 'indented' || fenceLang(lang) !== 'mermaid') return false;
        diagrams.push(text);
        return `<div class="mermaid-slot" data-mermaid="${diagrams.length - 1}"></div>\n`;
      },
    },
  });
  return { html: md.parse(markdown, { async: false }) as string, diagrams };
}
