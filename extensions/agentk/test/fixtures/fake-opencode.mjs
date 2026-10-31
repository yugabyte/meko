#!/usr/bin/env node
// Stand-in for the `opencode` CLI (OpenCode 2) in integration tests. A session loads the plugin
// folders its OPENCODE_CONFIG_CONTENT names (AgentK's, for real: their default `{ id, setup }`)
// and drives their hooks the way OpenCode 2.1 does: the session `prompt` hook for a prompt, tool
// `execute.before` (whose throw denies the call) and `.after` around a tool, the session `context`
// hook before a response, and the `session.created`, `permission.asked`/`.replied`,
// `session.step.started`/`.ended`, `session.execution.started` and `.succeeded` (or `.interrupted`)
// and `shell.created`/`.exited`/`.deleted` events, as `{ id, type, data }`.
// Each stdin line is a prompt. Like OpenCode 2 under AgentK, every command but `--version` and
// `upgrade` needs `--standalone`. Everything it does is recorded in $FAKE_OPENCODE_DIR.
import { randomBytes } from 'node:crypto';
import { appendFileSync, existsSync, mkdirSync, readFileSync, writeFileSync } from 'node:fs';
import { isAbsolute, join } from 'node:path';
import { pathToFileURL } from 'node:url';

const args = process.argv.slice(2);
const dir = process.env.FAKE_OPENCODE_DIR;
const log = (file, v) => appendFileSync(join(dir, file), JSON.stringify(v) + '\n');
const sleep = (ms) => new Promise((r) => setTimeout(r, ms));

// The version a test put in `version`, else 2.1.4. It writes nothing: a version read still going
// when a test ends (Core.stop doesn't wait for one) mustn't make the folder its cleanup removes.
if (args[0] === '--version') {
  const v = join(dir, 'version');
  process.stdout.write(existsSync(v) ? readFileSync(v, 'utf8') : '2.1.4\n');
  process.exit(0);
}
mkdirSync(dir, { recursive: true });
if (args[0] === 'upgrade') {
  log('upgrade.jsonl', { args });
  process.exit(0);
}
// Without it, OpenCode 2 runs the command on its shared background service, which never sees the
// variables AgentK gives it.
if (!args.includes('--standalone')) {
  process.stderr.write(`fake opencode: ${args.join(' ')} would run on the shared background service (no --standalone)\n`);
  process.exit(2);
}
if (args[0] === 'session' && args[1] === 'delete') {
  log('deleted.jsonl', { id: args.at(-1), cwd: process.cwd(), xdg: process.env.XDG_CONFIG_HOME ?? null });
  process.exit(0);
}
// Like `opencode run --format json` (Ask AgentK): records what it got, with the global config it
// would read ($XDG_CONFIG_HOME/opencode), and prints `ask-reply.jsonl` from the fake's dir; exits 1
// after an error event, as OpenCode does.
if (args[0] === 'run') {
  let input = '';
  for await (const chunk of process.stdin) input += chunk;
  const cfg = join(process.env.XDG_CONFIG_HOME || join(process.env.HOME, '.config'), 'opencode', 'opencode.json');
  const env = Object.fromEntries(['XDG_CONFIG_HOME', 'OPENCODE_DISABLE_PROJECT_CONFIG', 'OPENCODE_DISABLE_CLAUDE_CODE'].map((k) => [k, process.env[k] ?? null]));
  writeFileSync(join(dir, 'ask-input.json'), JSON.stringify({ args, cwd: process.cwd(), input, env, config: existsSync(cfg) ? JSON.parse(readFileSync(cfg, 'utf8')) : null }));
  const out = readFileSync(join(dir, 'ask-reply.jsonl'), 'utf8');
  process.stdout.write(out, () => process.exit(out.includes('"type":"error"') ? 1 : 0));
  await new Promise(() => undefined);
}

const opt = (name) => args.find((a) => a.startsWith(`--${name}=`))?.slice(name.length + 3) ?? null;
const known = join(dir, 'sessions.json');
const sessions = existsSync(known) ? JSON.parse(readFileSync(known, 'utf8')) : [];
const resumed = opt('session');
// OpenCode 2 starts an empty session under an id it doesn't have; AgentK resumes only ones it has.
if (resumed && !sessions.includes(resumed)) {
  process.stderr.write(`fake opencode: resumed ${resumed}, which it never made\n`);
  process.exit(1);
}
const sessionId = resumed ?? `ses_${randomBytes(6).toString('hex')}`;
if (!resumed) writeFileSync(known, JSON.stringify([...sessions, sessionId]));
const config = JSON.parse(process.env.OPENCODE_CONFIG_CONTENT || '{}');
log('argv.jsonl', {
  args, cwd: process.cwd(), config,
  env: Object.fromEntries(['MEKO_AGENT_ID', 'AK_SESSION', 'AK_OPENCODE_SOURCE', 'OPENCODE_CONFIG', 'OPENCODE_DISABLE_PROJECT_CONFIG'].map((k) => [k, process.env[k] ?? null])),
});

