import { User, Bot, Wrench, Lightbulb } from 'lucide-react';
import { cn } from '@/lib/utils';
import ReactMarkdown from 'react-markdown';

interface ChatMessageProps {
  block: {
    id: string;
    agent: string;
    role: 'user' | 'assistant';
    content: string;
    reasoning?: string;
    toolCalls: string[];
    isError?: boolean;
  };
}

export function ChatMessage({ block }: ChatMessageProps) {
  const isUser = block.role === 'user';

  return (
    <div className={cn('flex gap-3', isUser ? 'flex-row-reverse' : 'flex-row')}>
      <div
        className={cn(
          'flex h-8 w-8 shrink-0 items-center justify-center rounded-full',
          isUser ? 'bg-primary text-primary-foreground' : 'bg-muted'
        )}
      >
        {isUser ? <User className="h-4 w-4" /> : <Bot className="h-4 w-4" />}
      </div>

      <div
        className={cn(
          'max-w-[80%] space-y-1 rounded-lg px-3 py-2 text-sm',
          isUser
            ? 'bg-primary text-primary-foreground'
            : 'bg-muted',
          block.isError && 'border border-destructive/50 bg-destructive/10'
        )}
      >
        {!isUser && (
          <div className="text-xs font-medium text-muted-foreground">{block.agent}</div>
        )}

        {block.reasoning && !isUser && (
          <details className="rounded border border-muted-foreground/20 bg-muted/50 px-2 py-1">
            <summary className="flex cursor-pointer list-none items-center gap-1 text-xs text-muted-foreground">
              <Lightbulb className="h-3 w-3" />
              <span>思考过程</span>
            </summary>
            <div className="prose prose-sm max-w-none pt-1 text-muted-foreground">
              <ReactMarkdown>{block.reasoning}</ReactMarkdown>
            </div>
          </details>
        )}

        {block.content && (
          <div className={cn('prose prose-sm max-w-none', isUser && 'prose-invert')}>
            <ReactMarkdown>{block.content}</ReactMarkdown>
          </div>
        )}

        {block.toolCalls.length > 0 && (
          <div className="flex flex-wrap gap-1 pt-1">
            {block.toolCalls.map((tool, idx) => (
              <span
                key={idx}
                className="inline-flex items-center gap-1 rounded-md bg-background/80 px-2 py-0.5 text-xs text-muted-foreground"
              >
                <Wrench className="h-3 w-3" />
                {tool}
              </span>
            ))}
          </div>
        )}
      </div>
    </div>
  );
}
