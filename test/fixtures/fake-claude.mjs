#!/usr/bin/env node
// Stand-in for the `claude` CLI in integration tests. It fires the same hook
// events as Claude Code (through the hooks settings file agent-k-local passes
// with --settings), records its argv, and turns each stdin line into a turn.
import { execFileSync } from 'node:child_process';
import { appendFileSync, existsSync, mkdirSync, readFileSync, writeFileSync } from 'node:fs';
import { join } from 'node:path';

const args = process.argv.slice(2);
const opt = (name) => {
  const i = args.indexOf(name);
  return i >= 0 ? args[i + 1] : null;
};
const settings = JSON.parse(readFileSync(opt('--settings'), 'utf8'));
const sessionId = opt('--session-id') ?? opt('--resume');
const dir = process.env.FAKE_CLAUDE_DIR;
mkdirSync(dir, { recursive: true });
const transcript = join(dir, `${sessionId}.jsonl`);
appendFileSync(join(dir, 'argv.jsonl'), JSON.stringify({ args, cwd: process.cwd(), env: { MEKO_AGENT_ID: process.env.MEKO_AGENT_ID, MEKO_DATAPACK_ID: process.env.MEKO_DATAPACK_ID } }) + '\n');

// Positional prompt: the last argument that isn't a flag value.
const valued = new Set(['--settings', '--session-id', '--resume', '--name', '--permission-mode', '--append-system-prompt']);
let prompt = null;
for (let i = 0; i < args.length; i++) {
  if (valued.has(args[i])) i++;
  else if (!args[i].startsWith('--')) prompt = args[i];
}

function hook(event, extra = {}) {
  const groups = settings.hooks?.[event] ?? [];
  for (const g of groups) {
    for (const h of g.hooks) {
      const payload = JSON.stringify({ hook_event_name: event, session_id: sessionId, transcript_path: transcript, cwd: process.cwd(), ...extra });
      try {
        execFileSync(h.command, { input: payload, stdio: ['pipe', 'ignore', 'ignore'] });
      } catch {
        /* hooks never fail the session */
      }
    }
  }
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
  if (text.includes('PLAN')) {
    planWaiting = true;
    hook('PreToolUse', { tool_name: 'ExitPlanMode', tool_input: { plan: '# Design\n\nDo the thing.' } });
    return; // waits for approval (Enter)
  }
  hook('Stop', {});
}

// Claude's TUI enables bracketed paste at startup; tmux only wraps pastes when it's on.
process.stdout.write('\x1b[?2004h');

// Like Claude Code: when tmux dies the pane's process gets SIGHUP and fires SessionEnd.
process.on('SIGHUP', () => {
  hook('SessionEnd', { reason: 'other' });
  process.exit(129);
});

writeFileSync(transcript, '', { flag: 'a' });
hook('SessionStart', { source: opt('--resume') ? 'resume' : 'startup' });
if (prompt) await turn(prompt);

process.stdin.setEncoding('utf8');
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
      hook('Stop', {});
      continue;
    }
    if (!line) continue;
    if (line === 'exit') {
      hook('SessionEnd', { reason: 'exit' });
      process.exit(0);
    }
    await turn(line);
  }
});
