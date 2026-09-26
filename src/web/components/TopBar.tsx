import type { ReactNode } from 'react';

/**
 * The phone layout's app bar: the projects drawer, what you're looking at, and its actions.
 * The session screen leaves out the title and puts the session tabs first in `children`.
 */
export function TopBar({
  sub,
  title,
  onMenu,
  children,
}: {
  sub?: ReactNode;
  title?: ReactNode;
  onMenu: () => void;
  children?: ReactNode;
}) {
  return (
    <header className="topbar">
      <button className="icon-btn topbar-menu" onClick={onMenu} aria-label="Projects and settings">
        <svg width="20" height="20" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round" aria-hidden="true">
          <path d="M4 7h16 M4 12h16 M4 17h16" />
        </svg>
      </button>
      {title && (
        <span className="topbar-title">
          {sub && <span className="topbar-sub">{sub}</span>}
          <b>{title}</b>
        </span>
      )}
      {children}
    </header>
  );
}
