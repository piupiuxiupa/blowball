import { useEffect, useMemo, useRef } from 'react';
import { useMessages } from '@/hooks/use-messages';
import { useUIStore } from '@/stores/ui-store';
import { ScrollArea } from '@/components/ui/scroll-area';
import { Skeleton } from '@/components/ui/skeleton';
import { ChatMessage } from './chat-message';
import { TokenStream } from './token-stream';
import type { Message } from '@/lib/api';

interface MessageBlock {
  id: string;
  agent: string;
  role: 'user' | 'assistant';
  content: string;
  reasoning?: string;
  toolCalls: string[];
  isStreaming?: boolean;
  isError?: boolean;
}

function groupMessages(messages: Message[]): MessageBlock[] {
  const blocks: MessageBlock[] = [];
  let current: MessageBlock | null = null;

  for (const msg of messages) {
    if (msg.role === 'user') {
      if (current) blocks.push(current);
      current = null;
      blocks.push({
        id: `user-${msg.id}`,
        agent: 'user',
        role: 'user',
        content: msg.content,
        toolCalls: [],
      });
      continue;
    }

    if (msg.event_type === 'agent_start') {
      if (current) blocks.push(current);
      current = {
        id: `agent-${msg.id}`,
        agent: msg.agent,
        role: 'assistant',
        content: '',
        toolCalls: [],
      };
      continue;
    }

    if (msg.event_type === 'agent_end') {
      if (current) blocks.push(current);
      current = null;
      continue;
    }

    if (msg.event_type === 'agent_error') {
      if (current) {
        current.isError = true;
        current.content += `\n\n[错误] ${msg.content}`;
        blocks.push(current);
        current = null;
      } else {
        blocks.push({
          id: `error-${msg.id}`,
          agent: msg.agent,
          role: 'assistant',
          content: `[错误] ${msg.content}`,
          toolCalls: [],
          isError: true,
        });
      }
      continue;
    }

    if (msg.event_type === 'tool_call') {
      if (current) {
        current.toolCalls.push(msg.content);
      }
      continue;
    }

    if (msg.event_type === 'token') {
      if (!current) {
        current = {
          id: `agent-${msg.id}`,
          agent: msg.agent,
          role: 'assistant',
          content: '',
          toolCalls: [],
        };
      }
      current.content += msg.content;
      continue;
    }

    if (msg.event_type === 'reasoning') {
      if (!current) {
        current = {
          id: `agent-${msg.id}`,
          agent: msg.agent,
          role: 'assistant',
          content: '',
          toolCalls: [],
        };
      }
      current.reasoning = (current.reasoning ?? '') + msg.content;
    }
  }

  if (current) blocks.push(current);
  return blocks;
}

const SCROLL_THRESHOLD = 80;

export function MessageList() {
  const activeSessionId = useUIStore((s) => s.activeSessionId);
  const { data, isLoading } = useMessages(activeSessionId);
  const streamingText = useUIStore((s) =>
    activeSessionId ? s.streamingTokens[activeSessionId] ?? '' : ''
  );
  const streamingReasoning = useUIStore((s) =>
    activeSessionId ? s.streamingReasoningTokens[activeSessionId] ?? '' : ''
  );
  const agentStatus = useUIStore((s) =>
    activeSessionId ? s.agentStatus[activeSessionId] : null
  );
  const scrollRef = useRef<HTMLDivElement>(null);
  const isNearBottomRef = useRef(true);
  const scrollRafRef = useRef<number | null>(null);

  const messages = data?.messages ?? [];
  const blocks = useMemo(() => groupMessages(messages), [messages]);

  const scheduleScrollToBottom = () => {
    if (scrollRafRef.current != null) return;
    scrollRafRef.current = requestAnimationFrame(() => {
      scrollRafRef.current = null;
      const el = scrollRef.current;
      if (!el) return;
      // 只有用户本来就在底部附近时才继续自动滚动，避免他正在翻看历史时被强行拉下去。
      const distanceFromBottom = el.scrollHeight - el.scrollTop - el.clientHeight;
      if (distanceFromBottom <= SCROLL_THRESHOLD) {
        el.scrollTop = el.scrollHeight;
      }
    });
  };

  const handleScroll = () => {
    const el = scrollRef.current;
    if (!el) return;
    const distanceFromBottom = el.scrollHeight - el.scrollTop - el.clientHeight;
    isNearBottomRef.current = distanceFromBottom <= SCROLL_THRESHOLD;
  };

  useEffect(() => {
    if (isNearBottomRef.current) {
      scheduleScrollToBottom();
    }
    return () => {
      if (scrollRafRef.current != null) {
        cancelAnimationFrame(scrollRafRef.current);
        scrollRafRef.current = null;
      }
    };
    // 故意不把 streamingText 放进依赖：token 高频更新时只在 rAF 里读一次 DOM，
    // 避免每个 token 都触发 effect 和强制同步布局。
  }, [messages.length, activeSessionId, agentStatus?.status]);

  return (
    <ScrollArea ref={scrollRef} onScroll={handleScroll} className="h-full px-4 py-4">
      {isLoading && (
        <div className="space-y-4">
          <Skeleton className="h-16 w-3/4" />
          <Skeleton className="h-16 w-2/3" />
        </div>
      )}

      {!isLoading && blocks.length === 0 && !streamingText && (
        <div className="flex h-full items-center justify-center text-sm text-muted-foreground">
          发送第一条消息开始对话
        </div>
      )}

      <div className="space-y-4">
        {blocks.map((block) => (
          <ChatMessage key={block.id} block={block} />
        ))}

        {(streamingText || streamingReasoning || agentStatus?.status === 'running' || agentStatus?.status === 'tool_call') && (
          <TokenStream
            agent={agentStatus?.agent || 'Agent'}
            content={streamingText}
            reasoning={streamingReasoning}
            status={agentStatus?.status || 'running'}
          />
        )}
      </div>
    </ScrollArea>
  );
}
