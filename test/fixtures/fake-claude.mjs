#!/usr/bin/env node
// Stand-in for the `claude` CLI in integration tests. It fires the same hook
// events as Claude Code (through the hooks settings file agent-k-local passes
// with --settings), records its argv, and turns each stdin line into a turn.
import { execFileSync } from 'node:child_process';
import { randomUUID } from 'node:crypto';
import { appendFileSync, existsSync, mkdirSync, readFileSync, writeFileSync } from 'node:fs';
import { request } from 'node:http';
import { dirname, join } from 'node:path';

const args = process.argv.slice(2);
// Like `claude --version`; the version is the `version` file in the fake's dir, if there is one.
if (args[0] === '--version') {
  let v = '2.1.287';
  try {
    v = readFileSync(join(process.env.FAKE_CLAUDE_DIR ?? '', 'version'), 'utf8').trim();
  } catch {
    /* the default */
  }
  process.stdout.write(`${v} (Claude Code)\n`);
  process.exit(0);
}
// Like `claude update`: an `update-to` file in the fake's dir is the version it updates to (written
// to `version`); an `update-fail` file makes it fail with that file's text.
if (args[0] === 'update') {
  const fake = process.env.FAKE_CLAUDE_DIR ?? '';
  const read = (f) => {
    try {
      return readFileSync(join(fake, f), 'utf8').trim();
    } catch {
      return null;
    }
  };
  const fail = read('update-fail');
  if (fail !== null) {
    process.stderr.write(`${fail}\n`);
    process.exit(1);
  }
  const from = read('version') ?? '2.1.287';
  const to = read('update-to');
  if (to && to !== from) {
    writeFileSync(join(fake, 'version'), to);
    process.stdout.write(`Current version: ${from}\nSuccessfully updated from ${from} to version ${to}\n`);
  } else {
    process.stdout.write(`Claude Code is up to date (${from})\n`);
  }
  process.exit(0);
}
const opt = (name) => {
  const i = args.indexOf(name);
  return i >= 0 ? args[i + 1] : null;
};
const settings = JSON.parse(readFileSync(opt('--settings'), 'utf8'));
let sessionId = opt('--session-id') ?? opt('--resume');
const dir = process.env.FAKE_CLAUDE_DIR;
mkdirSync(dir, { recursive: true });
let transcript = join(dir, `${sessionId}.jsonl`);
appendFileSync(join(dir, 'argv.jsonl'), JSON.stringify({ args, cwd: process.cwd(), env: { MEKO_AGENT_ID: process.env.MEKO_AGENT_ID, MEKO_DATAPACK_ID: process.env.MEKO_DATAPACK_ID, MEKO_CHECKPOINT_INTERVAL: process.env.MEKO_CHECKPOINT_INTERVAL, CLAUDE_CODE_ENABLE_FUNCTION_HOOKS: process.env.CLAUDE_CODE_ENABLE_FUNCTION_HOOKS } }) + '\n');

// Positional prompt: the last argument that isn't a flag value; everything after "--" is positional
// (like Claude Code's CLI, which rejects a prompt starting with "-" before "--" as an unknown option).
const valued = new Set(['--settings', '--session-id', '--resume', '--name', '--permission-mode', '--append-system-prompt', '--add-dir', '--plugin-dir']);
let prompt = null;
for (let i = 0; i < args.length; i++) {
  if (args[i] === '--') {
    prompt = args[i + 1] ?? null;
    break;
  }
  if (valued.has(args[i])) i++;
  else if (args[i].startsWith('-')) {
    process.stderr.write(`error: unknown option '${args[i]}'\n`);
    process.exit(1);
  } else prompt = args[i];
}

// Like Claude Code: a group's matcher is a regex on the tool name ('*' or none matches all).
const matches = (matcher, tool) => !matcher || matcher === '*' || !tool || new RegExp(`^(?:${matcher})$`).test(tool);

/** Runs the hooks for an event; returns what they printed (a PreToolUse decision, if any). */
function hook(event, extra = {}) {
  const groups = settings.hooks?.[event] ?? [];
  let out = '';
  for (const g of groups) {
    if (!matches(g.matcher, extra.tool_name)) continue;
    for (const h of g.hooks) {
      const payload = JSON.stringify({ hook_event_name: event, session_id: sessionId, transcript_path: transcript, cwd: process.cwd(), ...extra });
      try {
        out += execFileSync(h.command, { input: payload, stdio: ['pipe', 'pipe', 'ignore'] }).toString();
      } catch {
        /* hooks never fail the session */
      }
    }
  }
  return out;
}

const sleep = (ms) => new Promise((r) => setTimeout(r, ms));
let planWaiting = false;

