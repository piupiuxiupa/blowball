import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import { apiGet, apiUpload } from '@/lib/api';
import type { FileListResponse, UploadResponse } from '@/lib/api';

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
