/**
 * Application base path injected by Vite (import.meta.env.BASE_URL).
 * Always ends with a trailing slash when configured, e.g. "/blowball/".
 * Root deployment is represented as "/".
 */
export const BASE_PATH = import.meta.env.BASE_URL || '/';

/**
 * React Router basename expects a leading slash and no trailing slash,
 * except for the root basename which is "/".
 */
export function getRouterBasename(base: string = BASE_PATH): string {
  if (!base || base === '/') return '/';
  return base.replace(/\/+$/, '');
}