// Like AgentK's mod (src/mod/agentk) loaded with --plugin-dir: when the fake's dir has a `mod` file,
// it polls the hook socket for prompts, acks each and takes it as a typed prompt (mod-prompts.jsonl
// records them, mod-acks.jsonl the server's answer to each ack), and reports a measurement after
// each turn. A `mod-silent` file makes it take prompts without acking them; a `mod-drop` file makes
// it ack them, then report them refused (their text put in the prompt box).
const pluginDir = opt('--plugin-dir');
const modOn = !!pluginDir && existsSync(join(pluginDir, 'hooks', 'register.ts')) && existsSync(join(dir, 'mod'));

function modPost(path, body) {
  return new Promise((resolve, reject) => {
    const headers = { 'Content-Type': 'application/json', 'X-AK-Token': process.env.AK_HOOK_TOKEN ?? '' };
    const req = request({ socketPath: process.env.AK_HOOK_SOCK, path, method: 'POST', headers }, (res) => {
      let text = '';
      res.setEncoding('utf8');
      res.on('data', (c) => (text += c));
      res.on('end', () => resolve({ status: res.statusCode ?? 0, text }));
    });
    req.on('error', reject);
    req.end(JSON.stringify(body));
  });
}

// Turns from the mod run one at a time, as the engine holds a plugin's prompt until it's idle.
let modTurns = Promise.resolve();

async function modLoop() {
  await modPost('/mod/hello', { protocol: 1 }).catch(() => undefined);
  for (;;) {
    try {
      const r = await modPost('/mod/poll', {});
      if (r.status === 200) {
        for (const p of JSON.parse(r.text).prompts) {
          appendFileSync(join(dir, 'mod-prompts.jsonl'), JSON.stringify(p) + '\n');
          if (existsSync(join(dir, 'mod-silent'))) continue;
          // Like the mod: only a 204 means AgentK still wants it run.
          const ack = await modPost('/mod/ack', { id: p.id, state: 'accepted' });
          appendFileSync(join(dir, 'mod-acks.jsonl'), JSON.stringify({ id: p.id, status: ack.status }) + '\n');
          if (ack.status !== 204) continue;
          if (existsSync(join(dir, 'mod-drop'))) {
            await modPost('/mod/ack', { id: p.id, state: 'dropped', reason: 'held by another plugin', filled: true });
            continue;
          }
          modTurns = modTurns.then(() => turn(p.text));
        }
      } else if (r.status !== 204) await sleep(500);
    } catch {
      await sleep(500);
    }
  }
}

// Like Claude's AskUserQuestion with the mod loaded: the mod forwards the question to AgentK and
// asks again after each hold, while the terminal's dialog is open (the PermissionRequest hook fires
// for it). An `ask-terminal` file answers in the terminal after its milliseconds instead. The
// outcome goes to mod-asks.jsonl.
const ASK = [{ question: 'Which color?', header: 'Color', options: [{ label: 'Red', description: 'Warm' }, { label: 'Blue', description: 'Cool' }], multiSelect: false }];
async function askUser() {
  const id = `toolu_${randomUUID()}`;
  const call = { settled: false };
  const agentk = (async () => {
    while (!call.settled) {
      const r = await modPost('/mod/ask', { id, questions: ASK }).catch(() => null);
      if (r?.status === 200) return { by: 'agentk', answers: JSON.parse(r.text).answers };
      if (r?.status !== 204) await new Promise(() => undefined); // left to the terminal
    }
    return new Promise(() => undefined);
  })();
  hook('PermissionRequest', { tool_name: 'AskUserQuestion', tool_input: { questions: ASK } });
  let terminalMs = null;
  try {
    terminalMs = Number(readFileSync(join(dir, 'ask-terminal'), 'utf8'));
  } catch {
    /* only AgentK answers */
  }
  const terminal = terminalMs === null ? new Promise(() => undefined) : sleep(terminalMs).then(() => ({ by: 'terminal', answers: { 0: 'Red' } }));
  const won = await Promise.race([agentk, terminal]);
  call.settled = true;
  await modPost('/mod/ask/done', { id }).catch(() => undefined);
  appendFileSync(join(dir, 'mod-asks.jsonl'), JSON.stringify(won) + '\n');
  hook('PostToolUse', { tool_name: 'AskUserQuestion', tool_response: won });
}

// Like Claude calling the mod's design_questions tool: a call missing an id first (told what's
// wrong), then a good one. Each answer goes to mod-tool.jsonl.
async function designQuestionsTool() {
  for (const questions of [
    [{ question: 'Where do designs live?' }],
    [{ id: 'store', question: 'Where do designs live?', options: [{ label: 'Datapack' }, { label: 'Repo' }], recommended: 0 }],
  ]) {
    const r = await modPost('/mod/questions', { questions }).catch((e) => ({ status: 0, text: String(e) }));
    appendFileSync(join(dir, 'mod-tool.jsonl'), JSON.stringify({ status: r.status, body: r.text ? JSON.parse(r.text) : null }) + '\n');
  }
}

