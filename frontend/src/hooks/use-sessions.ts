import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import { apiGet, apiPost } from '@/lib/api';
import type { SessionListResponse, CreateSessionResponse } from '@/lib/api';
import { useUIStore } from '@/stores/ui-store';

export function useSessions() {
  const queryClient = useQueryClient();
  const { setActiveSession } = useUIStore();

  const sessionsQuery = useQuery({
    queryKey: ['sessions'],
    queryFn: () => apiGet<SessionListResponse>('/api/v1/sessions'),
  });

  const createMutation = useMutation({
    mutationFn: () => apiPost<CreateSessionResponse>('/api/v1/sessions'),
    onSuccess: (data) => {
      queryClient.invalidateQueries({ queryKey: ['sessions'] });
      setActiveSession(data.session_id);
    },
  });

  return {
    sessions: sessionsQuery.data?.sessions ?? [],
    isLoading: sessionsQuery.isLoading,
    error: sessionsQuery.error,
    createSession: createMutation.mutateAsync,
    isCreating: createMutation.isPending,
  };
}
