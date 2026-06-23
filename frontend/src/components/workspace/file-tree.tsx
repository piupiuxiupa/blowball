import { useState } from 'react';
import { Folder, ChevronRight, ChevronDown, File } from 'lucide-react';
import { cn } from '@/lib/utils';
import { useWorkspace } from '@/hooks/use-workspace';
import { useUIStore } from '@/stores/ui-store';
import { ScrollArea } from '@/components/ui/scroll-area';
import { Skeleton } from '@/components/ui/skeleton';
import { UploadButton } from './upload-button';
import type { FileEntry } from '@/lib/api';

export function FileTree() {
  const { files, isLoading, error } = useWorkspace();

  return (
    <div className="flex h-full flex-col">
      <div className="flex h-10 items-center justify-between border-b px-3">
        <span className="text-xs font-medium text-muted-foreground">工作空间</span>
        <UploadButton />
      </div>

      <ScrollArea className="flex-1">
        <div className="p-2">
          {isLoading && (
            <div className="space-y-2">
              <Skeleton className="h-6 w-full" />
              <Skeleton className="h-6 w-full" />
              <Skeleton className="h-6 w-full" />
            </div>
          )}

          {error && (
            <div className="p-2 text-xs text-destructive">加载文件失败</div>
          )}

          {!isLoading && files.length === 0 && (
            <div className="p-2 text-xs text-muted-foreground">暂无文件</div>
          )}

          <FileNodeList entries={files} />
        </div>
      </ScrollArea>
    </div>
  );
}

function FileNodeList({ entries }: { entries: FileEntry[] }) {
  return (
    <div className="space-y-0.5">
      {entries.map((entry) => (
        <FileNode key={entry.name} entry={entry} />
      ))}
    </div>
  );
}

function FileNode({ entry }: { entry: FileEntry }) {
  const [expanded, setExpanded] = useState(false);
  const { activeFilePath, setActiveFile } = useUIStore();
  const childPath = entry.name;
  const isActive = activeFilePath === childPath;

  if (entry.type === 'dir') {
    return (
      <div>
        <button
          onClick={() => setExpanded(!expanded)}
          className="flex w-full items-center gap-1 rounded-md px-2 py-1 text-left text-sm hover:bg-muted"
        >
          {expanded ? (
            <ChevronDown className="h-3.5 w-3.5 shrink-0 text-muted-foreground" />
          ) : (
            <ChevronRight className="h-3.5 w-3.5 shrink-0 text-muted-foreground" />
          )}
          <Folder className="h-4 w-4 shrink-0 text-muted-foreground" />
          <span className="truncate">{entry.name}</span>
        </button>

        {expanded && (
          <div className="ml-5 border-l pl-1">
            <DirectoryChildren path={entry.name} />
          </div>
        )}
      </div>
    );
  }

  return (
    <button
      onClick={() => setActiveFile(childPath)}
      className={cn(
        'flex w-full items-center gap-2 rounded-md px-2 py-1 text-left text-sm transition-colors',
        isActive ? 'bg-accent text-accent-foreground' : 'hover:bg-muted'
      )}
    >
      <File className="h-4 w-4 shrink-0 text-muted-foreground" />
      <span className="truncate">{entry.name}</span>
    </button>
  );
}

function DirectoryChildren({ path }: { path: string }) {
  const { files, isLoading } = useWorkspace(path);

  if (isLoading) {
    return (
      <div className="space-y-1 p-1">
        <Skeleton className="h-5 w-full" />
        <Skeleton className="h-5 w-full" />
      </div>
    );
  }

  return <FileNodeList entries={files} />;
}