// The plugins' hooks, by domain and name, and the event streams they subscribed to.
const hooks = { session: {}, tool: {} };
const register = (domain) => async (name, cb) => {
  (hooks[domain][name] ??= []).push(cb);
  return { dispose: async () => undefined };
};
const parents = new Map();
const streams = new Set();
let evn = 0;
const ctx = {
  location: { directory: process.cwd() },
  session: { hook: register('session'), get: async ({ sessionID }) => ({ id: sessionID, parentID: parents.get(sessionID) }) },
  tool: { hook: register('tool') },
  event: {
    subscribe() {
      const queue = [];
      let wake = null;
      const push = (ev) => {
        queue.push(ev);
        wake?.();
      };
      streams.add(push);
      return {
        [Symbol.asyncIterator]: () => ({
          next: async () => {
            while (!queue.length) await new Promise((r) => (wake = r));
            wake = null;
            return { value: queue.shift(), done: false };
          },
          return: async () => {
            streams.delete(push);
            return { value: undefined, done: true };
          },
        }),
      };
    },
  },
};
// Like OpenCode 2: a plugin path is a folder, whose `server` (or `index`) module exports the plugin.
for (const spec of config.plugin ?? []) {
  const mod = await import(pathToFileURL(join(spec, existsSync(join(spec, 'server.js')) ? 'server.js' : 'index.js')).href);
  const plugin = mod.default;
  if (!plugin || typeof plugin.id !== 'string' || typeof plugin.setup !== 'function') {
    process.stderr.write(`fake opencode: ${spec} doesn't export a default { id, setup }\n`);
    process.exit(1);
  }
  await plugin.setup(ctx);
}
const call = async (domain, name, input) => {
  for (const h of hooks[domain][name] ?? []) await h(input);
};
const event = async (type, data) => {
  const ev = { id: `evt_${++evn}`, created: Date.now(), type, data };
  for (const push of streams) push(ev);
  await sleep(10);
  return ev;
};
// An event handed over again, as the stream does after it reconnects.
const replay = async (ev) => {
  for (const push of streams) push(ev);
  await sleep(10);
};

let n = 0;
let pending = null;
async function respond(session, model = 'claude-sonnet-4-5') {
  n++;
  const system = [];
  await call('session', 'context', { sessionID: session, agent: 'build', model: { id: model, providerID: 'anthropic' }, system, messages: [], options: {}, tools: {} });
  if (session === sessionId) log('system.jsonl', { system, instructions: (config.instructions ?? []).map((f) => readFileSync(f, 'utf8')) });
  const assistantMessageID = `msg_${sessionId}_${n}`;
  await event('session.step.started', { sessionID: session, assistantMessageID, agent: 'build', model: { id: model, providerID: 'anthropic' }, started: Date.now() });
  const ended = { sessionID: session, assistantMessageID, finish: 'stop', cost: 0, tokens: { input: 100, output: 20, reasoning: 5, cache: { read: 1000, write: 50 } } };
  const ev = { id: `evt_${++evn}`, created: Date.now(), type: 'session.step.ended', data: ended };
  // The stream hands an event over again after it reconnects: logged once.
  for (const push of streams) push(ev);
  for (const push of streams) push(ev);
  await sleep(10);
}

async function finish(interrupted = false) {
  await respond(sessionId);
  await sleep(100);
  await event(interrupted ? 'session.execution.interrupted' : 'session.execution.succeeded', { sessionID: sessionId });
}

/** A call to Meko's memory_search that takes `ms` (its id shared by calls from one code-mode run); Meko finds one memory, or nothing when `empty`. */
async function mekoCall(id, query, ms, empty) {
  const c = { tool: 'meko_memory_search', sessionID: sessionId, agent: 'build', messageID: `msg_${n}`, id, input: { query, datapack_id: 'dp1' } };
  await call('tool', 'execute.before', c);
  await sleep(ms);
  const text = JSON.stringify({ results: empty ? [] : [{ id: 'mem1', memory: 'What OpenCode knows.', score: 0.9, raw_similarity_score: 0.85 }] });
  await call('tool', 'execute.after', { ...c, status: 'completed', result: { output: { result: text }, content: [{ type: 'text', text }] } });
}

