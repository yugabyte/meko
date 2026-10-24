#!/usr/bin/env node
// Stand-in for the `opencode` CLI in integration tests. A session loads the plugins its
// OPENCODE_CONFIG_CONTENT names (AgentK's, for real) and drives their hooks the way OpenCode 1.19
// does: `chat.message` for a prompt, `tool.execute.before` (whose throw denies the call) and
// `.after` around a tool, `experimental.chat.system.transform` before a response, and the
// `session.created`, `permission.asked`/`.replied`, `message.updated` and `session.idle` events.
// Each stdin line is a prompt. `--session` with an id it never made exits 1, as OpenCode's
// validateSession does. Everything it does is recorded in $FAKE_OPENCODE_DIR.
import { randomBytes } from 'node:crypto';
import { appendFileSync, existsSync, mkdirSync, readFileSync, writeFileSync } from 'node:fs';
import { isAbsolute, join } from 'node:path';

const args = process.argv.slice(2);
const dir = process.env.FAKE_OPENCODE_DIR;
mkdirSync(dir, { recursive: true });
const log = (file, v) => appendFileSync(join(dir, file), JSON.stringify(v) + '\n');
const sleep = (ms) => new Promise((r) => setTimeout(r, ms));

// The version a test put in `version`, else 1.19.4.
if (args[0] === '--version') {
  const v = join(dir, 'version');
  process.stdout.write(existsSync(v) ? readFileSync(v, 'utf8') : '1.19.4\n');
  process.exit(0);
}
if (args[0] === 'upgrade') {
  log('upgrade.jsonl', { args });
  process.exit(0);
}
if (args[0] === 'session' && args[1] === 'delete') {
  log('deleted.jsonl', { id: args[2], cwd: process.cwd() });
  process.exit(0);
}
// Like `opencode run --format json` (Ask AgentK): records what it got, with the project config in
// its folder, and prints `ask-reply.jsonl` from the fake's dir.
if (args[0] === 'run') {
  let input = '';
  for await (const chunk of process.stdin) input += chunk;
  const cfg = join(process.cwd(), 'opencode.json');
  writeFileSync(join(dir, 'ask-input.json'), JSON.stringify({ args, cwd: process.cwd(), input, config: existsSync(cfg) ? JSON.parse(readFileSync(cfg, 'utf8')) : null }));
  process.stdout.write(readFileSync(join(dir, 'ask-reply.jsonl'), 'utf8'), () => process.exit(0));
  await new Promise(() => undefined);
}

const opt = (name) => args.find((a) => a.startsWith(`--${name}=`))?.slice(name.length + 3) ?? null;
const known = join(dir, 'sessions.json');
const sessions = existsSync(known) ? JSON.parse(readFileSync(known, 'utf8')) : [];
const resumed = opt('session');
if (resumed && !sessions.includes(resumed)) {
  process.stderr.write(`Session not found: ${resumed}\n`);
  process.exit(1);
}
const sessionId = resumed ?? `ses_${randomBytes(6).toString('hex')}`;
if (!resumed) writeFileSync(known, JSON.stringify([...sessions, sessionId]));
const config = JSON.parse(process.env.OPENCODE_CONFIG_CONTENT || '{}');
log('argv.jsonl', {
  args, cwd: process.cwd(), config,
  env: Object.fromEntries(['MEKO_AGENT_ID', 'AK_SESSION', 'AK_OPENCODE_SOURCE', 'OPENCODE_CONFIG', 'OPENCODE_DISABLE_PROJECT_CONFIG'].map((k) => [k, process.env[k] ?? null])),
});

// Like OpenCode: every plugin's exported functions are its server plugins.
const hooks = [];
for (const spec of config.plugin ?? []) {
  const mod = await import(spec);
  for (const fn of Object.values(mod)) hooks.push(await fn({ directory: process.cwd(), worktree: process.cwd() }));
}
const call = async (name, input, output) => {
  for (const h of hooks) if (h[name]) await h[name](input, output);
};
const event = (type, properties) => Promise.all(hooks.map((h) => h.event?.({ event: { type, properties } })));

let n = 0;
let pending = null;
async function respond(session, model = 'claude-sonnet-4-5') {
  n++;
  const system = [];
  await call('experimental.chat.system.transform', { sessionID: session, model: {} }, { system });
  if (session === sessionId) log('system.jsonl', { system, instructions: (config.instructions ?? []).map((f) => readFileSync(f, 'utf8')) });
  const info = {
    id: `msg_${sessionId}_${n}`, sessionID: session, role: 'assistant', modelID: model, providerID: 'anthropic',
    time: { created: Date.now(), completed: Date.now() }, tokens: { input: 100, output: 20, reasoning: 5, cache: { read: 1000, write: 50 } },
  };
  await event('message.updated', { info });
  // OpenCode sends a finished message again as its parts settle: logged once.
  await event('message.updated', { info });
}

async function finish() {
  await respond(sessionId);
  await sleep(100);
  await event('session.idle', { sessionID: sessionId });
}

async function turn(text) {
  if (pending) {
    // The permission answer: the tool runs, and the turn ends.
    await event('permission.replied', { sessionID: sessionId, response: text });
    pending = null;
    await finish();
    return;
  }
  await call('chat.message', { sessionID: sessionId }, { message: {}, parts: [{ type: 'text', text }] });
  await sleep(100);
  // "write <file>": OpenCode's write tool on that file, which the guard may deny.
  const w = /write (\S+)/.exec(text);
  if (w) {
    const filePath = isAbsolute(w[1]) ? w[1] : join(process.cwd(), w[1]);
    let denied = null;
    try {
      await call('tool.execute.before', { tool: 'write', sessionID: sessionId, callID: `call_${n}` }, { args: { filePath, content: 'x' } });
    } catch (e) {
      denied = e.message;
    }
    log('writes.jsonl', { path: w[1], denied });
    if (denied === null) {
      writeFileSync(filePath, 'x');
      await call('tool.execute.after', { tool: 'write', sessionID: sessionId, callID: `call_${n}`, args: { filePath } }, { title: '', output: '', metadata: {} });
    }
  }
  // "subagent": the task tool's session, whose response counts and whose idle doesn't end the turn.
  if (/\bsubagent\b/.test(text)) {
    const child = `${sessionId}_child`;
    await event('session.created', { info: { id: child, parentID: sessionId } });
    await respond(child, 'claude-haiku-4-5');
    await event('session.idle', { sessionID: child });
  }
  // "run <cmd>": bash needs the user's answer, the next line.
  if (/^run /.test(text)) {
    pending = text;
    await event('permission.asked', { id: `per_${n}`, sessionID: sessionId, permission: 'bash', patterns: [text.slice(4)] });
    return;
  }
  await finish();
}

if (!resumed) await event('session.created', { info: { id: sessionId } });
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
