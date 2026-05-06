// UI for inviting an agent via access token (MCP / CI). Token issuance is not wired yet — form validates UX only.
import { useState } from "react";
import { Bot, Shield } from "lucide-react";
import { Button } from "@/components/ui/button";
import { Checkbox } from "@/components/ui/checkbox";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select";
import { cn } from "@/lib/utils";
import {
  DATAPACK_SHARE_GRANT_CONTRIBUTOR,
  DATAPACK_SHARE_GRANT_MAINTAINER,
  DATAPACK_SHARE_GRANT_VIEWER,
  type DatapackShareGrant,
} from "@/lib/datapackShareGrants";
import { toast } from "@/hooks/use-toast";

const AGENT_ROLE_OPTIONS: { value: DatapackShareGrant; label: string }[] = [
  { value: DATAPACK_SHARE_GRANT_VIEWER, label: "Can view" },
  { value: DATAPACK_SHARE_GRANT_CONTRIBUTOR, label: "Can edit memories" },
  { value: DATAPACK_SHARE_GRANT_MAINTAINER, label: "Admin" },
];

type ExpiresChoice = "24h" | "7d" | "30d" | "never";

const SCOPE_ROWS: {
  id: string;
  label: string;
  description: string;
  defaultOn: boolean;
  column: 1 | 2;
}[] = [
  { id: "memory.read", label: "memory.read", description: "Read own memories", defaultOn: true, column: 1 },
  { id: "ko.read", label: "ko.read", description: "Read shared knowledge (Ko)", defaultOn: true, column: 1 },
  { id: "ko.write", label: "ko.write", description: "Directly write to Ko", defaultOn: false, column: 1 },
  { id: "memory.write", label: "memory.write", description: "Write own memories", defaultOn: true, column: 2 },
  { id: "ko.propose", label: "ko.propose", description: "Propose Me → Ko promotions", defaultOn: true, column: 2 },
  { id: "memory.read_all", label: "memory.read_all", description: "Read other agents' memories", defaultOn: false, column: 2 },
];

const EXPIRES_OPTIONS: { id: ExpiresChoice; label: string }[] = [
  { id: "24h", label: "24 hours" },
  { id: "7d", label: "7 days" },
  { id: "30d", label: "30 days" },
  { id: "never", label: "Never" },
];

export function InviteAgentByTokenSection() {
  const [agentName, setAgentName] = useState("");
  const [role, setRole] = useState<DatapackShareGrant>(DATAPACK_SHARE_GRANT_CONTRIBUTOR);
  const [scopes, setScopes] = useState<Record<string, boolean>>(() =>
    Object.fromEntries(SCOPE_ROWS.map((r) => [r.id, r.defaultOn])),
  );
  const [expires, setExpires] = useState<ExpiresChoice>("30d");

  const toggleScope = (id: string) => {
    setScopes((prev) => ({ ...prev, [id]: !prev[id] }));
  };

  const canSubmit = agentName.trim().length > 0;

  const handleGenerate = () => {
    if (!canSubmit) return;
    toast({
      title: "Agent tokens coming soon",
      description: "Token generation for MCP and CI agents is not available yet.",
    });
  };

  return (
    <div className="space-y-6">
      <p className="text-sm leading-relaxed text-muted-foreground">
        Generate an access token so an agent (running in Claude Code, Cursor, Codex, a CI pipeline, or any MCP
        client) can connect to this datapack.
      </p>

      <div className="space-y-2">
        <Label htmlFor="agent-invite-name" className="text-[11px] font-medium uppercase tracking-wide text-muted-foreground">
          Agent name
        </Label>
        <div className="relative">
          <Bot
            className="pointer-events-none absolute left-3 top-1/2 h-4 w-4 -translate-y-1/2 text-muted-foreground"
            aria-hidden
          />
          <Input
            id="agent-invite-name"
            placeholder="e.g. deploy-bot, codegen-agent"
            value={agentName}
            onChange={(e) => setAgentName(e.target.value)}
            className="bg-background/80 pl-9"
            autoComplete="off"
          />
        </div>
        <p className="text-xs text-muted-foreground">Used for memory attribution and audit logs.</p>
      </div>

      <div className="space-y-2">
        <Label htmlFor="agent-invite-role" className="text-[11px] font-medium uppercase tracking-wide text-muted-foreground">
          Role
        </Label>
        <Select value={role} onValueChange={(v) => setRole(v as DatapackShareGrant)}>
          <SelectTrigger id="agent-invite-role" className="bg-background/80">
            <SelectValue />
          </SelectTrigger>
          <SelectContent>
            {AGENT_ROLE_OPTIONS.map((o) => (
              <SelectItem key={o.value} value={o.value}>
                {o.label}
              </SelectItem>
            ))}
          </SelectContent>
        </Select>
        <p className="text-xs text-muted-foreground">Controls what this agent can read and write.</p>
      </div>

      <div className="space-y-3">
        <span className="text-[11px] font-medium uppercase tracking-wide text-muted-foreground">Scopes</span>
        <div className="grid gap-4 sm:grid-cols-2">
          {[1, 2].map((col) => (
            <div key={col} className="space-y-3">
              {SCOPE_ROWS.filter((r) => r.column === col).map((row) => (
                <label
                  key={row.id}
                  className="flex cursor-pointer items-start gap-3 rounded-lg border border-transparent px-1 py-0.5 hover:bg-muted/20"
                >
                  <Checkbox
                    checked={scopes[row.id] ?? false}
                    onCheckedChange={() => toggleScope(row.id)}
                    className="mt-0.5"
                    aria-label={row.label}
                  />
                  <span className="min-w-0">
                    <span className="font-mono text-xs text-foreground">{row.label}</span>
                    <span className="mt-0.5 block text-xs text-muted-foreground">{row.description}</span>
                  </span>
                </label>
              ))}
            </div>
          ))}
        </div>
      </div>

      <div className="space-y-2">
        <span className="text-[11px] font-medium uppercase tracking-wide text-muted-foreground">Expires</span>
        <div className="flex flex-col gap-3 sm:flex-row sm:flex-wrap sm:items-center sm:justify-between">
          <div className="flex flex-wrap gap-2">
            {EXPIRES_OPTIONS.map((opt) => (
              <button
                key={opt.id}
                type="button"
                onClick={() => setExpires(opt.id)}
                className={cn(
                  "rounded-lg border px-3 py-1.5 text-xs font-medium transition-colors",
                  expires === opt.id
                    ? "border-violet-500/70 bg-violet-500/15 text-violet-100"
                    : "border-border bg-muted/20 text-muted-foreground hover:border-muted-foreground/40 hover:bg-muted/30",
                )}
              >
                {opt.label}
              </button>
            ))}
          </div>
          <Button
            type="button"
            variant="secondary"
            className="w-full shrink-0 gap-2 bg-muted/40 hover:bg-muted/60 sm:ml-auto sm:w-auto"
            disabled={!canSubmit}
            onClick={() => handleGenerate()}
          >
            <Shield className="h-4 w-4 shrink-0" aria-hidden />
            Generate token
          </Button>
        </div>
      </div>
    </div>
  );
}
