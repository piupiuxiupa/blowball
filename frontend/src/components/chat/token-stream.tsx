import { Bot, Loader2, Wrench, AlertCircle, Lightbulb } from 'lucide-react';
import ReactMarkdown from 'react-markdown';

interface TokenStreamProps {
  agent: string;
  content: string;
  reasoning?: string;
  status: 'idle' | 'running' | 'tool_call' | 'error';
}

export function TokenStream({ agent, content, reasoning, status }: TokenStreamProps) {
  return (
    <div className="flex gap-3">
      <div className="flex h-8 w-8 shrink-0 items-center justify-center rounded-full bg-muted">
        <Bot className="h-4 w-4" />
      </div>

      <div className="max-w-[80%] space-y-1 rounded-lg bg-muted px-3 py-2 text-sm">
        <div className="flex items-center gap-2 text-xs font-medium text-muted-foreground">
          <span>{agent}</span>
          {status === 'running' && <Loader2 className="h-3 w-3 animate-spin" />}
          {status === 'tool_call' && <Wrench className="h-3 w-3" />}
          {status === 'error' && <AlertCircle className="h-3 w-3 text-destructive" />}
        </div>

        {reasoning && (
          <details className="rounded border border-muted-foreground/20 bg-muted/50 px-2 py-1" open>
            <summary className="flex cursor-pointer list-none items-center gap-1 text-xs text-muted-foreground">
              <Lightbulb className="h-3 w-3" />
              <span>思考过程</span>
            </summary>
            <div className="prose prose-sm max-w-none pt-1 text-muted-foreground">
              <ReactMarkdown>{reasoning}</ReactMarkdown>
            </div>
          </details>
        )}

        {content && (
          <div className="prose prose-sm max-w-none">
            <ReactMarkdown>{content}</ReactMarkdown>
          </div>
        )}

        {!content && !reasoning && status === 'running' && (
          <div className="text-xs text-muted-foreground">思考中…</div>
        )}

        {!content && reasoning && status === 'running' && (
          <div className="text-xs text-muted-foreground">思考中…</div>
        )}
      </div>
    </div>
  );
}
