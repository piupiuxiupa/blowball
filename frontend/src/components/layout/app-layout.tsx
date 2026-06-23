import { useAuth } from '@/hooks/use-auth';
import { Button } from '@/components/ui/button';
import { Separator } from '@/components/ui/separator';
import { Sidebar } from './sidebar';
import { CenterPanel } from './center-panel';
import { ChatPanel } from './chat-panel';

export function AppLayout() {
  const { logout } = useAuth();

  return (
    <div className="flex h-screen flex-col overflow-hidden">
      <header className="flex h-12 shrink-0 items-center justify-between border-b px-4">
        <div className="font-semibold">blowball</div>
        <Button variant="ghost" size="sm" onClick={logout}>
          退出登录
        </Button>
      </header>

      <div className="flex flex-1 overflow-hidden">
        <aside className="flex w-72 flex-col border-r bg-muted/30">
          <Sidebar />
        </aside>

        <Separator orientation="vertical" />

        <main className="flex min-w-0 flex-1 flex-col">
          <CenterPanel />
        </main>

        <Separator orientation="vertical" />

        <aside className="flex w-[420px] flex-col border-l bg-muted/30">
          <ChatPanel />
        </aside>
      </div>
    </div>
  );
}
