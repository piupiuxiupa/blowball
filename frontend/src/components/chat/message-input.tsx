import { useState } from 'react';
import { Send } from 'lucide-react';
import { useSendMessage } from '@/hooks/use-send-message';
import { useUIStore } from '@/stores/ui-store';
import { Button } from '@/components/ui/button';
import { Textarea } from '@/components/ui/textarea';

interface MessageInputProps {
  disabled?: boolean;
}

export function MessageInput({ disabled }: MessageInputProps) {
  const [content, setContent] = useState('');
  const activeSessionId = useUIStore((s) => s.activeSessionId);
  const { mutate: sendMessage, isPending } = useSendMessage();

  const handleSubmit = () => {
    if (!activeSessionId || !content.trim() || isPending) return;
    sendMessage({ sessionId: activeSessionId, content: content.trim() });
    setContent('');
  };

  const handleKeyDown = (e: React.KeyboardEvent<HTMLTextAreaElement>) => {
    if (e.key === 'Enter' && !e.shiftKey) {
      e.preventDefault();
      handleSubmit();
    }
  };

  return (
    <div className="flex items-end gap-2">
      <Textarea
        value={content}
        onChange={(e) => setContent(e.target.value)}
        onKeyDown={handleKeyDown}
        placeholder={disabled ? '先选择一个会话' : '输入消息，Shift+Enter 换行'}
        disabled={disabled || isPending}
        rows={3}
        className="min-h-[80px] flex-1"
      />
      <Button
        onClick={handleSubmit}
        disabled={disabled || isPending || !content.trim()}
        size="icon"
        className="h-9 w-9 shrink-0"
      >
        <Send className="h-4 w-4" />
      </Button>
    </div>
  );
}
