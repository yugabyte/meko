import { marked } from 'marked';
import { describe, expect, it } from 'vitest';
import { renderMarkdown } from '../../src/web/markdown.js';

describe('markdown designs', () => {
  it('takes each mermaid fence out into a numbered slot', () => {
    const md = '# Flow\n\n```mermaid\nflowchart LR\n  a --> b\n```\n\nText\n\n```mermaid\nsequenceDiagram\n  A->>B: hi\n```\n';
    const { html, diagrams } = renderMarkdown(md);
    expect(diagrams).toEqual(['flowchart LR\n  a --> b', 'sequenceDiagram\n  A->>B: hi']);
    expect(html).toContain('<div class="mermaid-slot" data-mermaid="0"></div>');
    expect(html).toContain('<div class="mermaid-slot" data-mermaid="1"></div>');
    expect(html.indexOf('data-mermaid="0"')).toBeLessThan(html.indexOf('data-mermaid="1"'));
    expect(html).not.toContain('flowchart');
    expect(html).toContain('<h1>Flow</h1>');
  });

  it('finds fences nested in lists and quotes, and ones with more in the info string', () => {
    const md = '- step\n\n  ```mermaid\n  graph TD\n  ```\n\n> ```Mermaid title="x"\n> pie\n> ```\n';
    const { html, diagrams } = renderMarkdown(md);
    expect(diagrams).toEqual(['graph TD', 'pie']);
    expect(html.match(/mermaid-slot/g)).toHaveLength(2);
  });

  it('leaves other code alone', () => {
    const md = '```js\nconst mermaid = 1;\n```\n\n    mermaid indented\n\n```\nplain\n```\n\n```mermaidish\nx\n```\n';
    const { html, diagrams } = renderMarkdown(md);
    expect(diagrams).toEqual([]);
    expect(html).toBe(marked.parse(md, { async: false }));
  });

  it('renders a design without diagrams as before', () => {
    const md = '# Design\n\n- [x] done\n- [ ] todo\n\n| a | b |\n|---|---|\n| 1 | 2 |\n\n[link](https://example.com) <b>raw</b>\n';
    expect(renderMarkdown(md)).toEqual({ html: marked.parse(md, { async: false }), diagrams: [] });
  });

  it('keeps diagram source text out of the HTML, even when it looks like HTML', () => {
    const { html, diagrams } = renderMarkdown('```mermaid\ngraph TD\n  a["<img src=x onerror=alert(1)>"]\n```\n');
    expect(html).not.toContain('<img');
    expect(diagrams[0]).toContain('<img src=x onerror=alert(1)>');
  });
});
