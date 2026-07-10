import { useMutation, useQueryClient } from '@tanstack/react-query';
import { useRef } from 'react';
import { apiPostStream } from '@/lib/api';
import { parseSSEStream } from '@/lib/sse';
import { useAuthStore } from '@/stores/auth-store';
import { useUIStore } from '@/stores/ui-store';
import type { SendMessageRequest, SessionMessagesResponse, Message } from '@/lib/api';

interface StreamEvent {
  type: string;
  agent: string;
  content?: string;
}

function buildOptimisticUserMessage(sessionId: string, content: string, messages: Message[]): Message {
  const now = new Date().toISOString();
  const maxIndex = messages.reduce((max, m) => (m.msg_index > max ? m.msg_index : max), 0);
  return {
    id: -Date.now(),
    session_id: sessionId,
    msg_time: now,
    agent: 'user',
    msg_index: maxIndex + 1,
    role: 'user',
    event_type: 'message',
    content,
    trace_id: '',
    update_time: now,
  };
}

export function useSendMessage() {
  const queryClient = useQueryClient();
  const { token } = useAuthStore();
  const { appendToken, clearStreaming, appendReasoningToken, clearStreamingReasoning, setAgentStatus } = useUIStore();
  const abortControllerRef = useRef<AbortController | null>(null);

  const mutation = useMutation({
    mutationFn: async ({
      sessionId,
      content,
    }: {
      sessionId: string;
      content: string;
    }) => {
      if (!token) throw new Error('Not authenticated');

      abortControllerRef.current = new AbortController();
      try {
        const response = await apiPostStream(
          `/api/v1/sessions/${encodeURIComponent(sessionId)}/messages`,
          {
            body: { content } as SendMessageRequest,
            token,
            signal: abortControllerRef.current.signal,
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
      } finally {
        abortControllerRef.current = null;
      }

      clearStreaming(sessionId);
      clearStreamingReasoning(sessionId);
    },
    onMutate: async ({ sessionId, content }) => {
      const queryKey = ['messages', sessionId];
      await queryClient.cancelQueries({ queryKey });
      const previous = queryClient.getQueryData<SessionMessagesResponse>(queryKey);
      const messages = previous?.messages ?? [];
      const optimistic = buildOptimisticUserMessage(sessionId, content, messages);
      queryClient.setQueryData<SessionMessagesResponse>(queryKey, {
        messages: [...messages, optimistic],
      });
      return { previous };
    },
    onError: (_err, { sessionId }, context) => {
      setAgentStatus(sessionId, 'system', 'error');
      if (context?.previous) {
        queryClient.setQueryData(['messages', sessionId], context.previous);
      }
    },
    onSettled: (_, __, { sessionId }) => {
      queryClient.invalidateQueries({ queryKey: ['messages', sessionId] });
      queryClient.invalidateQueries({ queryKey: ['sessions'] });
    },
  });

  const abort = (sessionId: string) => {
    abortControllerRef.current?.abort();
    clearStreaming(sessionId);
    clearStreamingReasoning(sessionId);
    setAgentStatus(sessionId, 'system', 'idle');
  };

  return { ...mutation, abort };
}
