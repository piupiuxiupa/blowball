import { Plus } from 'lucide-react';
import { useSessions } from '@/hooks/use-sessions';
import { useUIStore } from '@/stores/ui-store';
import { Button } from '@/components/ui/button';
import { ScrollArea } from '@/components/ui/scroll-area';
import { Skeleton } from '@/components/ui/skeleton';
import { SessionItem } from './session-item';

export function SessionList() {
  const { sessions, isLoading, error, createSession, isCreating } = useSessions();
  const { activeSessionId, setActiveSession } = useUIStore();

  return (
    <div className="flex h-full flex-col">
      <div className="flex h-10 items-center justify-between border-b px-3">
        <span className="text-xs font-medium text-muted-foreground">会话记录</span>
        <Button
          variant="ghost"
          size="icon"
          className="h-6 w-6"
          onClick={() => createSession()}
          disabled={isCreating}
          title="新建会话"
        >
          <Plus className="h-4 w-4" />
        </Button>
      </div>

      <ScrollArea className="flex-1">
        <div className="p-2">
          {isLoading && (
            <div className="space-y-2">
              <Skeleton className="h-10 w-full" />
              <Skeleton className="h-10 w-full" />
              <Skeleton className="h-10 w-full" />
            </div>
          )}

          {error && (
            <div className="p-2 text-xs text-destructive">加载会话失败</div>
          )}

          {!isLoading && sessions.length === 0 && (
            <div className="p-2 text-xs text-muted-foreground">暂无会话</div>
          )}

          {sessions.map((session) => (
            <SessionItem
              key={session.session_id}
              session={session}
              isActive={session.session_id === activeSessionId}
              onClick={() => setActiveSession(session.session_id)}
            />
          ))}
        </div>
      </ScrollArea>
    </div>
  );
}
