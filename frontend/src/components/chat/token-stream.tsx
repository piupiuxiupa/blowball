import { memo } from 'react';
import { Bot, Loader2, Wrench, AlertCircle, Lightbulb } from 'lucide-react';
import { MarkdownRenderer } from './markdown-renderer';

interface TokenStreamProps {
  agent: string;
  content: string;
  reasoning?: string;
  status: 'idle' | 'running' | 'tool_call' | 'error';
}

function splitStreamingContent(text: string, isFinished: boolean): { completed: string[]; pending: string } {
  if (isFinished) {
    return { completed: text ? [text] : [], pending: '' };
  }
  // 流式过程中：已完结的段落用 Markdown 渲染，正在输入的最后一行用纯文本展示，
  // 避免每来一个 token 都重新解析整段内容。
  const trimmed = text.endsWith('\n') ? text.slice(0, -1) : text;
  const lastBreak = trimmed.lastIndexOf('\n');
  if (lastBreak === -1) {
    return { completed: [], pending: text };
  }
  return {
    completed: trimmed.slice(0, lastBreak).split('\n').filter((s) => s.length > 0),
    pending: text.slice(lastBreak + 1),
  };
}

export const TokenStream = memo(function TokenStream({ agent, content, reasoning, status }: TokenStreamProps) {
  const isFinished = status === 'idle' || status === 'error';
  const { completed, pending } = splitStreamingContent(content, isFinished);

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
            <div className="prose prose-sm max-w-none whitespace-pre-wrap pt-1 text-muted-foreground">
              {reasoning}
            </div>
          </details>
        )}

        {(completed.length > 0 || pending) && (
          <div className="prose prose-sm max-w-none">
            {completed.map((segment, idx) => (
              <MarkdownRenderer key={idx}>{segment}</MarkdownRenderer>
            ))}
            {pending && <p className="m-0">{pending}</p>}
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
});
