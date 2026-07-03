import { getPreviewUrl } from '@/hooks/use-file-content';

interface ImageViewerProps {
  path: string;
}

export function ImageViewer({ path }: ImageViewerProps) {
  return (
    <div className="flex h-full items-start justify-center overflow-auto p-6">
      <img src={getPreviewUrl(path)} alt={path} className="max-h-full max-w-full object-contain shadow-sm" />
    </div>
  );
}
