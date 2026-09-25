#!/usr/bin/env node
// Stand-in for the Meko plugin's capture.js in tests. It records each run
// (argv, the hook input on stdin, the Meko env) to $FAKE_CAPTURE_LOG, taking
// $FAKE_CAPTURE_MS to finish, and writes a watermark like the plugin's.
import { appendFileSync, existsSync, mkdirSync, readFileSync, writeFileSync } from 'node:fs';
import { basename, join } from 'node:path';

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

const wmDir = process.env.MEKO_WATERMARK_DIR;
if (wmDir) {
  const id = basename(input.transcript_path, '.jsonl');
  const wm = join(wmDir, `${id}.watermark.json`);
  if (!existsSync(wm)) {
    mkdirSync(wmDir, { recursive: true });
    writeFileSync(wm, JSON.stringify({ conversation_id: id.replace(/-/g, ''), last_line_number: 0, agent_id: 'claude_code:fake', datapack_id: 'dp-fake', datapack_name: 'fake' }));
  }
}
