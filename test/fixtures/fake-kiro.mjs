#!/usr/bin/env node
// Stand-in for the `kiro-cli` CLI in integration tests. A session runs as the agent it's given
// (`chat --agent <name>`, read from $KIRO_HOME/agents) and fires that agent's inline hooks the way
// Kiro 2.x does (the event as JSON on stdin; a preToolUse hook's exit 2 denies the call, its
// stderr the reason); it keeps a session file like Kiro's ($KIRO_HOME/sessions/cli/<id>.json) with
// each finished turn's usage, and turns each stdin line into a turn. Everything it does is recorded
// in $FAKE_KIRO_DIR.
import { spawnSync } from 'node:child_process';
import { randomUUID } from 'node:crypto';
import { appendFileSync, existsSync, mkdirSync, readFileSync, writeFileSync } from 'node:fs';
import { join } from 'node:path';

const args = process.argv.slice(2);
const dir = process.env.FAKE_KIRO_DIR;
const home = process.env.KIRO_HOME;
mkdirSync(dir, { recursive: true });
const log = (file, v) => appendFileSync(join(dir, file), JSON.stringify(v) + '\n');

// The version a test put in `version` (an install or update since the last read), else 2.32.0.
if (args[0] === '--version') {
  const v = join(dir, 'version');
  process.stdout.write(existsSync(v) ? readFileSync(v, 'utf8') : 'kiro-cli 2.32.0\n');
  process.exit(0);
}
// Like `kiro-cli agent create <name> --from kiro_default --directory <dir>`: Kiro's built-in default.
if (args[0] === 'agent' && args[1] === 'create') {
  const out = args[args.indexOf('--directory') + 1];
  writeFileSync(join(out, `${args[2]}.json`), JSON.stringify({
    name: args[2], description: 'Default agent', prompt: 'You are Kiro.', mcpServers: {}, tools: ['*'], allowedTools: [],
    resources: ['file://AGENTS.md'], includeMcpJson: true, kiroUpgrade: { revision: 2 },
  }));
  process.exit(0);
}
if (args[0] === 'chat' && args[1] === '--delete-session') {
  log('deleted.jsonl', { id: args[2], cwd: process.cwd() });
  process.exit(0);
}
// Like `kiro-cli chat --no-interactive --output-format stream-json /usage`: the plan's usage, the
// text from `usage.txt` in the fake's dir when there is one; records the run in `usage.jsonl`.
if (args.includes('--no-interactive') && args.at(-1) === '/usage') {
  log('usage.jsonl', { args, cwd: process.cwd() });
  const f = join(dir, 'usage.txt');
  const finalText = existsSync(f) ? readFileSync(f, 'utf8') : 'Estimated Usage | resets on 2099-01-01 | KIRO FREE\nCredits (12.5 of 50 covered in plan), 25.0%\n';
  const events = [
    { type: 'runStarted', data: { payloadSchema: 'acp', acpProtocolVersion: 1, engine: 'v2' } },
    { type: 'runFinished', data: { sessionId: randomUUID(), status: 'success', stopReason: 'end_turn', finalText } },
  ];
  process.stdout.write(events.map((e) => JSON.stringify(e)).join('\n') + '\n', () => process.exit(0));
  await new Promise(() => undefined);
}
// Like `kiro-cli chat --no-interactive --output-format stream-json` (Ask AgentK): records what it got
// and prints `ask-reply.jsonl` from the fake's dir.
if (args.includes('--no-interactive')) {
  let input = '';
  for await (const chunk of process.stdin) input += chunk;
  writeFileSync(join(dir, 'ask-input.json'), JSON.stringify({ args, cwd: process.cwd(), input }));
  process.stdout.write(readFileSync(join(dir, 'ask-reply.jsonl'), 'utf8'), () => process.exit(0));
  await new Promise(() => undefined);
}

