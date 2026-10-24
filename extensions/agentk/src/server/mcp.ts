import { request } from 'node:http';
import { createInterface } from 'node:readline';
import { TOOL_DEFS, TOOLS_SERVER, TOOLS_VERSION } from './tooldefs.js';

/**
 * `agentk mcp`: AgentK's session tools as a stdio MCP server, started by a session's CLI (Claude
 * Code's `--mcp-config`, Kiro's `agentk` agent) as its own child. It lists the tools and forwards
 * each call to the server over the hook socket with the session's token, both from the environment
 * the session's CLI passes on, so the server knows which session asked. It loads no config, no
 * database and nothing of the server's: the server checks and runs every call.
 *
 * It speaks MCP's few stdio messages itself (newline-delimited JSON-RPC: initialize, tools/list,
 * tools/call, ping) rather than through the SDK, whose schemas cost about 30 MB in every session.
 */

/** How long a call may take: starting a session on a repo AgentK has to clone first can take minutes. */
const CALL_TIMEOUT_MS = 10 * 60 * 1000;
/** The MCP versions it speaks, newest first: a client asking for another gets the newest. */
const PROTOCOLS = ['2025-06-18', '2025-03-26', '2024-11-05'];

export interface ToolsLink {
  /** The hook socket (AK_HOOK_SOCK) and the session's hook token (AK_HOOK_TOKEN). */
  sock: string | undefined;
  token: string | undefined;
}

/** A tool call's result, as MCP has it. */
export interface ToolResult {
  content: { type: 'text'; text: string }[];
  isError: boolean;
}

const result = (text: string, isError: boolean): ToolResult => ({ content: [{ type: 'text', text }], isError });
const fail = (text: string) => result(text, true);

/** One call to the server's `/tools`, read as the tool's result. Never throws: a failure is the result's text. */
export function callServer(link: ToolsLink, tool: string, input: unknown): Promise<ToolResult> {
  if (!link.sock || !link.token) {
    return Promise.resolve(fail("This isn't an AgentK session, so AgentK's session tools can't be used here."));
  }
  const body = JSON.stringify({ v: TOOLS_VERSION, tool, input: input ?? {} });
  return new Promise((resolve) => {
    const req = request({
      socketPath: link.sock, path: '/tools', method: 'POST', timeout: CALL_TIMEOUT_MS,
      headers: { 'Content-Type': 'application/json', 'Content-Length': Buffer.byteLength(body), 'X-AK-Token': link.token },
    }, (res) => {
      let text = '';
      res.setEncoding('utf8');
      res.on('data', (c: string) => (text += c));
      res.on('end', () => {
        let reply: { text?: unknown; isError?: unknown } | null = null;
        try {
          const v: unknown = JSON.parse(text);
          if (v && typeof v === 'object') reply = v;
        } catch {
          /* said below */
        }
        if (res.statusCode === 200 && typeof reply?.text === 'string') resolve(result(reply.text, reply.isError === true));
        else if (res.statusCode === 403) resolve(fail('AgentK no longer runs this session, so its session tools are off.'));
        else if (res.statusCode === 404) resolve(fail('This AgentK server has no session tools. Update AgentK.'));
        else resolve(fail(`AgentK couldn't answer (HTTP ${res.statusCode ?? 'error'}). Try again.`));
      });
    });
    req.on('timeout', () => req.destroy(new Error('timed out')));
    req.on('error', (e) => resolve(fail(`AgentK isn't reachable (${e.message}). Is AgentK running? Try again once it is.`)));
    req.end(body);
  });
}

interface Message {
  jsonrpc?: unknown;
  id?: string | number | null;
  method?: unknown;
  params?: Record<string, unknown>;
}

/**
 * Answers one JSON-RPC message from the client: the response to send, or null for a notification
 * (or a response, which a client doesn't send this server).
 */
export async function handle(link: ToolsLink, msg: Message, version = '1'): Promise<Record<string, unknown> | null> {
  if (msg.id === undefined || msg.id === null || typeof msg.method !== 'string') return null;
  const reply = (body: Record<string, unknown>) => ({ jsonrpc: '2.0', id: msg.id, ...body });
  switch (msg.method) {
    case 'initialize': {
      const asked = msg.params?.protocolVersion;
      return reply({ result: {
        protocolVersion: typeof asked === 'string' && PROTOCOLS.includes(asked) ? asked : PROTOCOLS[0],
        capabilities: { tools: {} },
        serverInfo: { name: TOOLS_SERVER, version },
      } });
    }
    case 'ping':
      return reply({ result: {} });
    case 'tools/list':
      return reply({ result: { tools: TOOL_DEFS.map((t) => ({
        name: t.name, description: t.description, inputSchema: t.inputSchema,
        annotations: { readOnlyHint: !!t.readOnly, destructiveHint: false, openWorldHint: false },
      })) } });
    case 'tools/call': {
      const name = msg.params?.name;
      if (typeof name !== 'string') return reply({ error: { code: -32602, message: 'tools/call needs a tool name.' } });
      return reply({ result: await callServer(link, name, msg.params?.arguments) });
    }
    default:
      return reply({ error: { code: -32601, message: `No method ${msg.method}.` } });
  }
}

/** Runs the server on stdio until the session's CLI closes it: a message per line in, one per line out. */
export async function runMcp(version: string): Promise<void> {
  const link = { sock: process.env.AK_HOOK_SOCK, token: process.env.AK_HOOK_TOKEN };
  const send = (m: unknown) => process.stdout.write(`${JSON.stringify(m)}\n`);
  for await (const line of createInterface({ input: process.stdin, crlfDelay: Infinity })) {
    if (!line.trim()) continue;
    let msg: Message;
    try {
      msg = JSON.parse(line) as Message;
    } catch {
      send({ jsonrpc: '2.0', id: null, error: { code: -32700, message: 'That line isn\'t JSON.' } });
      continue;
    }
    // Calls run side by side (a client can list the tools while a start runs); each answer goes when it's ready.
    void handle(link, msg && typeof msg === 'object' ? msg : {}, version)
      .then((r) => (r ? send(r) : undefined))
      .catch((e: unknown) => send({ jsonrpc: '2.0', id: msg.id ?? null, error: { code: -32603, message: e instanceof Error ? e.message : String(e) } }));
  }
}
