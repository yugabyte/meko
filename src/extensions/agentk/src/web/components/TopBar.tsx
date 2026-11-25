import type { ReactNode } from 'react';

/**
 * The phone layout's app bar: the projects drawer, what you're looking at, and its actions.
 * The session screen puts the session tabs first in `children` and leaves out the title, unless the drawer
 * lists the sessions in place of the tabs.
 */
export function TopBar({
  sub,
  title,
  icon,
  onMenu,
  menuDot = false,
  children,
}: {
  sub?: ReactNode;
  title?: ReactNode;
  /** Drawn before the title, outside its ellipsis, so a status dot's halo isn't clipped. */
  icon?: ReactNode;
  onMenu: () => void;
  /** Another session the drawer lists needs you. */
  menuDot?: boolean;
  children?: ReactNode;
}) {
  return (
    <header className="topbar">
      <button
        className="icon-btn topbar-menu"
        onClick={onMenu}
        aria-label={menuDot ? 'Projects and settings, a session needs you' : 'Projects and settings'}
      >
        <svg width="20" height="20" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round" aria-hidden="true">
          <path d="M4 7h16 M4 12h16 M4 17h16" />
        </svg>
        {menuDot && <span className="topbar-dot" aria-hidden="true" />}
      </button>
      {title && (
        <span className="topbar-title">
          {sub && <span className="topbar-sub">{sub}</span>}
          <span className="topbar-name">
            {icon}
            <b>{title}</b>
          </span>
        </span>
      )}
      {children}
    </header>
  );
}