const opt = (name) => {
  const i = args.indexOf(name);
  return i >= 0 ? args[i + 1] : null;
};
// Like Kiro: the first prompt is the positional argument (one given after "--" is dropped, as
// kiro-cli 2.32 does with --agent).
const valued = new Set(['--agent', '--resume-id', '--model', '--trust-tools']);
let prompt = null;
for (let i = 0; i < args.length; i++) {
  if (args[i] === '--') break;
  if (valued.has(args[i])) i++;
  else if (i > 0 && !args[i].startsWith('-')) prompt = args[i];
}
const agent = JSON.parse(readFileSync(join(home, 'agents', `${opt('--agent')}.json`), 'utf8'));
const sessions = join(home, 'sessions', 'cli');
mkdirSync(sessions, { recursive: true });
// Like Kiro: --resume-id continues that session, --resume the newest in this folder, else a new one.
const newest = () => {
  const f = join(dir, 'last-session');
  return existsSync(f) ? readFileSync(f, 'utf8') : null;
};
const sessionId = opt('--resume-id') ?? (args.includes('--resume') ? newest() : null) ?? randomUUID();
writeFileSync(join(dir, 'last-session'), sessionId);
const file = join(sessions, `${sessionId}.json`);
const state = existsSync(file)
  ? JSON.parse(readFileSync(file, 'utf8'))
  : { session_id: sessionId, cwd: process.cwd(), session_state: { conversation_metadata: { user_turn_metadatas: [] } } };
const save = () => writeFileSync(file, JSON.stringify(state));
save();
log('argv.jsonl', { args, cwd: process.cwd(), env: { MEKO_AGENT_ID: process.env.MEKO_AGENT_ID, AK_SESSION: process.env.AK_SESSION } });

/** Runs the agent's hooks for an event (matcher '*' or the tool's name); returns their stdout, and a denial's reason. */
function hook(event, extra = {}) {
  let out = '';
  let denied = null;
  for (const h of agent.hooks?.[event] ?? []) {
    if (h.matcher && h.matcher !== '*' && !(h.matcher === extra.tool_name || (h.matcher === 'fs_write' && extra.tool_name === 'write'))) continue;
    const input = JSON.stringify({ hook_event_name: event, cwd: process.cwd(), session_id: sessionId, ...extra });
    const r = spawnSync('sh', ['-c', h.command], { input });
    out += r.stdout?.toString() ?? '';
    if (event === 'preToolUse' && r.status === 2) denied = r.stderr?.toString().trim() ?? '';
  }
  return { out, denied };
}

function turn(text) {
  hook('userPromptSubmit', { prompt: text });
  // "write <file>": Kiro's write tool on that file, which the guard may deny.
  const w = /write (\S+)/.exec(text);
  if (w) {
    const input = { command: 'create', path: w[1], content: 'x' };
    const { denied } = hook('preToolUse', { tool_name: 'write', tool_input: input });
    log('writes.jsonl', { path: w[1], denied });
    if (denied === null) {
      writeFileSync(join(process.cwd(), w[1]), 'x');
      hook('postToolUse', { tool_name: 'write', tool_input: input, tool_response: { success: true } });
    }
  }
  const turns = state.session_state.conversation_metadata.user_turn_metadatas;
  turns.push({
    end_timestamp: new Date(Date.now() + turns.length).toISOString(), model: 'auto', input_token_count: 0, output_token_count: 0,
    cache_read_input_token_count: 0, cache_write_input_token_count: 0, metering_usage: [{ value: 0.5, unit: 'credit', unitPlural: 'credits' }],
    final_context_usage_percentage: 12.6,
  });
  save();
  hook('stop', { assistant_response: 'Done.' });
}

// Like Kiro: the agent spawns (its context hooks run) before the first prompt.
const spawned = hook('agentSpawn');
log('spawn.jsonl', { sessionId, context: spawned.out });
process.stdout.write('\x1b[?2004h');
if (prompt) turn(prompt);

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
    turn(line);
  }
});
setInterval(() => undefined, 1000);
