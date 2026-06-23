import { useEffect, useRef } from 'react';
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

  const messages = data?.messages ?? [];
  const blocks = groupMessages(messages);

  useEffect(() => {
    if (scrollRef.current) {
      scrollRef.current.scrollTop = scrollRef.current.scrollHeight;
    }
  }, [messages, streamingText, agentStatus?.status]);

  return (
    <ScrollArea ref={scrollRef} className="h-full px-4 py-4">
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
