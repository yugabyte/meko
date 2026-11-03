#!/usr/bin/env node
// Stand-in for the `claude` CLI in integration tests. It fires the same hook
// events as Claude Code (through the hooks settings file agent-k-local passes
// with --settings), records its argv, and turns each stdin line into a turn.
import { execFileSync } from 'node:child_process';
import { randomUUID } from 'node:crypto';
import { appendFileSync, existsSync, mkdirSync, readFileSync, rmSync, writeFileSync } from 'node:fs';
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
// Like `claude -p` (Ask AgentK): writes what it got (argv, cwd, stdin) to `ask-input.json` in the
// fake's dir and prints `ask-reply.json` from there; with an `ask-hang` file it never answers.
if (args[0] === '-p') {
  const fake = process.env.FAKE_CLAUDE_DIR ?? '';
  let input = '';
  for await (const chunk of process.stdin) input += chunk;
  mkdirSync(fake, { recursive: true });
  writeFileSync(join(fake, 'ask-input.json'), JSON.stringify({ args, cwd: process.cwd(), input }));
  if (existsSync(join(fake, 'ask-hang'))) {
    setInterval(() => undefined, 1000);
    await new Promise(() => undefined);
  }
  // Exits once the reply is written: a pipe's writes can still be pending at process.exit.
  process.stdout.write(readFileSync(join(fake, 'ask-reply.json'), 'utf8'), () => process.exit(0));
  await new Promise(() => undefined);
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
appendFileSync(join(dir, 'argv.jsonl'), JSON.stringify({ args, cwd: process.cwd(), env: { MEKO_AGENT_ID: process.env.MEKO_AGENT_ID, MEKO_DATAPACK_ID: process.env.MEKO_DATAPACK_ID, MEKO_CHECKPOINT_INTERVAL: process.env.MEKO_CHECKPOINT_INTERVAL, CLAUDE_CODE_ENABLE_FUNCTION_HOOKS: process.env.CLAUDE_CODE_ENABLE_FUNCTION_HOOKS, CLAUDE_CODE_DISABLE_BACKGROUND_TASKS: process.env.CLAUDE_CODE_DISABLE_BACKGROUND_TASKS, CLAUDE_CODE_DISABLE_BG_EXIT_HANDOFF: process.env.CLAUDE_CODE_DISABLE_BG_EXIT_HANDOFF } }) + '\n');

// Positional prompt: the last argument that isn't a flag value; everything after "--" is positional
// (like Claude Code's CLI, which rejects a prompt starting with "-" before "--" as an unknown option).
const valued = new Set(['--settings', '--session-id', '--resume', '--name', '--permission-mode', '--append-system-prompt', '--add-dir', '--plugin-dir', '--mcp-config', '--setting-sources', '--agents', '--disallowedTools', '--model']);
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
// it ack them, then report them refused (their text put in the prompt box). A learn step's parts on
// a poll are answered like the mod's fork and duplicate check would: the `learn-facts` file's text
// (else no facts), then the `learn-decisions` file's (else none); each post goes to mod-learn.jsonl,
// and each line AgentK has it show to mod-toasts.jsonl. A `learn-silent` file makes it never answer;
// a `learn-delay` file holds each answer that many milliseconds.
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
// Like the mod, it tells AgentK which prompts are still held: on each poll, and as each starts its
// turn or is refused (/mod/held).
let modTurns = Promise.resolve();
const held = new Set();
const reportHeld = () => modPost('/mod/held', { held: [...held] }).catch(() => undefined);

// Like Claude Code moving its conversation to its background sessions just after taking a prompt:
// with an `exit-unrun` file, a prompt (through the mod, or typed) that contains its text ends Claude
// before its turn starts (SessionEnd, then exit 0), once: the file goes with it.
function exitUnrun(text) {
  const f = join(dir, 'exit-unrun');
  if (!existsSync(f) || !text.includes(readFileSync(f, 'utf8'))) return;
  rmSync(f);
  hook('SessionEnd', { reason: 'other' });
  process.exit(0);
}

const LEARN_USAGE = { input_tokens: 10, output_tokens: 200, cache_read_input_tokens: 5000, cache_creation_input_tokens: 0 };

/** Like the mod's learn step: the fork's reply to an `extract` part, the duplicate check's to a `check`. */
async function modLearn(job) {
  if (existsSync(join(dir, 'learn-silent'))) return;
  const read = (f, fallback) => (existsSync(join(dir, f)) ? readFileSync(join(dir, f), 'utf8') : fallback);
  await sleep(Number(read('learn-delay', '0')));
  const post = job.kind === 'check'
    ? { id: job.id, step: 'decisions', model: job.model, text: read('learn-decisions', '{"decisions":[]}'), usage: LEARN_USAGE }
    : { id: job.id, step: 'facts', model: 'claude-fake-1', text: read('learn-facts', '{"facts":[]}'), usage: LEARN_USAGE };
  const r = await modPost('/mod/learn', post);
  appendFileSync(join(dir, 'mod-learn.jsonl'), JSON.stringify({ job, post, status: r.status }) + '\n');
}

async function modLoop() {
  await modPost('/mod/hello', { protocol: 2 }).catch(() => undefined);
  for (;;) {
    try {
      const r = await modPost('/mod/poll', { held: [...held], protocol: 2 });
      if (r.status === 200) {
        const answer = JSON.parse(r.text);
        if (answer.learn) void modLearn(answer.learn).catch(() => undefined);
        if (answer.toast) appendFileSync(join(dir, 'mod-toasts.jsonl'), JSON.stringify(answer.toast) + '\n');
        for (const p of answer.prompts) {
          appendFileSync(join(dir, 'mod-prompts.jsonl'), JSON.stringify(p) + '\n');
          if (existsSync(join(dir, 'mod-silent'))) continue;
          // Like the mod: only a 204 means AgentK still wants it run.
          const ack = await modPost('/mod/ack', { id: p.id, state: 'accepted' });
          appendFileSync(join(dir, 'mod-acks.jsonl'), JSON.stringify({ id: p.id, status: ack.status }) + '\n');
          if (ack.status !== 204) continue;
          exitUnrun(p.text);
          held.add(p.id);
          if (existsSync(join(dir, 'mod-drop'))) {
            await modPost('/mod/ack', { id: p.id, state: 'dropped', reason: 'held by another plugin', filled: true });
            held.delete(p.id);
            await reportHeld();
            continue;
          }
          modTurns = modTurns.then(async () => {
            held.delete(p.id);
            await reportHeld();
            await turn(p.text);
          });
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
  if (text.includes('INTERRUPT') || existsSync(join(dir, 'interrupt'))) {
    // Like the person pressing Ctrl+C (or Esc) mid-turn (an INTERRUPT prompt, or every turn while
    // an `interrupt` file is there): Claude Code writes the interruption to the transcript and
    // fires no Stop hook; the mod's turn.complete hook reports the turn.
    writeFileSync(transcript, JSON.stringify({ type: 'user', timestamp: new Date().toISOString(), message: { role: 'user', content: [{ type: 'text', text: '[Request interrupted by user]' }] } }) + '\n', { flag: 'a' });
    if (modOn) await modPost('/mod/turn', { turnId: `turn-${responses}`, reason: 'aborted' }).catch(() => undefined);
    return;
  }
  if (text.includes('DESIGN_MD')) writeFileSync(process.env.AK_DESIGN_FILE, '# Design\n\nDo the thing.\n');
  // Asked for the PR description before a code review: `pr-description.md` in the fake's dir, when there, beside the design file.
  if (text.includes('pr-description.md') && text.includes("write the PR's title and description") && existsSync(join(dir, 'pr-description.md'))) {
    writeFileSync(join(dirname(process.env.AK_DESIGN_FILE), 'pr-description.md'), readFileSync(join(dir, 'pr-description.md')));
  }
  // A learn session that's done: its summary, beside the design file (learnedPath).
  if (text.includes('LEARN_DONE')) writeFileSync(join(dirname(process.env.AK_DESIGN_FILE), 'learned.md'), 'Covered billing and 12 PRs.\nSaved 5 facts.\n');
  if (text.includes('DESIGN_QUESTIONS') || text.includes('RESOLVE_STORE')) {
    // A design with open questions, listed in the questions file beside it; RESOLVE_STORE (in the
    // user's answer) revises it, resolving the first one. WITH_DECISION adds a decision AgentK made
    // itself, resting on a Meko memory, kept as the user's once they changed it.
    const resolved = text.includes('RESOLVE_STORE');
    const changed = text.includes('The user wants: New cards');
    const decision = text.includes('WITH_DECISION') ? [{
      id: 'cards', kind: 'decision', question: 'How the panel is built', options: [{ label: 'Reuse the cards' }, { label: 'New cards' }], recommended: 0,
      status: 'resolved', decision: changed ? 'New cards' : 'Reuse the cards', ...(changed ? { decidedBy: 'user' } : {}),
      meko: [{ kind: 'memory', id: '131a7d0e-8f56', datapackId: 'dp-1', excerpt: 'The cards already keep drafts.' }],
    }] : [];
    writeFileSync(process.env.AK_DESIGN_FILE, `# Design\n\nDo the thing${resolved ? ' in the datapack' : ''}.\n`);
    writeFileSync(join(dirname(process.env.AK_DESIGN_FILE), 'questions.json'), JSON.stringify({
      questions: [
        { id: 'store', question: 'Where do designs live?', options: [{ label: 'Datapack' }, { label: 'Repo' }], recommended: 0,
          status: resolved ? 'resolved' : 'open', decision: resolved ? 'Datapack.' : null },
        { id: 'tabs', question: 'Tabs or one column?', status: 'open' },
        ...decision,
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
  // After the approval: the design gets its questions and decisions (DESIGN_QA, a markdown design),
  // the design's own Artifact is republished with them (REPUBLISH_DESIGN), or another page is
  // published (PUBLISH_REPORT).
  if (text.includes('DESIGN_QA')) appendFileSync(process.env.AK_DESIGN_FILE, '\n## Questions and decisions\n\n- Where do designs live? The datapack.\n');
  if (text.includes('REPUBLISH_DESIGN') || text.includes('PUBLISH_REPORT')) {
    const report = text.includes('PUBLISH_REPORT');
    const page = join(dir, `${report ? 'report' : 'design'}-${sessionId}.html`);
    writeFileSync(page, report ? '<h1>Benchmarks</h1>' : '<!doctype html><html><body><h1>Design</h1><h2>Questions and decisions</h2></body></html>');
    hook('PostToolUse', {
      tool_name: 'Artifact',
      tool_input: { file_path: page, icon: 'doc' },
      tool_response: `Published https://claude.ai/code/artifact/${report ? '9a8b7c6d-0000-4000-8000-000000000001' : '0f1e2d3c-4b5a-6978-8a9b-0c1d2e3f4a5b'} (private).`,
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
  if (text.includes('MEKO_SEARCH')) await mekoSearch();
  if (text.includes('ASK_USER') && modOn) await askUser();
  if (text.includes('agentk-review-')) await reviewSteps(text);
  respond();
  // BACKGROUND: the turn ends with a shell and a subagent still running in the background, as
  // Claude Code's Stop reports them.
  // SAY:<words>: the turn's reply is those words, a literal \n a line break (Stop's last_assistant_message).
  const said = /SAY:(.*)$/.exec(text)?.[1]?.trim().replace(/\\n/g, '\n');
  hook('Stop', said ? { last_assistant_message: said } : text.includes('BACKGROUND') ? {
    background_tasks: [
      { id: 'b1', type: 'shell', status: 'running', description: 'Run the tests', command: 'npm test' },
      { id: 'a2', type: 'subagent', status: 'running', description: 'Review the change', agent_type: 'general-purpose' },
    ],
  } : {});
  modMeasure();
}

// Like a code-review session running its steps in subagents (AgentK's code review): each step's
// file names its report tool and step id, and the subagent reports through AgentK's tools route.
// `review-reports.json` in the fake's dir maps a pass name to its report (default LGTM), and
// `review-scores.json` a step id to its score (default confirmed at 90); a `review-skip` file lists
// steps to leave unreported, and `review-skip-once` steps to leave unreported only the first time
// they're asked for. Every call and its answer go to `review-calls.jsonl`.
async function reviewSteps(text) {
  const read = (f, d) => {
    try {
      return JSON.parse(readFileSync(join(dir, f), 'utf8'));
    } catch {
      return d;
    }
  };
  const skip = read('review-skip', []);
  const once = read('review-skip-once', []);
  for (const m of text.matchAll(/Read (\S+) and do what it says\./g)) {
    const file = readFileSync(m[1], 'utf8');
    const step = /"step": "([^"]+)"/.exec(file)?.[1];
    const tool = /call `([^`]+)` once/.exec(file)?.[1]?.split('__').pop();
    if (!step || !tool || skip.includes(step)) continue;
    if (once.includes(step)) {
      writeFileSync(join(dir, 'review-skip-once'), JSON.stringify(once.filter((s) => s !== step)));
      continue;
    }
    const input = tool === 'review_score'
      ? { step, score: read('review-scores.json', {})[step] ?? { read: ['src/a.ts:1-5'], refutation: '', verdict: 'confirmed', confidence: 90, reason: 'Read it; it holds.' } }
      : { step, report: read('review-reports.json', {})[step.split(':')[1]] ?? { summary: 'LGTM', findings: [] } };
    const r = await modPost('/tools', { v: 1, tool, input }).catch((e) => ({ status: 0, text: String(e) }));
    appendFileSync(join(dir, 'review-calls.jsonl'), JSON.stringify({ step, tool, status: r.status, text: r.text }) + '\n');
  }
}

// Like Claude Code calling the Meko MCP server: a memory_search that finds one memory. With the mod,
// AgentK hears when it starts and ends; a `meko-ms` file makes it take that many milliseconds, a
// `meko-lost` file loses its end, and a `meko-then-ms` file keeps the turn going that long after it.
async function mekoSearch() {
  const id = `msg_${sessionId.slice(0, 8)}_${process.pid}_${responses++}`;
  const toolUseId = `toolu_${id}`;
  if (modOn) {
    await modPost('/mod/meko', { id: toolUseId, state: 'start' }).catch(() => undefined);
    try {
      await sleep(Number(readFileSync(join(dir, 'meko-ms'), 'utf8')));
    } catch {
      /* a quick call */
    }
    if (!existsSync(join(dir, 'meko-lost'))) await modPost('/mod/meko', { id: toolUseId, state: 'end' }).catch(() => undefined);
    try {
      await sleep(Number(readFileSync(join(dir, 'meko-then-ms'), 'utf8')));
    } catch {
      /* the turn ends at once */
    }
  }
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

// Claude's TUI enables bracketed paste at startup; the session host only wraps pastes when it's on.
process.stdout.write('\x1b[?2004h');

// Like Claude Code: when the session host ends its pane, the process gets SIGHUP and fires SessionEnd.
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
    // Like Claude crashing: gone without a SessionEnd hook.
    if (line === 'crash') process.exit(1);
    exitUnrun(line);
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
