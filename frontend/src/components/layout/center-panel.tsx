import { useUIStore } from '@/stores/ui-store';
import { FileRenderer } from '@/components/files/file-renderer';

export function CenterPanel() {
  const activeFilePath = useUIStore((s) => s.activeFilePath);

  return (
    <div className="flex h-full flex-col">
      <div className="flex h-10 shrink-0 items-center border-b px-4 text-sm text-muted-foreground">
        {activeFilePath ? activeFilePath : '未选择文件'}
      </div>
      <div className="flex-1 min-h-0 overflow-auto">
        <FileRenderer />
      </div>
    </div>
  );
}
