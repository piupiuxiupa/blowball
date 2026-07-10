import { useState, useCallback } from 'react';

function readStoredWidth(key: string, fallback: number): number {
  if (typeof window === 'undefined') return fallback;
  try {
    const raw = localStorage.getItem(key);
    if (!raw) return fallback;
    const value = Number.parseInt(raw, 10);
    return Number.isFinite(value) ? value : fallback;
  } catch {
    return fallback;
  }
}

function storeWidth(key: string, value: number) {
  if (typeof window === 'undefined') return;
  try {
    localStorage.setItem(key, String(value));
  } catch {
    // ignore storage errors
  }
}

export function useResizableWidth(
  storageKey: string,
  fallback: number,
  options: { min?: number; max?: number } = {}
) {
  const { min = 160, max = 800 } = options;
  const [width, setWidth] = useState(() => readStoredWidth(storageKey, fallback));

  const adjust = useCallback(
    (delta: number) => {
      setWidth((prev) => {
        const next = Math.max(min, Math.min(max, prev + delta));
        storeWidth(storageKey, next);
        return next;
      });
    },
    [min, max, storageKey]
  );

  return [width, adjust] as const;
}
