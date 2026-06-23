import { useQuery } from '@tanstack/react-query';
import { apiGet } from '@/lib/api';
import type { SessionMessagesResponse } from '@/lib/api';

const DEFAULT_PAGE_SIZE = 100;

export function useMessages(sessionId: string | null) {
  return useQuery({
    queryKey: ['messages', sessionId],
    queryFn: async () => {
      if (!sessionId) return { messages: [] } as SessionMessagesResponse;

      const allMessages: NonNullable<SessionMessagesResponse['messages']> = [];
      let pageToken: string | undefined;

      do {
        const response = await apiGet<SessionMessagesResponse>(
          `/api/v1/sessions/${encodeURIComponent(sessionId)}/messages`,
          {
            params: {
              page_size: DEFAULT_PAGE_SIZE,
              order: 'asc',
              ...(pageToken ? { page_token: pageToken } : {}),
            },
          }
        );
        if (response.messages) {
          allMessages.push(...response.messages);
        }
        pageToken = response.next_page_token;
      } while (pageToken);

      return { messages: allMessages } as SessionMessagesResponse;
    },
    enabled: !!sessionId,
  });
}
