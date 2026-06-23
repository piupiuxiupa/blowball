import { FileWarning, Download } from 'lucide-react';
import { useDownloadFile } from '@/hooks/use-file-content';
import { Button } from '@/components/ui/button';

interface BinaryPlaceholderProps {
  path: string;
  message?: string;
}

export function BinaryPlaceholder({ path, message }: BinaryPlaceholderProps) {
  const download = useDownloadFile(path);

  return (
    <div className="flex h-full flex-col items-center justify-center gap-4 p-8 text-center">
      <FileWarning className="h-12 w-12 text-muted-foreground" />
      <div className="space-y-1">
        <p className="font-medium">{message ?? '二进制文件'}</p>
        <p className="text-sm text-muted-foreground">{path}</p>
      </div>
      <Button onClick={() => download()} className="gap-2">
        <Download className="h-4 w-4" />
        下载文件
      </Button>
    </div>
  );
}
