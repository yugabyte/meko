#!/usr/bin/env node
// A stand-in for `claude -p --output-format stream-json`: reads the message on stdin, records the
// call (its arguments and message) in FAKE_CLAUDE_DIR/calls.jsonl, writes any files the answer asks
// for into its working folder, and prints stream-json events ending in a result whose
// structured_output is the first answer in FAKE_CLAUDE_DIR/answers.json whose `match` the message has.
import { appendFileSync, mkdirSync, readFileSync, writeFileSync } from 'node:fs';
import { dirname, join } from 'node:path';

const dir = process.env.FAKE_CLAUDE_DIR;
const args = process.argv.slice(2);
let input = '';
process.stdin.setEncoding('utf8');
process.stdin.on('data', (d) => (input += d));
process.stdin.on('end', () => {
  appendFileSync(join(dir, 'calls.jsonl'), JSON.stringify({ args, input, key: process.env.PRESENTER_MEKO_KEY ?? null }) + '\n');
  const answers = JSON.parse(readFileSync(join(dir, 'answers.json'), 'utf8'));
  const a = answers.find((x) => input.includes(x.match));
  const session = args.includes('--session-id') ? args[args.indexOf('--session-id') + 1] : args.includes('--fork-session') ? `fork-${Math.random().toString(16).slice(2)}` : args[args.indexOf('--resume') + 1];
  const out = (e) => process.stdout.write(JSON.stringify(e) + '\n');
  out({ type: 'system', subtype: 'init', session_id: session });
  out({ type: 'assistant', session_id: session, message: { content: [{ type: 'tool_use', name: 'mcp__presenter_meko__context_search', input: { query: 'xcluster lag' } }] } });
  if (!a) {
    out({ type: 'result', is_error: true, result: 'no answer for this message', session_id: session });
    process.exit(1);
  }
  if (a.error) {
    out({ type: 'result', is_error: true, result: a.error, session_id: session });
    process.exit(1);
  }
  for (const [path, base64] of Object.entries(a.files ?? {})) {
    mkdirSync(dirname(join(process.cwd(), path)), { recursive: true });
    writeFileSync(join(process.cwd(), path), Buffer.from(base64, 'base64'));
  }
  out({ type: 'result', subtype: 'success', is_error: false, result: '', structured_output: a.answer, session_id: session });
});
