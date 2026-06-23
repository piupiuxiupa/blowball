import * as React from 'react';

interface TooltipProps {
  content: React.ReactNode;
  children: React.ReactNode;
  side?: 'top' | 'bottom' | 'left' | 'right';
}

function Tooltip({ content, children }: TooltipProps) {
  const [open, setOpen] = React.useState(false);

  return (
    <div
      className="relative inline-flex"
      onMouseEnter={() => setOpen(true)}
      onMouseLeave={() => setOpen(false)}
      onFocus={() => setOpen(true)}
      onBlur={() => setOpen(false)}
    >
      {children}
      {open && (
        <div className="absolute bottom-full left-1/2 z-50 mb-2 -translate-x-1/2 whitespace-nowrap rounded-md bg-primary px-2 py-1 text-xs text-primary-foreground shadow-md">
          {content}
        </div>
      )}
    </div>
  );
}

export { Tooltip };
