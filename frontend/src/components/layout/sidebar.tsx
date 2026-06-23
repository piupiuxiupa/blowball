import { Separator } from '@/components/ui/separator';
import { SessionList } from '@/components/sessions/session-list';
import { FileTree } from '@/components/workspace/file-tree';

export function Sidebar() {
  return (
    <div className="flex h-full flex-col">
      <div className="flex-1 min-h-0 overflow-auto">
        <SessionList />
      </div>

      <Separator />

      <div className="flex-1 min-h-0 overflow-auto">
        <FileTree />
      </div>
    </div>
  );
}
