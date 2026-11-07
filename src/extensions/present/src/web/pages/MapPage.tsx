// The story map: scenes on a canvas, edges between them, edited by hand (drag, connect, rename,
// delete) or by asking Claude, and a walk-through that glides along the default path and stops at
// each fork.

import {
  Background, Controls, Handle, MarkerType, MiniMap, Position, ReactFlow, ReactFlowProvider, useReactFlow,
  type Connection, type Edge as FlowEdge, type Node, type NodeProps,
} from '@xyflow/react';
import '@xyflow/react/dist/style.css';
import { Hammer, Map as MapIcon, Play, Plus, Square, Trash2 } from 'lucide-react';
import { useCallback, useEffect, useMemo, useState } from 'react';
import { choices, newId, type Deck, type Edge, type EdgeKind, type Scene } from '../../shared/deck.js';
import { afterScene } from '../../shared/run.js';
import type { AskScope } from '../../shared/types.js';
import { api } from '../api.js';
import { AskBar, ChangeSetPanel, DeckBar, JobActivity } from '../components/DeckChrome.js';
import { Button, cx, ErrorLine, inputClass, Label, Pill } from '../components/ui.js';
import { navigate } from '../router.js';
import { useDeck, type DeckHandle } from '../useDeck.js';

type SceneData = { scene: Scene; start: boolean; walking: boolean; built: number };

function SceneNode({ data, selected }: NodeProps<Node<SceneData>>) {
  const s = data.scene;
  return (
    <div className={cx('w-40 rounded-lg border bg-card px-2 py-1.5 text-left shadow-sm', s.fork && 'border-info', s.detour && 'border-dashed', selected && 'outline-2 outline-primary', data.walking && 'ring-2 ring-primary ring-offset-2')}>
      <Handle type="target" position={Position.Left} />
      <div className="truncate text-xs font-semibold">{s.fork && <span className="text-info">◆ </span>}{s.title}</div>
      <div className="truncate text-[10px] text-muted-foreground">
        {data.start ? 'start · ' : ''}{s.fork ? 'fork · ' : s.detour ? 'detour · ' : ''}{s.slides.length} slide{s.slides.length === 1 ? '' : 's'}
      </div>
      {s.slides.length > 0 && (
        <div className="mt-1 flex gap-0.5">
          {s.slides.slice(0, 6).map((id, i) => <i key={id} className={cx('block h-2.5 w-4 rounded-[2px]', i < data.built ? 'bg-slide' : 'bg-muted')} />)}
        </div>
      )}
      <Handle type="source" position={Position.Right} />
    </div>
  );
}
const nodeTypes = { scene: SceneNode };

/** The default path from the start: next edges, else a fork's first choice, until it ends or repeats. */
export function defaultPath(deck: Deck, from = deck.start): string[] {
  const path: string[] = [];
  let at: string | null = from;
  while (at && !path.includes(at) && deck.scenes[at]) {
    path.push(at);
    at = deck.scenes[at]!.fork ? (choices(deck, at)[0]?.to ?? null) : afterScene(deck, at);
  }
  return path;
}

function edgeStyle(e: Edge): Partial<FlowEdge> {
  return {
    label: e.kind === 'choice' ? `${e.key ?? ''} ${e.label ?? ''}`.trim() : e.kind === 'return' ? '↩ return' : undefined,
    style: e.kind === 'return' ? { strokeDasharray: '5 4' } : undefined,
    markerEnd: { type: MarkerType.ArrowClosed },
  };
}

