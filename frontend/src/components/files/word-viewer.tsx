import { useEffect, useState } from 'react';
import { useDownloadUrl } from '@/hooks/use-file-content';
import { getFileExtension } from '@/lib/file-type';
import { Skeleton } from '@/components/ui/skeleton';
import { BinaryPlaceholder } from './binary-placeholder';

interface WordViewerProps {
  path: string;
}

export function WordViewer({ path }: WordViewerProps) {
  const { data: url, isLoading, isError } = useDownloadUrl(path);
  const [html, setHtml] = useState<string | null>(null);
  const [error, setError] = useState(false);
  const ext = getFileExtension(path);

  useEffect(() => {
    if (!url) return;
    if (ext === 'doc') {
      setHtml(null);
      setError(false);
      return;
    }
    let cancelled = false;
    fetch(url)
      .then((res) => res.arrayBuffer())
      .then(async (buf) => {
        const mammoth = await import('mammoth');
        const result = await mammoth.convertToHtml({ arrayBuffer: buf });
        if (!cancelled) setHtml(result.value);
      })
      .catch(() => {
        if (!cancelled) setError(true);
      });
    return () => {
      cancelled = true;
    };
  }, [url, ext]);

  if (isLoading || (url && ext === 'docx' && html === null && !error)) {
    return (
      <div className="space-y-3 p-4">
        <Skeleton className="h-4 w-3/4" />
        <Skeleton className="h-4 w-1/2" />
        <Skeleton className="h-4 w-2/3" />
      </div>
    );
  }

  if (isError || error) {
    return <BinaryPlaceholder path={path} />;
  }

  if (ext === 'doc') {
    return (
      <BinaryPlaceholder
        path={path}
        message=".doc 预览暂不支持，请下载后查看"
      />
    );
  }

  return (
    <div
      className="prose max-w-none p-4"
      dangerouslySetInnerHTML={{ __html: html ?? '' }}
    />
  );
}
