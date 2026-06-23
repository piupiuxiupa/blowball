import { useMutation, useQueryClient } from '@tanstack/react-query';
import { apiPostStream } from '@/lib/api';
import { parseSSEStream } from '@/lib/sse';
import { useAuthStore } from '@/stores/auth-store';
import { useUIStore } from '@/stores/ui-store';
import type { SendMessageRequest } from '@/lib/api';

interface StreamEvent {
  type: string;
  agent: string;
  content?: string;
}

export function useSendMessage() {
  const queryClient = useQueryClient();
  const { token } = useAuthStore();
  const { appendToken, clearStreaming, appendReasoningToken, clearStreamingReasoning, setAgentStatus } = useUIStore();

  return useMutation({
    mutationFn: async ({
      sessionId,
      content,
    }: {
      sessionId: string;
      content: string;
    }) => {
      if (!token) throw new Error('Not authenticated');

      const response = await apiPostStream(
        `/api/v1/sessions/${encodeURIComponent(sessionId)}/messages`,
        {
          body: { content } as SendMessageRequest,
          token,
        }
      );

      for await (const sseEvent of parseSSEStream(response)) {
        let payload: StreamEvent;
        try {
          payload = JSON.parse(sseEvent.data) as StreamEvent;
        } catch {
          continue;
        }

        switch (payload.type) {
          case 'agent_start':
            setAgentStatus(sessionId, payload.agent, 'running');
            break;
          case 'token':
            appendToken(sessionId, payload.content ?? '');
            break;
          case 'reasoning':
            appendReasoningToken(sessionId, payload.content ?? '');
            break;
          case 'tool_call':
            setAgentStatus(sessionId, payload.agent, 'tool_call');
            break;
          case 'agent_end':
            setAgentStatus(sessionId, payload.agent, 'idle');
            break;
          case 'agent_error':
            setAgentStatus(sessionId, payload.agent, 'error');
            break;
          case 'done':
            clearStreaming(sessionId);
            clearStreamingReasoning(sessionId);
            break;
        }
      }

      clearStreaming(sessionId);
      clearStreamingReasoning(sessionId);
    },
    onSuccess: (_, { sessionId }) => {
      queryClient.invalidateQueries({ queryKey: ['messages', sessionId] });
      queryClient.invalidateQueries({ queryKey: ['sessions'] });
    },
    onError: (_, { sessionId }) => {
      setAgentStatus(sessionId, 'system', 'error');
    },
  });
}
