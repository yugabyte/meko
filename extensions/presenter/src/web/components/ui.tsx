import type { ButtonHTMLAttributes, ReactNode } from 'react';

export const cx = (...c: (string | false | null | undefined)[]) => c.filter(Boolean).join(' ');

export function Button({ variant = 'default', className, ...p }: ButtonHTMLAttributes<HTMLButtonElement> & { variant?: 'default' | 'primary' | 'ghost' | 'danger' }) {
  return (
    <button
      type="button"
      {...p}
      className={cx(
        'inline-flex items-center gap-1.5 whitespace-nowrap rounded-md border px-2.5 py-1 text-sm disabled:cursor-not-allowed disabled:opacity-50',
        variant === 'primary' && 'border-primary bg-primary font-semibold text-primary-foreground hover:brightness-105',
        variant === 'default' && 'bg-card hover:bg-accent',
        variant === 'ghost' && 'border-transparent hover:bg-accent',
        variant === 'danger' && 'border-destructive/50 text-destructive hover:bg-destructive/10',
        className,
      )}
    />
  );
}

export function Label({ children }: { children: ReactNode }) {
  return <span className="text-[11px] font-semibold uppercase tracking-wide text-muted-foreground">{children}</span>;
}

export function Pill({ tone = 'info', children }: { tone?: 'info' | 'ok' | 'warn' | 'bad'; children: ReactNode }) {
  return (
    <span className={cx(
      'inline-block rounded-full px-2 py-0.5 font-mono text-[11px]',
      tone === 'info' && 'bg-info/15 text-info',
      tone === 'ok' && 'bg-success/15 text-success',
      tone === 'warn' && 'bg-warning/15 text-warning',
      tone === 'bad' && 'bg-destructive/15 text-destructive',
    )}
    >
      {children}
    </span>
  );
}

export function ErrorLine({ error }: { error: string | null }) {
  if (!error) return null;
  return <p role="alert" className="rounded-md border border-destructive/40 bg-destructive/10 px-3 py-2 text-sm text-destructive">{error}</p>;
}

export const inputClass = 'w-full rounded-md border bg-card px-2 py-1 text-sm';
