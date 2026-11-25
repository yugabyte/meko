#!/usr/bin/env node
// Stand-in for the Meko plugin's capture.js in tests. It records each run
// (argv, the hook input on stdin, the Meko env) to $FAKE_CAPTURE_LOG, taking
// $FAKE_CAPTURE_MS to finish.
import { appendFileSync, readFileSync } from 'node:fs';

const log = process.env.FAKE_CAPTURE_LOG ?? `${process.env.FAKE_CLAUDE_DIR}/capture.jsonl`;
const input = JSON.parse(readFileSync(0, 'utf8'));
const start = Date.now();
await new Promise((r) => setTimeout(r, Number(process.env.FAKE_CAPTURE_MS ?? 0)));
appendFileSync(log, JSON.stringify({
  args: process.argv.slice(2),
  input,
  env: { MEKO_MCP_URL: process.env.MEKO_MCP_URL, MEKO_API_KEY: process.env.MEKO_API_KEY },
  start,
  end: Date.now(),
}) + '\n');
