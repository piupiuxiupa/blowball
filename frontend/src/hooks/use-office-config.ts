import { useQuery } from '@tanstack/react-query';
import { fetchOfficeConfig } from '@/lib/onlyoffice';

// useOfficeConfig fetches the backend-built + signed OnlyOffice editor config.
//
// `nonce` is part of the query key so the Refresh button (which bumps nonce and
// remounts the editor) forces a fresh fetch — the backend mints a new random
// document.key on every request, which makes OnlyOffice re-convert instead of
// serving a cached (stale) document. staleTime is infinite because each nonce is
// a one-shot fetch; refetching the same nonce would be pointless.
export function useOfficeConfig(path: string, nonce: number) {
  return useQuery({
    queryKey: ['office-config', path, nonce],
    queryFn: () => fetchOfficeConfig(path),
    enabled: !!path,
    staleTime: Infinity,
    retry: false,
  });
}
