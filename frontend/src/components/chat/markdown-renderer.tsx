import type { Components } from 'react-markdown';
import ReactMarkdown from 'react-markdown';
import remarkGfm from 'remark-gfm';
import { Prism as SyntaxHighlighter } from 'react-syntax-highlighter';
import { oneLight } from 'react-syntax-highlighter/dist/esm/styles/prism';

const components: Components = {
  code(props) {
    const { children, className, node, ref, ...rest } = props;
    void node;
    void ref;
    const match = /language-(\w+)/.exec(className || '');
    const language = match ? match[1] : '';
    const value = String(children ?? '').replace(/\n$/, '');

    if (language) {
      return (
        <SyntaxHighlighter
          {...rest}
          style={oneLight}
          language={language}
          PreTag="div"
          className="rounded-md text-sm"
        >
          {value}
        </SyntaxHighlighter>
      );
    }

    return (
      <code {...rest} className={className}>
        {children}
      </code>
    );
  },
  pre({ children }) {
    return <div className="overflow-auto">{children}</div>;
  },
};

interface MarkdownRendererProps {
  children: string;
  className?: string;
}

export function MarkdownRenderer({ children, className }: MarkdownRendererProps) {
  return (
    <ReactMarkdown
      remarkPlugins={[remarkGfm]}
      components={components}
      className={className}
    >
      {children}
    </ReactMarkdown>
  );
}
