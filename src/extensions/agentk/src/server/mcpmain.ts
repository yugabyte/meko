// The entry sessions start `me_koder mcp` by (tools.ts, toolsCommand): the tools' MCP server alone,
// without the CLI's imports, which load the whole server.
import { runMcp } from './mcp.js';

runMcp('1').catch((e: unknown) => {
  console.error(e instanceof Error ? e.message : String(e));
  process.exit(1);
});