function ScenePanel({ h, scene }: { h: DeckHandle; scene: Scene }) {
  const deck = h.state!.deck;
  const [title, setTitle] = useState(scene.title);
  const [intent, setIntent] = useState(scene.intent);
  useEffect(() => (setTitle(scene.title), setIntent(scene.intent)), [scene.id, scene.title, scene.intent]);
  const patch = (p: Partial<Scene>) => void h.edit([{ op: 'updateScene', id: scene.id, patch: p }]);
  const out = deck.edges.filter((e) => e.from === scene.id);
  // A fork's outgoing edges are its choices: turning one on makes each next edge a choice, and off makes them next again.
  const toggleFork = (on: boolean) => void h.edit([
    { op: 'updateScene', id: scene.id, patch: { fork: on } },
    ...out.filter((e) => (on ? e.kind === 'next' : e.kind === 'choice')).map((e, i) => ({
      op: 'updateEdge' as const, id: e.id,
      patch: on ? { kind: 'choice' as const, key: String(i + 1), label: deck.scenes[e.to]?.title ?? `Option ${i + 1}` } : { kind: 'next' as const, key: undefined, label: undefined },
    })),
  ]);
  return (
    <div className="space-y-3">
      <Label>Scene</Label>
      <label className="block space-y-1 text-sm"><span>Title</span>
        <input className={inputClass} value={title} onChange={(e) => setTitle(e.target.value)} onBlur={() => title.trim() && title !== scene.title && patch({ title: title.trim() })} />
      </label>
      <label className="block space-y-1 text-sm"><span>What it covers</span>
        <textarea className={cx(inputClass, 'min-h-20')} value={intent} onChange={(e) => setIntent(e.target.value)} onBlur={() => intent !== scene.intent && patch({ intent })} />
      </label>
      <div className="flex flex-wrap gap-3 text-sm">
        <label className="flex items-center gap-1.5"><input type="checkbox" checked={!!scene.fork} onChange={(e) => toggleFork(e.target.checked)} />Fork</label>
        <label className="flex items-center gap-1.5"><input type="checkbox" checked={!!scene.detour} onChange={(e) => patch({ detour: e.target.checked })} />Detour</label>
      </div>
      <div className="space-y-1 text-sm">
        <span className="text-muted-foreground">Out</span>
        {out.length ? out.map((e) => <div key={e.id}>{e.kind} → <b>{deck.scenes[e.to]?.title}</b>{e.label ? ` (${e.key} ${e.label})` : ''}</div>) : <div className="text-muted-foreground">Nothing: drag from its right edge to another scene.</div>}
      </div>
      <div className="flex flex-wrap gap-1.5">
        <Button onClick={() => navigate({ view: 'slides', deck: h.state!.id, slide: scene.slides[0] ?? null })}>Slides</Button>
        <Button onClick={() => void h.act(() => api.startJob(h.state!.id, { kind: 'build', scenes: [scene.id] }))}><Hammer className="size-4" />Build</Button>
        {deck.start !== scene.id && <Button onClick={() => void h.edit([{ op: 'setStart', scene: scene.id }])}>Make start</Button>}
        {deck.start !== scene.id && <Button variant="danger" onClick={() => void h.edit([{ op: 'removeScene', id: scene.id }])}><Trash2 className="size-4" />Delete</Button>}
      </div>
    </div>
  );
}

function EdgePanel({ h, edge }: { h: DeckHandle; edge: Edge }) {
  const deck = h.state!.deck;
  const [label, setLabel] = useState(edge.label ?? '');
  useEffect(() => setLabel(edge.label ?? ''), [edge.id, edge.label]);
  const patch = (p: Partial<Edge>) => void h.edit([{ op: 'updateEdge', id: edge.id, patch: p }]);
  return (
    <div className="space-y-3">
      <Label>Edge</Label>
      <p className="text-sm">{deck.scenes[edge.from]?.title} → {deck.scenes[edge.to]?.title}</p>
      <label className="block space-y-1 text-sm"><span>Kind</span>
        <select className={inputClass} value={edge.kind} onChange={(e) => patch({ kind: e.target.value as EdgeKind })}>
          <option value="next">Next</option>
          <option value="choice" disabled={!deck.scenes[edge.from]?.fork}>Choice (from a fork)</option>
          <option value="return">Return</option>
        </select>
      </label>
      {edge.kind === 'choice' && (
        <>
          <label className="block space-y-1 text-sm"><span>Option, as the room sees it</span>
            <input className={inputClass} value={label} onChange={(e) => setLabel(e.target.value)} onBlur={() => label !== (edge.label ?? '') && patch({ label })} />
          </label>
          <label className="block space-y-1 text-sm"><span>Key</span>
            <select className={inputClass} value={edge.key ?? ''} onChange={(e) => patch({ key: e.target.value })}>
              {['1', '2', '3', '4', '5', '6', '7', '8', '9'].map((k) => <option key={k}>{k}</option>)}
            </select>
          </label>
        </>
      )}
      <Button variant="danger" onClick={() => void h.edit([{ op: 'disconnect', id: edge.id }])}><Trash2 className="size-4" />Delete</Button>
    </div>
  );
}

