import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import { apiDelete, apiGet, apiUpload, ApiRequestError } from '@/lib/api';
import type { FileListResponse, UploadResponse } from '@/lib/api';
import { useUIStore } from '@/stores/ui-store';

export function useWorkspace(path?: string) {
  const queryClient = useQueryClient();

  const filesQuery = useQuery({
    queryKey: ['workspace', path ?? ''],
    queryFn: () =>
      apiGet<FileListResponse>('/api/v1/workspace/files', {
        params: { path: path || undefined },
      }),
  });

  const uploadMutation = useMutation({
    mutationFn: async ({ file, subdir }: { file: File; subdir?: string }) => {
      return apiUpload<UploadResponse>('/api/v1/workspace/upload', {
        file,
        subdir,
      });
    },
    onSuccess: () => {
      queryClient.invalidateQueries({ queryKey: ['workspace'] });
    },
  });

  return {
    files: filesQuery.data?.files ?? [],
    isLoading: filesQuery.isLoading,
    error: filesQuery.error,
    uploadFile: uploadMutation.mutateAsync,
    isUploading: uploadMutation.isPending,
  };
}

// Delete is a dedicated hook so each file/dir node owns its own mutation.
//
// A 404 is treated as "already gone": the entry vanished server-side between
// listing and click (e.g. an agent's xizhi tool restructured the directory),
// so we still refresh the tree and clear the active selection rather than
// leaving a stale row. Only genuine failures (403/500/network) leave the entry.
export function useDeleteFile() {
  const queryClient = useQueryClient();

  const syncDeleted = (path: string) => {
    queryClient.removeQueries({ queryKey: ['file-content', path] });
    queryClient.removeQueries({ queryKey: ['image-blob', path] });
    queryClient.removeQueries({ queryKey: ['download-url', path] });
    queryClient.invalidateQueries({ queryKey: ['workspace'] });

    const ui = useUIStore.getState();
    const active = ui.activeFilePath;
    if (active && (active === path || active.startsWith(`${path}/`))) {
      ui.setActiveFile(null);
    }
  };

  return useMutation({
    mutationFn: (path: string) =>
      apiDelete<void>(`/api/v1/workspace/files/${encodeURIComponent(path)}`),
    onSuccess: (_data, path) => syncDeleted(path),
    onError: (err, path) => {
      if (err instanceof ApiRequestError && err.code === 'NOT_FOUND') {
        syncDeleted(path);
      }
    },
  });
}
