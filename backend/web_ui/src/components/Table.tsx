// A table from column definitions: each column's header and how to draw a row's cell.

import type { ReactNode } from "react";
import { cx } from "./ui";

export interface Column<T> {
  label: ReactNode;
  cell: (row: T) => ReactNode;
  right?: boolean;
  className?: string;
}

export function Table<T>({
  rows,
  columns,
  rowKey,
  onRow,
}: {
  rows: T[];
  columns: Column<T>[];
  rowKey: (row: T, i: number) => string;
  onRow?: (row: T) => void;
}) {
  return (
    <div className="overflow-x-auto">
      <table className="w-full text-sm">
        <thead className="bg-muted text-xs text-muted-foreground">
          <tr>
            {columns.map((c, i) => (
              <th key={i} className={cx("px-3 py-2 font-medium", c.right ? "text-right" : "text-left")}>
                {c.label}
              </th>
            ))}
          </tr>
        </thead>
        <tbody className="divide-y">
          {rows.map((r, i) => (
            <tr
              key={rowKey(r, i)}
              onClick={onRow && (() => onRow(r))}
              className={cx("group", onRow && "cursor-pointer hover:bg-accent")}
            >
              {columns.map((c, j) => (
                <td key={j} className={cx("px-3 py-2", c.right && "text-right tabular-nums", c.className)}>
                  {c.cell(r)}
                </td>
              ))}
            </tr>
          ))}
        </tbody>
      </table>
    </div>
  );
}
