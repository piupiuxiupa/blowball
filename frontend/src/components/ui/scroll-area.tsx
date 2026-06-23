import * as React from 'react';
import { cn } from '@/lib/utils';

interface ScrollAreaProps extends React.HTMLAttributes<HTMLDivElement> {
  orientation?: 'vertical' | 'horizontal' | 'both';
}

const ScrollArea = React.forwardRef<HTMLDivElement, ScrollAreaProps>(
  ({ className, children, orientation = 'vertical', ...props }, ref) => {
    return (
      <div
        ref={ref}
        className={cn(
          'overflow-auto',
          orientation === 'vertical' && 'overflow-x-hidden',
          orientation === 'horizontal' && 'overflow-y-hidden',
          className
        )}
        {...props}
      >
        {children}
      </div>
    );
  }
);
ScrollArea.displayName = 'ScrollArea';

export { ScrollArea };
