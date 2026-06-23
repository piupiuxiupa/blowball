import { useImageBlob } from '@/hooks/use-file-content';
import { Skeleton } from '@/components/ui/skeleton';

interface ImageViewerProps {
  path: string;
}

export function ImageViewer({ path }: ImageViewerProps) {
  const { data: url, isLoading } = useImageBlob(path);

  if (isLoading) {
    return <Skeleton className="m-4 h-64 w-96" />;
  }

  if (!url) {
    return (
      <div className="flex h-full items-center justify-center text-sm text-muted-foreground">
        无法加载图片
      </div>
    );
  }

  return (
    <div className="flex h-full items-start justify-center overflow-auto p-6">
      <img src={url} alt={path} className="max-h-full max-w-full object-contain shadow-sm" />
    </div>
  );
}
