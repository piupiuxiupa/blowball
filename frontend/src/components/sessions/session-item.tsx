import { useEffect, useRef, useState } from 'react';
import { MessageSquare, Trash2, Pencil, Loader2 } from 'lucide-react';
import { cn } from '@/lib/utils';
import { useDeleteSession, useUpdateSession } from '@/hooks/use-sessions';
import { Button } from '@/components/ui/button';
import { Input } from '@/components/ui/input';

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
  const updateSession = useUpdateSession();
  const isDeleting = deleteSession.isPending;
  const isUpdating = updateSession.isPending;
  const label = session.title || '未命名会话';

  const [isEditing, setIsEditing] = useState(false);
  const inputRef = useRef<HTMLInputElement>(null);

  useEffect(() => {
    if (isEditing && inputRef.current) {
      inputRef.current.focus();
      inputRef.current.select();
    }
  }, [isEditing]);

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

  const startEditing = (e: React.MouseEvent) => {
    e.stopPropagation();
    setIsEditing(true);
  };

  const submitTitle = async (newTitle: string) => {
    const trimmed = newTitle.trim();
    if (trimmed && trimmed !== label) {
      try {
        await updateSession.mutateAsync({ sessionId: session.session_id, title: trimmed });
      } catch (err) {
        alert(`重命名会话失败：${err instanceof Error ? err.message : String(err)}`);
      }
    }
    setIsEditing(false);
  };

  const handleKeyDown = (e: React.KeyboardEvent<HTMLInputElement>) => {
    if (e.key === 'Enter') {
      e.preventDefault();
      void submitTitle(e.currentTarget.value);
    } else if (e.key === 'Escape') {
      setIsEditing(false);
    }
  };

  const handleBlur = (e: React.FocusEvent<HTMLInputElement>) => {
    void submitTitle(e.target.value);
  };

  return (
    <div className="group relative flex items-center">
      <button
        onClick={onClick}
        disabled={isDeleting || isEditing}
        className={cn(
          'flex flex-1 items-center gap-2 rounded-md px-2 py-2 pr-14 text-left text-sm transition-colors',
          isActive ? 'bg-accent text-accent-foreground' : 'hover:bg-muted',
          isDeleting && 'opacity-50'
        )}
      >
        <MessageSquare className="h-4 w-4 shrink-0 text-muted-foreground" />
        <div className="min-w-0 flex-1">
          {isEditing ? (
            <Input
              ref={inputRef}
              defaultValue={session.title}
              onKeyDown={handleKeyDown}
              onBlur={handleBlur}
              disabled={isUpdating}
              className="h-6 px-1 py-0 text-sm"
              onClick={(e) => e.stopPropagation()}
            />
          ) : (
            <>
              <div className="truncate font-medium">{label}</div>
              {session.update_time && (
                <div className="truncate text-xs text-muted-foreground">
                  {new Date(session.update_time).toLocaleString()}
                </div>
              )}
            </>
          )}
        </div>
      </button>

      {!isEditing && (
        <>
          <Button
            variant="ghost"
            size="icon"
            className="absolute right-8 top-1/2 h-6 w-6 -translate-y-1/2 text-muted-foreground opacity-0 transition-opacity hover:bg-accent hover:text-accent-foreground focus-visible:opacity-100 group-hover:opacity-100"
            onClick={startEditing}
            disabled={isDeleting}
            title="重命名会话"
            aria-label={`重命名会话 ${label}`}
          >
            <Pencil className="h-3.5 w-3.5" />
          </Button>

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
        </>
      )}
    </div>
  );
}
