import ReactMarkdown from 'react-markdown';

interface MarkdownViewerProps {
  content: string;
}

export function MarkdownViewer({ content }: MarkdownViewerProps) {
  return (
    <article className="prose prose-sm max-w-none p-6">
      <ReactMarkdown>{content}</ReactMarkdown>
    </article>
  );
}
