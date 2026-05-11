import * as React from 'react';
import { cn } from '../../lib/utils';

type BadgeTone = 'cyan' | 'green' | 'amber' | 'rose' | 'slate';

const tones: Record<BadgeTone, string> = {
  cyan: 'border-aqua/35 bg-aqua/12 text-orange-100',
  green: 'border-mint/35 bg-mint/12 text-lime-100',
  amber: 'border-amber/35 bg-amber/12 text-amber-100',
  rose: 'border-rose/35 bg-rose/12 text-red-100',
  slate: 'border-stone-500/30 bg-stone-900 text-stone-200',
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
