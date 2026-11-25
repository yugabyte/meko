// A block of monospace text with a copy button, for commands and tokens.

import { useState } from "react";
import { Check, Copy } from "lucide-react";
import { Button } from "./ui";

export function CopyBlock({ text, display }: { text: string; display?: string }) {
  const [copied, setCopied] = useState(false);
  const copy = async () => {
    await navigator.clipboard.writeText(text);
    setCopied(true);
    setTimeout(() => setCopied(false), 1500);
  };
  return (
    <div className="flex items-start gap-2 rounded-lg border bg-muted p-3">
      <pre className="min-w-0 flex-1 whitespace-pre-wrap break-all font-mono text-xs">{display ?? text}</pre>
      <Button size="sm" variant="ghost" onClick={copy} title="Copy">
        {copied ? <Check className="size-3.5 text-success" /> : <Copy className="size-3.5" />}
      </Button>
    </div>
  );
}
