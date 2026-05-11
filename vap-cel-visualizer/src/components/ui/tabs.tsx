import * as React from 'react';
import { cn } from '../../lib/utils';

export function Tabs({
  tabs,
  value,
  onValueChange,
  className,
}: {
  tabs: { value: string; label: string }[];
  value: string;
  onValueChange: (value: string) => void;
  className?: string;
}) {
  return (
    <div className={cn('flex flex-wrap gap-1 rounded-md border border-line bg-panel2 p-1', className)}>
      {tabs.map((tab) => (
        <button
          key={tab.value}
          type="button"
          onClick={() => onValueChange(tab.value)}
          className={cn(
            'rounded px-3 py-1.5 text-xs font-medium text-stone-400 transition-colors hover:text-stone-100',
            value === tab.value && 'bg-stone-700 text-white',
          )}
        >
          {tab.label}
        </button>
      ))}
    </div>
  );
}