/** Like the mod's session.measure: the context fill (the `context-pct` file, else 37%) and plan limits. */
function modMeasure() {
  if (!modOn) return;
  let percent = 37;
  try {
    percent = Number(readFileSync(join(dir, 'context-pct'), 'utf8'));
  } catch {
    /* the default */
  }
  void modPost('/mod/measure', {
    context: { tokens: percent * 2000, window: 200000, percent },
    rateLimits: [{ kind: 'five_hour', percentUsed: 21, resetsAt: '2100-01-01T00:00:00.000Z' }, { kind: 'seven_day', percentUsed: 9, resetsAt: '2100-01-01T00:00:00.000Z' }],
  }).catch(() => undefined);
}

async function turn(text) {
  writeFileSync(transcript, JSON.stringify({ type: 'user', text }) + '\n', { flag: 'a' });
  hook('UserPromptSubmit', { prompt: text });
  // Early in the turn, so the questions show while it goes on.
  if (text.includes('DESIGN_TOOL') && modOn) await designQuestionsTool();
  let ms = 200;
  try {
    ms = Number(readFileSync(join(dir, 'turn-ms'), 'utf8'));
  } catch {
    /* default */
  }
  await sleep(ms);
  if (text.includes('DESIGN_MD')) writeFileSync(process.env.AK_DESIGN_FILE, '# Design\n\nDo the thing.\n');
  if (text.includes('DESIGN_QUESTIONS') || text.includes('RESOLVE_STORE')) {
    // A design with open questions, listed in the questions file beside it; RESOLVE_STORE (in the
    // user's answer) revises it, resolving the first one.
    const resolved = text.includes('RESOLVE_STORE');
    writeFileSync(process.env.AK_DESIGN_FILE, `# Design\n\nDo the thing${resolved ? ' in the datapack' : ''}.\n`);
    writeFileSync(join(dirname(process.env.AK_DESIGN_FILE), 'questions.json'), JSON.stringify({
      questions: [
        { id: 'store', question: 'Where do designs live?', options: [{ label: 'Datapack' }, { label: 'Repo' }], recommended: 0,
          status: resolved ? 'resolved' : 'open', decision: resolved ? 'Datapack.' : null },
        { id: 'tabs', question: 'Tabs or one column?', status: 'open' },
      ],
    }));
  }
  if (text.includes('DESIGN_ARTIFACT')) {
    // Like publishing with the Artifact tool: the page is a local file, the result has its link.
    const page = join(dir, `design-${sessionId}.html`);
    writeFileSync(page, '<!doctype html><html><head><title>Design</title></head><body><h1>Design</h1></body></html>');
    hook('PostToolUse', {
      tool_name: 'Artifact',
      tool_input: { file_path: page, icon: 'doc' },
      tool_response: 'Published https://claude.ai/code/artifact/0f1e2d3c-4b5a-6978-8a9b-0c1d2e3f4a5b (private).',
    });
  }
  if (text.includes('STATUSLINE')) {
    // Like Claude Code: runs the status line command with the session's JSON, plan limits included.
    const input = JSON.stringify({
      session_id: sessionId, model: { id: 'claude-fake-1' },
      rate_limits: { five_hour: { used_percentage: 42, resets_at: 4102444800 }, seven_day: { used_percentage: 18, resets_at: 4102444800 } },
    });
    let out = '';
    try {
      out = execFileSync('sh', ['-c', settings.statusLine.command], { input, stdio: ['pipe', 'pipe', 'ignore'] }).toString();
    } catch {
      /* a failing status line shows nothing */
    }
    writeFileSync(join(dir, 'statusline.out'), out);
  }
  if (text.includes('EDIT_REPO')) {
    // Asks the edit guard about a file in the repo and one outside it, and records the answers.
    for (const file of [join(process.cwd(), 'src.ts'), join(dir, 'scratch.txt')]) {
      const out = hook('PreToolUse', { tool_name: 'Write', tool_input: { file_path: file, content: 'x' } });
      appendFileSync(join(dir, 'guard.jsonl'), JSON.stringify({ file, out }) + '\n');
    }
  }
  if (text.includes('PLAN')) {
    planWaiting = true;
    hook('PreToolUse', { tool_name: 'ExitPlanMode', tool_input: { plan: '# Design\n\nDo the thing.' } });
    return; // waits for approval (Enter)
  }
  if (text.includes('MEKO_SEARCH')) mekoSearch();
  if (text.includes('ASK_USER') && modOn) await askUser();
  respond();
  hook('Stop', {});
  modMeasure();
}