async function turn(text) {
  if (pending) {
    // The permission answer: the tool runs, and the turn ends.
    await event('permission.replied', { sessionID: sessionId, requestID: pending, reply: 'once' });
    pending = null;
    await finish();
    return;
  }
  await call('session', 'prompt', { sessionID: sessionId, messageID: `msg_u${n}`, prompt: { text }, delivery: 'steer' });
  await event('session.execution.started', { sessionID: sessionId });
  await sleep(100);
  // "write <file>": OpenCode's write tool on that file (relative to the worktree), which the guard may deny.
  const w = /write (\S+)/.exec(text);
  if (w) {
    let denied = null;
    const call0 = { tool: 'write', sessionID: sessionId, agent: 'build', messageID: `msg_${n}`, id: `call_${n}`, input: { path: w[1], content: 'x' } };
    try {
      await call('tool', 'execute.before', call0);
    } catch (e) {
      denied = e.message;
    }
    log('writes.jsonl', { path: w[1], denied });
    if (denied === null) {
      writeFileSync(isAbsolute(w[1]) ? w[1] : join(process.cwd(), w[1]), 'x');
      await call('tool', 'execute.after', { ...call0, status: 'completed', result: { content: 'ok' } });
    }
  }
  // "meko <ms> [empty]": a Meko search; "meko code": two from code mode, inside one execute call
  // and sharing its id, as OpenCode 2 runs them.
  const m = /\bmeko (\d+)(?: (empty))?/.exec(text);
  if (m) await mekoCall(`call_m${n}`, m[0], Number(m[1]), !!m[2]);
  if (/\bmeko code\b/.test(text)) {
    const x = { tool: 'execute', sessionID: sessionId, agent: 'build', messageID: `msg_${n}`, id: `call_x${n}`, input: { code: 'await tools.meko.memory_search(…)' } };
    await call('tool', 'execute.before', x);
    await mekoCall(x.id, 'first', 10, false);
    await mekoCall(x.id, 'second', 10, true);
    await call('tool', 'execute.after', { ...x, status: 'completed', result: { content: 'done' } });
  }
  // "subagent": the subagent tool's session. Its prompt comes before its session.created event, so
  // the plugin asks OpenCode; its response counts, and its turn's end doesn't end the turn.
  if (/\bsubagent\b/.test(text)) {
    const child = `${sessionId}_child`;
    parents.set(child, sessionId);
    await call('session', 'prompt', { sessionID: child, messageID: `msg_c${n}`, prompt: { text: 'help' }, delivery: 'steer' });
    await event('session.created', { sessionID: child, parentID: sessionId });
    await respond(child, 'claude-haiku-4-5');
    await event('session.execution.succeeded', { sessionID: child });
  }
  // "background <ms>": a bash command run in the background, its shell still running when the turn
  // ends and exiting <ms> later (its shell.created then handed over again, as after a reconnect);
  // "bgagent <ms>": a subagent run in the background, alike, which runs a subagent of its own.
  const bg = /\bbackground (\d+)/.exec(text);
  if (bg) {
    const id = `shl_${n}`;
    const created = await event('shell.created', { info: { id, status: 'running', command: 'npm test', cwd: process.cwd(), metadata: { sessionID: sessionId }, time: { started: Date.now() } } });
    void sleep(Number(bg[1])).then(async () => {
      await event('shell.exited', { id, exit: 0, status: 'exited' });
      await event('shell.deleted', { id });
      await replay(created);
    });
  }
  const bga = /\bbgagent (\d+)/.exec(text);
  if (bga) {
    const child = `${sessionId}_bg${n}`;
    const grandchild = `${child}_sub`;
    parents.set(child, sessionId);
    parents.set(grandchild, child);
    await event('session.created', { sessionID: child, parentID: sessionId });
    await event('session.execution.started', { sessionID: child });
    await event('session.created', { sessionID: grandchild, parentID: child });
    await event('session.execution.started', { sessionID: grandchild });
    void sleep(Number(bga[1])).then(async () => {
      await event('session.execution.succeeded', { sessionID: grandchild });
      await event('session.execution.succeeded', { sessionID: child });
    });
  }
  // "run <cmd>": the shell needs the user's answer, the next line.
  if (/^run /.test(text)) {
    pending = `per_${n}`;
    await event('permission.asked', { id: pending, sessionID: sessionId, action: 'bash', resources: [text.slice(4)] });
    return;
  }
  // "interrupt": the user pressed Esc, and the turn ends interrupted.
  await finish(/\binterrupt\b/.test(text));
}

if (!resumed) await event('session.created', { sessionID: sessionId });
log('spawn.jsonl', { sessionId });
process.stdout.write('\x1b[?2004h');
const prompt = opt('prompt');
let queue = prompt ? turn(prompt) : Promise.resolve();

if (process.stdin.isTTY) process.stdin.setRawMode(true);
process.stdin.setEncoding('utf8');
let buf = '';
process.stdin.on('data', (chunk) => {
  // Bracketed paste's markers aren't part of the text.
  buf += chunk.split('\u001b[200~').join('').split('\u001b[201~').join('');
  let i;
  while ((i = buf.search(/[\r\n]/)) >= 0) {
    const line = buf.slice(0, i).trim();
    buf = buf.slice(i + 1);
    if (!line) continue;
    if (line === 'exit') process.exit(0);
    queue = queue.then(() => turn(line));
  }
});
setInterval(() => undefined, 1000);
