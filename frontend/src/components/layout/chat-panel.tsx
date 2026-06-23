import { useUIStore } from '@/stores/ui-store';
import { MessageList } from '@/components/chat/message-list';
import { MessageInput } from '@/components/chat/message-input';

export function ChatPanel() {
  const activeSessionId = useUIStore((s) => s.activeSessionId);

  return (
    <div className="flex h-full flex-col">
      <div className="flex h-10 shrink-0 items-center border-b px-4 text-sm font-medium">
        Agent 聊天
      </div>

      <div className="flex-1 min-h-0 overflow-hidden">
        {activeSessionId ? (
          <MessageList />
        ) : (
          <div className="flex h-full items-center justify-center text-sm text-muted-foreground">
            选择一个会话开始聊天
          </div>
        )}
      </div>

      <div className="border-t p-3">
        <MessageInput disabled={!activeSessionId} />
      </div>
    </div>
  );
}
