// A row of mutually exclusive options, such as a scope or a chart mode.

import { cx } from "./ui";

export function Segmented<T extends string>({
  options,
  value,
  onChange,
  size = "md",
}: {
  options: readonly { id: T; label: string }[];
  value: T;
  onChange: (v: T) => void;
  size?: "sm" | "md";
}) {
  return (
    <div className={cx("no-scrollbar flex max-w-full shrink-0 overflow-x-auto rounded-lg border bg-card p-0.5", size === "sm" ? "text-xs" : "text-sm")}>
      {options.map((o) => (
        <button
          key={o.id}
          type="button"
          onClick={() => onChange(o.id)}
          className={cx(
            "shrink-0 whitespace-nowrap rounded-md",
            size === "sm" ? "px-2.5 py-1" : "px-3 py-1",
            value === o.id ? "bg-primary text-primary-foreground" : "text-muted-foreground hover:text-foreground",
          )}
        >
          {o.label}
        </button>
      ))}
    </div>
  );
}
