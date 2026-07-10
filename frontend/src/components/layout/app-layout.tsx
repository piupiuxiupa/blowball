import { useAuth } from '@/hooks/use-auth';
import { useResizableWidth } from '@/hooks/use-resizable-width';
import { Button } from '@/components/ui/button';
import { Resizer } from '@/components/ui/resizer';
import { Sidebar } from './sidebar';
import { CenterPanel } from './center-panel';
import { ChatPanel } from './chat-panel';

const LEFT_PANEL_KEY = 'blowball:left-panel-width';
const RIGHT_PANEL_KEY = 'blowball:right-panel-width';

export function AppLayout() {
  const { logout } = useAuth();
  const [leftWidth, adjustLeftWidth] = useResizableWidth(LEFT_PANEL_KEY, 288, { min: 200, max: 480 });
  const [rightWidth, adjustRightWidth] = useResizableWidth(RIGHT_PANEL_KEY, 420, { min: 320, max: 720 });

  return (
    <div className="flex h-screen flex-col overflow-hidden">
      <header className="flex h-12 shrink-0 items-center justify-between border-b px-4">
        <div className="font-semibold">blowball</div>
        <Button variant="ghost" size="sm" onClick={logout}>
          退出登录
        </Button>
      </header>

      <div className="flex flex-1 overflow-hidden">
        <aside
          className="flex shrink-0 flex-col border-r bg-muted/30"
          style={{ width: leftWidth }}
        >
          <Sidebar />
        </aside>

        <Resizer onResize={adjustLeftWidth} />

        <main className="flex min-w-0 flex-1 flex-col">
          <CenterPanel />
        </main>

        <Resizer onResize={(delta) => adjustRightWidth(-delta)} />

        <aside
          className="flex shrink-0 flex-col border-l bg-muted/30"
          style={{ width: rightWidth }}
        >
          <ChatPanel />
        </aside>
      </div>
    </div>
  );
}