// Like Claude Code calling the Meko MCP server: a memory_search that finds one memory.
function mekoSearch() {
  const id = `msg_${sessionId.slice(0, 8)}_${process.pid}_${responses++}`;
  const toolUseId = `toolu_${id}`;
  const usage = { input_tokens: 3, output_tokens: 40, cache_read_input_tokens: 5000, cache_creation_input_tokens: 100 };
  const call = { type: 'tool_use', id: toolUseId, name: 'mcp__meko__memory_search', input: { query: 'how does it work' } };
  const found = JSON.stringify({ result: JSON.stringify({ results: [{ id: 'm1', memory: 'It works like this.' }] }) });
  // A second before the response that follows it, as a real tool call would be.
  const at = new Date(Date.now() - 1000).toISOString();
  writeFileSync(transcript,
    JSON.stringify({ type: 'assistant', timestamp: at, message: { id, model: 'claude-fake-1', stop_reason: 'tool_use', usage, content: [call] } }) + '\n' +
    JSON.stringify({ type: 'user', timestamp: at, message: { role: 'user', content: [{ type: 'tool_result', tool_use_id: toolUseId, content: found }] } }) + '\n',
    { flag: 'a' });
}

// Like Claude Code: one response is written as several lines sharing a message id; the last one
// says it ended the turn.
let responses = 0;
function respond() {
  const id = `msg_${sessionId.slice(0, 8)}_${process.pid}_${responses++}`;
  const usage = { input_tokens: 3, output_tokens: 50, cache_read_input_tokens: 1000, cache_creation_input_tokens: 200 };
  const entry = (stop_reason) =>
    JSON.stringify({ type: 'assistant', timestamp: new Date().toISOString(), message: { id, model: 'claude-fake-1', stop_reason, usage } }) + '\n';
  writeFileSync(transcript, entry(null) + entry('end_turn'), { flag: 'a' });
}

// Claude's TUI enables bracketed paste at startup; tmux only wraps pastes when it's on.
process.stdout.write('\x1b[?2004h');

// Like Claude Code: when tmux dies the pane's process gets SIGHUP and fires SessionEnd.
process.on('SIGHUP', () => {
  hook('SessionEnd', { reason: 'other' });
  process.exit(129);
});

// Like Claude's TUI, the terminal is read raw, not line by line: a line-mode terminal caps a line
// (1,024 bytes on macOS, 4,096 on Linux) and a longer typed prompt never arrives whole.
if (process.stdin.isTTY) process.stdin.setRawMode(true);
process.stdin.setEncoding('utf8');
// Like Claude Code in an untrusted folder: no hooks until the trust dialog is answered.
if (existsSync(join(dir, 'trust-prompt'))) {
  process.stdout.write('Do you trust the files in this folder?\r\n');
  await new Promise((r) => process.stdin.once('data', r));
}

writeFileSync(transcript, '', { flag: 'a' });
{
  // Like Claude Code, the first prompt runs after the SessionStart hooks; what they print is context.
  const source = opt('--resume') ? 'resume' : 'startup';
  const context = hook('SessionStart', { source });
  appendFileSync(join(dir, 'session-start.jsonl'), JSON.stringify({ sessionId, source, context }) + '\n');
}
if (modOn) void modLoop();
if (prompt) await turn(prompt);

let buf = '';
let inPaste = false;
process.stdin.on('data', async (chunk) => {
  if (process.env.FAKE_CLAUDE_DEBUG || existsSync(join(dir, 'debug'))) appendFileSync(join(dir, 'stdin.log'), JSON.stringify({ t: Date.now(), chunk }) + '\n');
  // Like Claude's TUI, a bracketed paste is one input: newlines inside it don't submit.
  // The tty may split a paste across reads, so track the paste state across chunks.
  // eslint-disable-next-line no-control-regex
  for (const part of chunk.split(/(\x1b\[20[01]~)/)) {
    if (part === '\x1b[200~') inPaste = true;
    else if (part === '\x1b[201~') inPaste = false;
    else buf += inPaste ? part.replace(/[\r\n]+/g, ' ') : part;
  }
  let i;
  while ((i = buf.search(/[\r\n]/)) >= 0) {
    const line = buf.slice(0, i).trim();
    buf = buf.slice(i + 1);
    if (planWaiting && line === '') {
      planWaiting = false;
      hook('PostToolUse', { tool_name: 'ExitPlanMode', tool_response: 'User has approved your plan.' });
      respond();
      hook('Stop', {});
      continue;
    }
    if (!line) continue;
    if (line === 'exit') {
      hook('SessionEnd', { reason: 'exit' });
      process.exit(0);
    }
    if (line === '/clear') {
      // Like Claude Code: /clear starts a new conversation with a new session id and transcript.
      hook('SessionEnd', { reason: 'clear' });
      sessionId = randomUUID();
      transcript = join(dir, `${sessionId}.jsonl`);
      writeFileSync(transcript, '', { flag: 'a' });
      hook('SessionStart', { source: 'clear' });
      continue;
    }
    await turn(line);
  }
});
