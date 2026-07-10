import { useRef, useCallback } from 'react';
import { cn } from '@/lib/utils';

interface ResizerProps {
  direction?: 'horizontal' | 'vertical';
  min?: number;
  max?: number;
  onResize: (delta: number) => void;
  className?: string;
}

export function Resizer({ direction = 'horizontal', onResize, className }: ResizerProps) {
  const startRef = useRef(0);
  const isDraggingRef = useRef(false);

  const handlePointerDown = useCallback(
    (e: React.PointerEvent) => {
      e.preventDefault();
      isDraggingRef.current = true;
      startRef.current = direction === 'horizontal' ? e.clientX : e.clientY;
      (e.target as Element).setPointerCapture(e.pointerId);
      document.body.style.userSelect = 'none';
      document.body.style.cursor = direction === 'horizontal' ? 'col-resize' : 'row-resize';
    },
    [direction]
  );

  const handlePointerMove = useCallback(
    (e: React.PointerEvent) => {
      if (!isDraggingRef.current) return;
      const current = direction === 'horizontal' ? e.clientX : e.clientY;
      const delta = current - startRef.current;
      if (delta !== 0) {
        onResize(delta);
        startRef.current = current;
      }
    },
    [direction, onResize]
  );

  const handlePointerUp = useCallback((e: React.PointerEvent) => {
    if (!isDraggingRef.current) return;
    isDraggingRef.current = false;
    (e.target as Element).releasePointerCapture(e.pointerId);
    document.body.style.userSelect = '';
    document.body.style.cursor = '';
  }, []);

  return (
    <div
      role="separator"
      aria-orientation={direction}
      onPointerDown={handlePointerDown}
      onPointerMove={handlePointerMove}
      onPointerUp={handlePointerUp}
      onPointerCancel={handlePointerUp}
      className={cn(
        'shrink-0 bg-border hover:bg-primary/30 active:bg-primary/50',
        direction === 'horizontal'
          ? 'w-1 cursor-col-resize'
          : 'h-1 cursor-row-resize',
        className
      )}
    />
  );
}
