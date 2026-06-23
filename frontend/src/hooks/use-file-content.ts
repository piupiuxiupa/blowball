import { useQuery } from '@tanstack/react-query';
import { apiGet, getApiBase, getToken } from '@/lib/api';
import type { FileContentResponse } from '@/lib/api';

export function useFileContent(path: string | null) {
  return useQuery({
    queryKey: ['file-content', path],
    queryFn: async () => {
      if (!path) return null;
      try {
        return await apiGet<FileContentResponse>(
          `/api/v1/workspace/files/${encodeURIComponent(path)}/content`
        );
      } catch (error) {
        // If binary or directory, return a sentinel so caller can try download
        return { path, content: null, error: error as Error } as {
          path: string;
          content: null;
          error: Error;
        };
      }
    },
    enabled: !!path,
  });
}

export function useImageBlob(path: string | null) {
  return useQuery({
    queryKey: ['image-blob', path],
    queryFn: async () => {
      if (!path) return null;
      const token = getToken();
      const response = await fetch(
        `${getApiBase()}/api/v1/workspace/files/${encodeURIComponent(path)}`,
        {
          headers: token ? { Authorization: `Bearer ${token}` } : {},
        }
      );
      if (!response.ok) throw new Error('Failed to load image');
      const blob = await response.blob();
      return URL.createObjectURL(blob);
    },
    enabled: !!path,
    staleTime: Infinity,
  });
}

export function useDownloadFile(path: string | null) {
  return async () => {
    if (!path) return;
    const token = getToken();
    const response = await fetch(
      `${getApiBase()}/api/v1/workspace/files/${encodeURIComponent(path)}`,
      {
        headers: token ? { Authorization: `Bearer ${token}` } : {},
      }
    );
    if (!response.ok) throw new Error('Failed to download file');
    const blob = await response.blob();
    const url = URL.createObjectURL(blob);
    const a = document.createElement('a');
    a.href = url;
    a.download = path.split('/').pop() || 'download';
    document.body.appendChild(a);
    a.click();
    document.body.removeChild(a);
    URL.revokeObjectURL(url);
  };
}

export function useDownloadUrl(path: string | null) {
  return useQuery({
    queryKey: ['download-url', path],
    queryFn: async () => {
      if (!path) return null;
      const token = getToken();
      const response = await fetch(
        `${getApiBase()}/api/v1/workspace/files/${encodeURIComponent(path)}`,
        {
          headers: token ? { Authorization: `Bearer ${token}` } : {},
        }
      );
      if (!response.ok) throw new Error('Failed to fetch file');
      const blob = await response.blob();
      return URL.createObjectURL(blob);
    },
    enabled: !!path,
    staleTime: Infinity,
  });
}
