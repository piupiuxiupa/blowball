import { MessageSquare, Trash2, Loader2 } from 'lucide-react';
import { cn } from '@/lib/utils';
import { useDeleteSession } from '@/hooks/use-sessions';
import { Button } from '@/components/ui/button';

interface SessionItemProps {
  session: {
    session_id: string;
    title: string;
    update_time?: string;
  };
  isActive: boolean;
  onClick: () => void;
}

export function SessionItem({ session, isActive, onClick }: SessionItemProps) {
  const deleteSession = useDeleteSession();
  const isDeleting = deleteSession.isPending;
  const label = session.title || '未命名会话';

  const handleDelete = async (e: React.MouseEvent) => {
    // Stop propagation so the click doesn't also select the session.
    e.stopPropagation();
    if (!window.confirm(`确定删除会话「${label}」吗？此操作不可撤销。`)) return;
    try {
      await deleteSession.mutateAsync(session.session_id);
    } catch (err) {
      alert(`删除会话失败：${err instanceof Error ? err.message : String(err)}`);
    }
  };

  return (
    <div className="group relative flex items-center">
      <button
        onClick={onClick}
        disabled={isDeleting}
        className={cn(
          'flex flex-1 items-center gap-2 rounded-md px-2 py-2 pr-7 text-left text-sm transition-colors',
          isActive ? 'bg-accent text-accent-foreground' : 'hover:bg-muted',
          isDeleting && 'opacity-50'
        )}
      >
        <MessageSquare className="h-4 w-4 shrink-0 text-muted-foreground" />
        <div className="min-w-0 flex-1">
          <div className="truncate font-medium">{label}</div>
          {session.update_time && (
            <div className="truncate text-xs text-muted-foreground">
              {new Date(session.update_time).toLocaleString()}
            </div>
          )}
        </div>
      </button>

      <Button
        variant="ghost"
        size="icon"
        className="absolute right-0.5 top-1/2 h-6 w-6 -translate-y-1/2 text-muted-foreground opacity-0 transition-opacity hover:bg-destructive/10 hover:text-destructive focus-visible:opacity-100 group-hover:opacity-100"
        onClick={handleDelete}
        disabled={isDeleting}
        title="删除会话"
        aria-label={`删除会话 ${label}`}
      >
        {isDeleting ? (
          <Loader2 className="h-3.5 w-3.5 animate-spin" />
        ) : (
          <Trash2 className="h-3.5 w-3.5" />
        )}
      </Button>
    </div>
  );
}
