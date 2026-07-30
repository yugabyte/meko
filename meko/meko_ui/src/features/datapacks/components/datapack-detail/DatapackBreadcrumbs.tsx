import { Fragment } from "react";
import { Link } from "react-router-dom";
import { ChevronRight } from "lucide-react";
import { cn } from "@/lib/utils";

export type DatapackBreadcrumbItem = {
  label: string;
  href?: string;
};

type Props = {
  items: DatapackBreadcrumbItem[];
  className?: string;
};

/**
 * Shared trail for datapack routes (overview, detail, share).
 */
export function DatapackBreadcrumbs({ items, className }: Props) {
  return (
    <nav
      className={cn("flex flex-wrap items-center gap-1.5 text-sm text-muted-foreground", className)}
      aria-label="Breadcrumb"
    >
      {items.map((item, index) => (
        <Fragment key={`${item.label}-${index}`}>
          {index > 0 ? (
            <ChevronRight className="h-4 w-4 shrink-0 opacity-60" aria-hidden />
          ) : null}
          {item.href ? (
            <Link to={item.href} className="hover:text-foreground transition-colors truncate max-w-[12rem] sm:max-w-none">
              {item.label}
            </Link>
          ) : (
            <span
              className={cn(
                "truncate max-w-[12rem] sm:max-w-none",
                index === items.length - 1 ? "font-medium text-foreground" : "",
              )}
            >
              {item.label}
            </span>
          )}
        </Fragment>
      ))}
    </nav>
  );
}
