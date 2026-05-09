import { cn } from "@/lib/utils";

export function MemoryIcon({ className }: { className?: string }) {
  return (
    <span role="img" aria-label="memory" className={cn("inline-block leading-none", className)}>
      🧠
    </span>
  );
}
