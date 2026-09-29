#!/usr/bin/env node
// Stand-in for the `claude` CLI in integration tests. It fires the same hook
// events as Claude Code (through the hooks settings file agent-k-local passes
// with --settings), records its argv, and turns each stdin line into a turn.
import { execFileSync } from 'node:child_process';
import { randomUUID } from 'node:crypto';
import { appendFileSync, existsSync, mkdirSync, readFileSync, writeFileSync } from 'node:fs';
import { join } from 'node:path';

const args = process.argv.slice(2);
const opt = (name) => {
  const i = args.indexOf(name);
  return i >= 0 ? args[i + 1] : null;
};
const settings = JSON.parse(readFileSync(opt('--settings'), 'utf8'));
let sessionId = opt('--session-id') ?? opt('--resume');
const dir = process.env.FAKE_CLAUDE_DIR;
mkdirSync(dir, { recursive: true });
let transcript = join(dir, `${sessionId}.jsonl`);
appendFileSync(join(dir, 'argv.jsonl'), JSON.stringify({ args, cwd: process.cwd(), env: { MEKO_AGENT_ID: process.env.MEKO_AGENT_ID, MEKO_DATAPACK_ID: process.env.MEKO_DATAPACK_ID, MEKO_CHECKPOINT_INTERVAL: process.env.MEKO_CHECKPOINT_INTERVAL } }) + '\n');

// Positional prompt: the last argument that isn't a flag value; everything after "--" is positional
// (like Claude Code's CLI, which rejects a prompt starting with "-" before "--" as an unknown option).
const valued = new Set(['--settings', '--session-id', '--resume', '--name', '--permission-mode', '--append-system-prompt', '--add-dir']);
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

async function turn(text) {
  writeFileSync(transcript, JSON.stringify({ type: 'user', text }) + '\n', { flag: 'a' });
  hook('UserPromptSubmit', { prompt: text });
  let ms = 200;
  try {
    ms = Number(readFileSync(join(dir, 'turn-ms'), 'utf8'));
  } catch {
    /* default */
  }
  await sleep(ms);
  if (text.includes('DESIGN_MD')) writeFileSync(process.env.AK_DESIGN_FILE, '# Design\n\nDo the thing.\n');
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
  respond();
  hook('Stop', {});
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
    JSON.stringify({ type: 'assistant', timestamp: at, message: { id, model: 'claude-fake-1', usage, content: [call] } }) + '\n' +
    JSON.stringify({ type: 'user', timestamp: at, message: { role: 'user', content: [{ type: 'tool_result', tool_use_id: toolUseId, content: found }] } }) + '\n',
    { flag: 'a' });
}

// Like Claude Code: one response is written as several lines sharing a message id.
let responses = 0;
function respond() {
  const id = `msg_${sessionId.slice(0, 8)}_${process.pid}_${responses++}`;
  const usage = { input_tokens: 3, output_tokens: 50, cache_read_input_tokens: 1000, cache_creation_input_tokens: 200 };
  const entry = JSON.stringify({ type: 'assistant', timestamp: new Date().toISOString(), message: { id, model: 'claude-fake-1', usage } }) + '\n';
  writeFileSync(transcript, entry + entry, { flag: 'a' });
}

// Claude's TUI enables bracketed paste at startup; tmux only wraps pastes when it's on.
process.stdout.write('\x1b[?2004h');

// Like Claude Code: when tmux dies the pane's process gets SIGHUP and fires SessionEnd.
process.on('SIGHUP', () => {
  hook('SessionEnd', { reason: 'other' });
  process.exit(129);
});

process.stdin.setEncoding('utf8');
// Like Claude Code in an untrusted folder: no hooks until the trust dialog is answered.
if (existsSync(join(dir, 'trust-prompt'))) {
  process.stdout.write('Do you trust the files in this folder?\r\n');
  await new Promise((r) => process.stdin.once('data', r));
}

writeFileSync(transcript, '', { flag: 'a' });
hook('SessionStart', { source: opt('--resume') ? 'resume' : 'startup' });
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
