// What Present tells Claude, and the shape it must answer in: a summary, a list of deck operations
// (src/shared/ops.ts), and the files it made. Present checks the operations against the deck; a
// bad answer is sent back once with what was wrong.

import { BLOCK_TYPES, LAYOUTS, type Deck, type Scene } from '../shared/deck.js';
import { OP_NAMES } from '../shared/ops.js';
import type { AskScope } from '../shared/types.js';

/** The answer every job asks for. */
export const ANSWER_SCHEMA = {
  type: 'object',
  required: ['summary', 'ops'],
  properties: {
    summary: { type: 'string', description: 'One or two sentences on what changed and why.' },
    ops: {
      type: 'array',
      items: {
        type: 'object',
        required: ['op'],
        properties: { op: { type: 'string', enum: OP_NAMES } },
        additionalProperties: true,
      },
    },
    files: {
      type: 'array',
      description: 'Files you made in ./assets for the deck to use (diagrams, charts, images).',
      items: {
        type: 'object',
        required: ['name', 'path', 'type'],
        properties: { name: { type: 'string' }, path: { type: 'string' }, type: { type: 'string' } },
      },
    },
  },
} as const;

export interface Answer {
  summary: string;
  ops: unknown[];
  files: { name: string; path: string; type: string }[];
}

/** The answer, or what's wrong with its shape. */
export function readAnswer(x: unknown): Answer | string {
  if (!x || typeof x !== 'object') return 'The answer wasn\'t an object.';
  const a = x as Record<string, unknown>;
  if (typeof a.summary !== 'string' || !Array.isArray(a.ops)) return 'The answer needs a summary and a list of ops.';
  const files = Array.isArray(a.files) ? a.files : [];
  for (const f of files as Record<string, unknown>[]) {
    if (typeof f?.name !== 'string' || typeof f.path !== 'string' || typeof f.type !== 'string') return 'Each file needs a name, a path and a type.';
  }
  return { summary: a.summary, ops: a.ops, files: files as Answer['files'] };
}

export const SYSTEM_PROMPT = `You are working inside Present, which builds interactive, branching presentations from Meko datapacks.

A deck is a graph of scenes. A scene is a short, linear run of slides on one topic. Edges join scenes:
- "next": the talk goes on to the next scene.
- "choice": from a fork scene (fork: true), one labelled option per edge, each with a key "1" to "9". A fork's last slide asks the room a question; its choices are the answers.
- "return": back to a hub scene, such as a product menu.
A detour scene (detour: true) has no incoming edges: the presenter jumps to it and comes back.
Every scene but a detour must be reachable from the start scene.

A slide has a layout (${LAYOUTS.join(', ')}), a variant for each of the deck's variants (the first, usually "overview", is required; "deep" goes further for technical rooms), speaker notes, and sources.
Blocks (${BLOCK_TYPES.join(', ')}):
- {"type":"heading","text"} {"type":"text","markdown"} {"type":"bullets","items":[...],"build":true|false}
- {"type":"metric","value","label"} {"type":"quote","text","by"} {"type":"table","header":[...],"rows":[[...]]}
- {"type":"image","asset","alt"} {"type":"video","asset","caption"}: asset is a name in the deck's assets
- {"type":"diagram","mermaid"}: Mermaid text
- {"type":"animation","ref":"<animation datapack>:<name>","params":{...}}
- {"type":"code","code","language"}
Keep slides short: a heading and four bullets at most; put detail in the "deep" variant and the notes.

Answer only with operations on the deck (Present applies them, and refuses the lot if the deck would break):
${OP_NAMES.map((n) => `- ${n}`).join('\n')}
Shapes: addScene {scene:{id,title,intent,slides:[],fork?,detour?,x?,y?}}; updateScene {id,patch}; removeScene {id};
connect {edge:{id,from,to,kind,label?,key?}}; disconnect {id}; updateEdge {id,patch};
addSlide {scene,slide:{id,layout,variants:{overview:{blocks:[...]}},notes,sources:[]},index?}; updateSlide {slide} (the whole slide);
moveSlide {id,scene,index}; removeSlide {id}; setStart {scene}; setTitle {title}; setVariants {variants}; addAsset (Present adds the files you list).
Make ids short and unique: scenes like "xcluster", slides "s-<scene>-<n>", edges "e-<from>-<to>".
Place scenes on the map with x and y (about 260 apart, left to right in talk order, branches stacked).

Rules:
- Every claim on a slide needs a source: {"kind":"memory"|"knowledge","datapack":"<name>","id":"<id Meko gave>","excerpt":"..."} or {"kind":"web","url":"..."}. Never invent facts, numbers or ids; leave a claim out rather than guess.
- Read the deck's content datapacks with the Meko tools (context_search, memory_search, knowledgebase_search with each datapack's id). You may use any other tool too: the web, files, the user's other MCP servers.
- Work only in the current folder. To add a picture (an SVG diagram, a chart), write it under ./assets and list it in "files" with the name blocks use; never change anything outside this folder.
- Don't rewrite or remove a slide marked handEdited unless the instruction asks for exactly that.
- The deck you're given is the current one, and it wins over what you remember from earlier in this session.`;

