import * as React from 'react';
import { cn } from '../../lib/utils';

type BadgeTone = 'cyan' | 'green' | 'amber' | 'rose' | 'slate';

const tones: Record<BadgeTone, string> = {
  cyan: 'border-cyan-400/30 bg-cyan-400/10 text-cyan-200',
  green: 'border-green-400/30 bg-green-400/10 text-green-200',
  amber: 'border-amber-400/30 bg-amber-400/10 text-amber-200',
  rose: 'border-rose-400/30 bg-rose-400/10 text-rose-200',
  slate: 'border-slate-500/30 bg-slate-800 text-slate-200',
};

export function Badge({
  className,
  tone = 'slate',
  ...props
}: React.HTMLAttributes<HTMLSpanElement> & { tone?: BadgeTone }) {
  return (
    <span
      className={cn(
        'inline-flex items-center rounded-md border px-2 py-0.5 text-xs font-medium',
        tones[tone],
        className,
      )}
      {...props}
    />
  );
}
