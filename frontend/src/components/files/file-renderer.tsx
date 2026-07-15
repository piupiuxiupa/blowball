import { useUIStore } from '@/stores/ui-store';
import { useFileContent } from '@/hooks/use-file-content';
import { getFileExtension, isImage, isMarkdown, isPdf, isOffice } from '@/lib/file-type';
import { MarkdownViewer } from './markdown-viewer';
import { CodeViewer } from './code-viewer';
import { ImageViewer } from './image-viewer';
import { PdfViewer } from './pdf-viewer';
import { BinaryPlaceholder } from './binary-placeholder';
import { OfficeViewer } from './office-viewer';
import { Skeleton } from '@/components/ui/skeleton';

export function FileRenderer() {
  const activeFilePath = useUIStore((s) => s.activeFilePath);
  // Office files render through OnlyOffice (binary, via the download endpoint),
  // so we skip the text-content fetch for them — otherwise /content 400s with
  // BINARY_FILE on every office open. ext is computed before the hook so it can
  // gate the query (hooks must run unconditionally).
  const ext = getFileExtension(activeFilePath ?? '');
  const { data, isLoading } = useFileContent(activeFilePath, {
    enabled: !!activeFilePath && !isOffice(ext),
  });

  if (!activeFilePath) {
    return (
      <div className="flex h-full items-center justify-center text-sm text-muted-foreground">
        从左侧选择一个文件以查看内容
      </div>
    );
  }

  if (isLoading) {
    return (
      <div className="space-y-3 p-4">
        <Skeleton className="h-4 w-3/4" />
        <Skeleton className="h-4 w-1/2" />
        <Skeleton className="h-4 w-2/3" />
      </div>
    );
  }

  // Route binary previewers by extension before falling back to text content.
  if (isOffice(ext)) {
    return <OfficeViewer path={activeFilePath} />;
  }
  if (isPdf(ext)) {
    return <PdfViewer path={activeFilePath} />;
  }
  if (isImage(ext)) {
    return <ImageViewer path={activeFilePath} />;
  }

  // If content came back as null and there is an error, treat as binary
  const hasError = data && 'error' in data && data.error != null;
  const content = data && 'content' in data ? (data.content as string) : null;

  if (hasError || content === null) {
    return <BinaryPlaceholder path={activeFilePath} />;
  }

  if (isMarkdown(ext)) {
    return <MarkdownViewer content={content} />;
  }

  return <CodeViewer content={content} language={ext} />;
}