function outline(deck: Deck): unknown {
  return {
    title: deck.title,
    variants: deck.variants,
    start: deck.start,
    scenes: Object.values(deck.scenes).map((s: Scene) => ({
      id: s.id, title: s.title, intent: s.intent, fork: !!s.fork, detour: !!s.detour,
      slides: s.slides.map((id) => ({ id, handEdited: !!deck.slides[id]?.handEdited })),
    })),
    edges: deck.edges,
    assets: Object.keys(deck.assets),
  };
}

export interface DatapackRef {
  id: string;
  name: string;
}

function context(deck: Deck, packs: { content: DatapackRef[]; animations: DatapackRef[] }) {
  return {
    brief: deck.brief,
    contentDatapacks: packs.content,
    animationDatapacks: packs.animations,
    template: deck.sources.template ?? 'the built-in default template',
  };
}

export function mapMessage(deck: Deck, packs: { content: DatapackRef[]; animations: DatapackRef[] }, note?: string): string {
  return [
    note
      ? `Redraw the story map of this deck. The author says: ${note}`
      : `Draft the story map of this deck: its scenes, forks, choices, returns and detours, as the brief's shape asks (${deck.brief.shape}). Give each scene a title and an intent (what it covers), and a fork a question slide whose choices are the answers. Leave the other slides for later: they're built scene by scene. Keep the opening scene as the start.`,
    'Context:', JSON.stringify(context(deck, packs)),
    'The deck now:', JSON.stringify(outline(deck)),
  ].join('\n\n');
}

export function buildMessage(deck: Deck, packs: { content: DatapackRef[]; animations: DatapackRef[] }, scene: string): string {
  const s = deck.scenes[scene]!;
  const neighbours = deck.edges.filter((e) => e.from === scene || e.to === scene);
  return [
    `Build the slides of scene "${s.title}" (id ${scene}): ${s.intent || 'see its title'}. Replace its slides that aren't hand-edited (removeSlide, then addSlide), give every slide the "overview" variant and a "deep" one where the room may want more, speaker notes, and sources. Suggest likely audience questions as detour scenes only if the deck has none for them.`,
    'Context:', JSON.stringify(context(deck, packs)),
    'Its neighbours:', JSON.stringify(neighbours),
    'The scene\'s slides now:', JSON.stringify(s.slides.map((id) => deck.slides[id])),
    'The deck now:', JSON.stringify(outline(deck)),
  ].join('\n\n');
}

export function askMessage(deck: Deck, packs: { content: DatapackRef[]; animations: DatapackRef[] }, instruction: string, scope: AskScope): string {
  const what = scope.kind === 'block' ? `block ${scope.block} (counting from 0) of slide ${scope.slide}'s "${scope.variant ?? deck.variants[0]}" variant`
    : scope.kind === 'slide' ? `slide ${scope.slide}` : scope.kind === 'scene' ? `scene ${scope.scene}` : 'the whole deck';
  const inScope = scope.kind === 'deck' ? null
    : scope.kind === 'scene' ? deck.scenes[scope.scene ?? '']?.slides.map((id) => deck.slides[id])
      : [deck.slides[scope.slide ?? '']];
  return [
    `The author asks, about ${what}: ${instruction}`,
    'Change only what that needs. Your operations are shown to the author as a change set to apply or not.',
    'Context:', JSON.stringify(context(deck, packs)),
    ...(inScope ? ['In scope:', JSON.stringify(inScope)] : []),
    'The deck now:', JSON.stringify(outline(deck)),
  ].join('\n\n');
}

export function retryMessage(problems: string[]): string {
  return `Present couldn't use that answer:\n${problems.map((p) => `- ${p}`).join('\n')}\nAnswer again with operations that fix this, against the same deck.`;
}