function Canvas({ h }: { h: DeckHandle }) {
  const deck = h.state!.deck;
  const flow = useReactFlow();
  const [sel, setSel] = useState<{ node?: string; edge?: string }>({});
  const [walk, setWalk] = useState<{ path: string[]; at: number } | null>(null);
  const [scope, setScope] = useState<AskScope>({ kind: 'deck' });

  const walking = walk ? walk.path[walk.at] : undefined;
  const nodes: Node<SceneData>[] = useMemo(() => Object.values(deck.scenes).map((s, i) => ({
    id: s.id, type: 'scene', position: { x: s.x ?? i * 220, y: s.y ?? 0 },
    data: { scene: s, start: deck.start === s.id, walking: walking === s.id, built: s.slides.filter((id) => (deck.slides[id]?.variants[deck.variants[0]!]?.blocks.length ?? 0) > 0).length },
    selected: sel.node === s.id,
  })), [deck, sel.node, walking]);
  const edges: FlowEdge[] = useMemo(() => deck.edges.map((e) => ({ id: e.id, source: e.from, target: e.to, selected: sel.edge === e.id, ...edgeStyle(e) })), [deck.edges, sel.edge]);

  const onConnect = useCallback((c: Connection) => {
    if (!c.source || !c.target || c.source === c.target) return;
    const fork = !!deck.scenes[c.source]?.fork;
    const used = new Set(choices(deck, c.source).map((e) => e.key));
    const key = ['1', '2', '3', '4', '5', '6', '7', '8', '9'].find((k) => !used.has(k)) ?? '9';
    const edge: Edge = { id: newId('e'), from: c.source, to: c.target, kind: fork ? 'choice' : 'next', ...(fork ? { label: deck.scenes[c.target]?.title ?? 'Option', key } : {}) };
    void h.edit([{ op: 'connect', edge }]).then((ok) => ok && setSel({ edge: edge.id }));
  }, [deck, h]);

  const addScene = () => {
    const id = newId('scene');
    const xs = Object.values(deck.scenes).map((s) => s.x ?? 0);
    void h.edit([{ op: 'addScene', scene: { id, title: 'New scene', intent: '', slides: [], detour: true, x: (xs.length ? Math.max(...xs) : 0) + 220, y: 160 } }])
      .then((ok) => ok && setSel({ node: id }));
  };

  // The walk-through: arrows step, number keys take a fork's branch, Escape stops.
  useEffect(() => {
    if (!walk) return;
    const id = walk.path[walk.at];
    const s = id ? deck.scenes[id] : undefined;
    if (s) void flow.setCenter((s.x ?? 0) + 80, (s.y ?? 0) + 30, { zoom: 1.2, duration: 600 });
  }, [walk, deck.scenes, flow]);
  useEffect(() => {
    if (!walk) return;
    const on = (e: KeyboardEvent) => {
      if ((e.target as HTMLElement).closest('input, textarea, select')) return;
      if (e.key === 'Escape') setWalk(null);
      else if (e.key === 'ArrowRight' || e.key === ' ') setWalk((w) => w && { ...w, at: Math.min(w.at + 1, w.path.length - 1) });
      else if (e.key === 'ArrowLeft') setWalk((w) => w && { ...w, at: Math.max(w.at - 1, 0) });
      else if (/^[1-9]$/.test(e.key)) {
        const here = walk.path[walk.at]!;
        const to = choices(deck, here).find((c) => c.key === e.key)?.to;
        if (to) setWalk({ path: [...walk.path.slice(0, walk.at + 1), ...defaultPath(deck, to)], at: walk.at + 1 });
      } else return;
      e.preventDefault();
    };
    addEventListener('keydown', on);
    return () => removeEventListener('keydown', on);
  }, [walk, deck]);

  const onlyOpening = Object.keys(deck.scenes).length === 1;
  const mapRunning = h.jobs.some((j) => j.kind === 'map' && (j.status === 'running' || j.status === 'queued'));
  const selScene = sel.node ? deck.scenes[sel.node] : undefined;
  const selEdge = sel.edge ? deck.edges.find((e) => e.id === sel.edge) : undefined;
  const walkScene = walking ? deck.scenes[walking] : undefined;

  return (
    <div className="flex h-full flex-col">
      <DeckBar h={h} view="map">
        {walk ? <Button onClick={() => setWalk(null)}><Square className="size-4" />Stop walking</Button>
          : <Button onClick={() => setWalk({ path: defaultPath(deck), at: 0 })}><Play className="size-4" />Walk through</Button>}
        <Button onClick={addScene}><Plus className="size-4" />Scene</Button>
        {onlyOpening && <Button disabled={mapRunning} onClick={() => void h.act(() => api.startJob(deck && h.state!.id, { kind: 'map' }))}><MapIcon className="size-4" />Draft story map</Button>}
        <Button variant="primary" disabled={onlyOpening} onClick={() => void h.act(() => api.startJob(h.state!.id, { kind: 'build' }))}><Hammer className="size-4" />Build slides</Button>
      </DeckBar>
      <div className="px-3 pt-2"><ErrorLine error={h.error} /></div>
      <div className="grid min-h-0 flex-1 md:grid-cols-[minmax(0,1fr)_17rem]">
        <div className="relative min-h-80">
          <ReactFlow
            nodes={nodes} edges={edges} nodeTypes={nodeTypes} fitView proOptions={{ hideAttribution: true }}
            onNodeDragStop={(_e, n) => void h.edit([{ op: 'updateScene', id: n.id, patch: { x: Math.round(n.position.x), y: Math.round(n.position.y) } }])}
            onConnect={onConnect}
            onNodeClick={(_e, n) => (setSel({ node: n.id }), setScope({ kind: 'scene', scene: n.id }))}
            onEdgeClick={(_e, ed) => setSel({ edge: ed.id })}
            onPaneClick={() => (setSel({}), setScope({ kind: 'deck' }))}
            onNodeDoubleClick={(_e, n) => navigate({ view: 'slides', deck: h.state!.id, slide: deck.scenes[n.id]?.slides[0] ?? null })}
            deleteKeyCode={null}
          >
            <Background gap={16} />
            <MiniMap pannable zoomable />
            <Controls showInteractive={false} />
          </ReactFlow>
          {walk && walkScene && (
            <div className="absolute bottom-3 left-3 right-3 z-10 mx-auto max-w-xl rounded-lg border bg-card p-3 shadow-lg">
              <div className="flex items-center gap-2"><Pill tone="info">{walk.at + 1} of {walk.path.length}</Pill><b className="truncate">{walkScene.title}</b></div>
              <p className="mt-1 text-sm text-muted-foreground">{walkScene.intent || 'No intent written yet.'}</p>
              {walkScene.fork && <div className="mt-2 flex flex-wrap gap-1.5">{choices(deck, walkScene.id).map((c) => <Pill key={c.id}>{c.key} {c.label}</Pill>)}</div>}
              <p className="mt-2 text-xs text-muted-foreground">← → to step{walkScene.fork ? ', a number to take that branch' : ''}, Esc to stop.</p>
            </div>
          )}
        </div>
        <aside className="space-y-4 overflow-auto border-l bg-card p-3" aria-label="Selection">
          {selScene ? <ScenePanel h={h} scene={selScene} /> : selEdge ? <EdgePanel h={h} edge={selEdge} /> : (
            <div className="space-y-2 text-sm text-muted-foreground">
              <p>Click a scene or an edge to edit it. Drag from a scene's right edge to another to connect them; from a fork, that's a choice.</p>
              <p>Double-click a scene to open its slides.</p>
            </div>
          )}
        </aside>
      </div>
      {h.changeSets.length > 0 && <div className="max-h-[45%] overflow-auto border-t bg-muted/40 p-3"><ChangeSetPanel key={h.changeSets[0]!.id} h={h} cs={h.changeSets[0]!} /></div>}
      <JobActivity h={h} />
      <AskBar h={h} scope={scope} onScope={setScope}
        scopes={selScene ? [{ kind: 'scene', scene: selScene.id }, { kind: 'deck' }] : [{ kind: 'deck' }]}
        placeholder="Add a fork after xCluster on HA vs. cost, and fold AMP into one scene" />
    </div>
  );
}

export function MapPage({ deckId }: { deckId: string }) {
  const h = useDeck(deckId);
  if (!h.state) return <main className="p-6">{h.error ? <ErrorLine error={h.error} /> : <p className="text-muted-foreground">Loading the deck…</p>}</main>;
  return <ReactFlowProvider><Canvas h={h} /></ReactFlowProvider>;
}
