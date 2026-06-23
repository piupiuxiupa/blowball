import { Prism as SyntaxHighlighter } from 'react-syntax-highlighter';
import { oneLight } from 'react-syntax-highlighter/dist/esm/styles/prism';

interface CodeViewerProps {
  content: string;
  language?: string;
}

const LANGUAGE_MAP: Record<string, string> = {
  ts: 'typescript',
  tsx: 'tsx',
  js: 'javascript',
  jsx: 'jsx',
  py: 'python',
  go: 'go',
  rs: 'rust',
  java: 'java',
  cpp: 'cpp',
  c: 'c',
  cs: 'csharp',
  rb: 'ruby',
  php: 'php',
  swift: 'swift',
  kt: 'kotlin',
  sh: 'bash',
  yaml: 'yaml',
  yml: 'yaml',
  json: 'json',
  html: 'html',
  css: 'css',
  sql: 'sql',
  md: 'markdown',
  dockerfile: 'dockerfile',
};

export function CodeViewer({ content, language }: CodeViewerProps) {
  const lang = language ? LANGUAGE_MAP[language] || language : 'text';

  return (
    <div className="p-4">
      <SyntaxHighlighter
        language={lang}
        style={oneLight}
        customStyle={{
          margin: 0,
          borderRadius: '0.5rem',
          fontSize: '0.875rem',
        }}
        showLineNumbers
      >
        {content}
      </SyntaxHighlighter>
    </div>
  );
}
