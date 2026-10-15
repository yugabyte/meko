import { describe, expect, it } from 'vitest';
import { DIAGRAM_FONT, diagramAlt, diagramSize, mermaidConfig, mermaidLook, svgDataUrl, zoomStep } from '../../src/web/mermaid.js';

describe('mermaid diagrams', () => {
  it('draws in strict mode with SVG labels in a system font, themed', () => {
    const light = mermaidConfig(false);
    expect(light).toMatchObject({ startOnLoad: false, securityLevel: 'strict', htmlLabels: false, theme: 'default', fontFamily: DIAGRAM_FONT });
    expect(mermaidConfig(true).theme).toBe('dark');
    // Markdown designs and Artifact frames share the look.
    expect(mermaidConfig(true)).toMatchObject(mermaidLook(true));
    expect(mermaidLook(false)).not.toHaveProperty('securityLevel');
    // No web font: an SVG drawn as an image can't load one.
    expect(DIAGRAM_FONT).not.toMatch(/Plex|Bricolage|JetBrains/);
  });

  it('reads the natural size from the viewBox', () => {
    expect(diagramSize('<svg xmlns="http://www.w3.org/2000/svg" width="100%" viewBox="-8 -8 640.5 312">')).toEqual({ width: 641, height: 312 });
    expect(diagramSize("<svg viewBox='0,0,10,20' style='max-width: 10px'>")).toEqual({ width: 10, height: 20 });
    expect(diagramSize('<svg width="100%">')).toEqual({ width: null, height: null });
    expect(diagramSize('<svg viewBox="0 0 0 20">')).toEqual({ width: null, height: null });
    expect(diagramSize('<svg viewBox="0 0 a b">')).toEqual({ width: null, height: null });
    // Only the outer <svg>'s own viewBox counts.
    expect(diagramSize('<svg><symbol viewBox="0 0 5 5"/></svg>')).toEqual({ width: null, height: null });
  });

  it.each([
    [1, 1, 1.25],
    [1, -1, 0.9],
    // A fitted diagram, between steps or a hair off one.
    [0.6, 1, 0.67],
    [0.6, -1, 0.5],
    [0.995, 1, 1.25],
    [1.005, -1, 0.9],
    [4, 1, null],
    [0.25, -1, null],
    [0.1, 1, 0.25],
  ] as const)('zooms from %s by %s to %s', (scale, dir, next) => {
    expect(zoomStep(scale, dir)).toBe(next);
  });

  it('makes an image URL that keeps the SVG intact', () => {
    const svg = '<svg xmlns="http://www.w3.org/2000/svg"><style>#a{fill:#f00}</style><text>50% "q" \'s\'\n</text></svg>';
    const url = svgDataUrl(svg);
    expect(url.startsWith('data:image/svg+xml;charset=utf-8,')).toBe(true);
    expect(url).not.toMatch(/[#%"\n<>](?![0-9A-F]{2})/);
    expect(decodeURIComponent(url.slice(url.indexOf(',') + 1))).toBe(svg);
  });

  it('describes a diagram by its accTitle or its kind', () => {
    expect(diagramAlt('flowchart LR\n  accTitle: Session lifecycle\n  a --> b')).toBe('Session lifecycle');
    expect(diagramAlt('graph TD\n a-->b')).toBe('Flowchart');
    expect(diagramAlt('flowchart-v2 LR')).toBe('Flowchart');
    expect(diagramAlt('stateDiagram-v2\n [*] --> A')).toBe('State diagram');
    expect(diagramAlt('mindmap\n root')).toBe('Mindmap diagram');
    expect(diagramAlt('gitGraph\n commit')).toBe('Git graph');
    expect(diagramAlt('gantt\n title x')).toBe('Gantt chart');
    expect(diagramAlt('---\ntitle: Hello\n---\n%%{init: {"theme":"dark"}}%%\n%% a comment\nerDiagram\n A ||--o{ B : has')).toBe('Entity relationship diagram');
    expect(diagramAlt('%% only a comment')).toBe('Diagram');
  });
});
