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
    <div className={cx("flex shrink-0 rounded-lg border bg-card p-0.5", size === "sm" ? "text-xs" : "text-sm")}>
      {options.map((o) => (
        <button
          key={o.id}
          type="button"
          onClick={() => onChange(o.id)}
          className={cx(
            "rounded-md",
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
