// How to point an agent at this datapack: the MCP URL, the token, and the commands that register
// Meko with Claude Code or any other MCP client.

import { useState } from "react";
import { Eye, EyeOff } from "lucide-react";
import { api } from "../lib/api";
import type { Datapack } from "../lib/types";
import { useLoad } from "../lib/useLoad";
import { CopyBlock } from "../components/CopyBlock";
import { Button, Card, Code, ErrorNote, Loading } from "../components/ui";

export function ConnectPage({ datapack }: { datapack: Datapack }) {
  const { data, error, loading } = useLoad(() => api.connect(datapack.datapack_id), [datapack.datapack_id]);
  const [show, setShow] = useState(false);
  if (loading && !data) return <Loading />;
  if (error) return <ErrorNote error={error} />;
  if (!data) return null;

  // The token comes only to a signed-in owner: with local access (anyone on this computer) or auth
  // off, this page gets none, and `meko token` on the host prints it.
  const token = data.mcp_api_key;
  const masked = token ? `${token.slice(0, 6)}${"•".repeat(20)}` : "(not shown here: run meko token on this computer)";
  const header = token ? ` --header "Authorization: Bearer ${token}"` : "";
  const claude = token
    ? `claude mcp add --transport http --scope user meko ${data.mcp_server_url}${header}`
    : "meko token --mcp   # prints the claude mcp add command, with the token";

  return (
    <div className="max-w-3xl space-y-4">
      <Card className="space-y-3 p-4">
        <h3 className="font-medium">Claude Code</h3>
        <p className="text-sm text-muted-foreground">
          Registers Meko for every repo (drop <Code>--scope user</Code> for this folder only). Its tools show up as{" "}
          <Code>mcp__meko__*</Code>. Pass <Code>datapack_id</Code> {datapack.datapack_id} to use this datapack.
        </p>
        <CopyBlock text={claude} display={show || !token ? claude : claude.replace(token, masked)} />
      </Card>

      <Card className="space-y-3 p-4">
        <h3 className="font-medium">Any MCP client</h3>
        <p className="text-sm text-muted-foreground">Streamable HTTP, with the token as a Bearer header.</p>
        <div className="space-y-2 text-sm">
          <div className="text-xs font-medium text-muted-foreground">URL</div>
          <CopyBlock text={data.mcp_server_url} />
          <div className="flex items-center justify-between">
            <span className="text-xs font-medium text-muted-foreground">Token</span>
            <Button size="sm" variant="ghost" onClick={() => setShow(!show)}>
              {show ? <EyeOff className="size-3.5" /> : <Eye className="size-3.5" />} {show ? "Hide" : "Show"}
            </Button>
          </div>
          <CopyBlock text={token} display={show ? token : masked} />
        </div>
      </Card>

      <Card className="space-y-3 p-4">
        <h3 className="font-medium">From the terminal</h3>
        <CopyBlock text={`meko search "what do I like" --datapack ${datapack.datapack_name}`} />
        <CopyBlock text={`meko upload ~/notes.pdf --datapack ${datapack.datapack_name}`} />
      </Card>
    </div>
  );
}
