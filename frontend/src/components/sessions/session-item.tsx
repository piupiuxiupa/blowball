import { MessageSquare } from 'lucide-react';
import { cn } from '@/lib/utils';

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
  return (
    <button
      onClick={onClick}
      className={cn(
        'flex w-full items-center gap-2 rounded-md px-2 py-2 text-left text-sm transition-colors',
        isActive
          ? 'bg-accent text-accent-foreground'
          : 'hover:bg-muted'
      )}
    >
      <MessageSquare className="h-4 w-4 shrink-0 text-muted-foreground" />
      <div className="min-w-0 flex-1">
        <div className="truncate font-medium">{session.title || '未命名会话'}</div>
        {session.update_time && (
          <div className="truncate text-xs text-muted-foreground">
            {new Date(session.update_time).toLocaleString()}
          </div>
        )}
      </div>
    </button>
  );
}
