import { useEffect, useId, useRef, useState } from 'react';
import { Button } from '@/components/ui/button';
import { Skeleton } from '@/components/ui/skeleton';
import {
  buildOnlyOfficeConfig,
  loadOnlyOfficeApi,
  signOnlyOfficeToken,
} from '@/lib/onlyoffice';

interface OfficeViewerProps {
  path: string;
}

// How long to wait for the editor's onAppReady before assuming it stalled.
const LOAD_TIMEOUT_MS = 20000;

// Renders an office file (docx/xlsx/pptx, and legacy doc/xls/ppt) via the local
// OnlyOffice DocumentServer in read-only mode. See src/lib/onlyoffice.ts for the
// hardcoded local-dev configuration and the security note about secret signing.
//
// Switching files remounts <EditorMount> (keyed by path+nonce) so OnlyOffice
// never reuses a stale element/session across documents — reusing it crashes the
// editor (blank screen). Each document gets its own fresh mount + lifecycle.
export function OfficeViewer({ path }: OfficeViewerProps) {
  const [nonce, setNonce] = useState(0);
  return (
    <div className="flex h-full w-full flex-col">
      <div className="flex items-center justify-between border-b px-3 py-1.5 text-xs text-muted-foreground">
        <span className="truncate">OnlyOffice 预览（只读）· {path}</span>
        <Button variant="ghost" size="sm" onClick={() => setNonce((n) => n + 1)}>
          刷新
        </Button>
      </div>
      <div className="relative flex-1">
        <EditorMount key={`${path}::${nonce}`} path={path} nonce={nonce} />
      </div>
    </div>
  );
}

interface EditorMountProps {
  path: string;
  nonce: number;
}

// One OnlyOffice editor instance, fully isolated. Mounted fresh per document
// (via the parent's key) and destroyed on unmount.
function EditorMount({ path, nonce }: EditorMountProps) {
  // useId() can contain ':' which breaks some internal lookups; sanitize.
  const editorId = 'oo-' + useId().replace(/[^a-zA-Z0-9]/g, '');
  const editorRef = useRef<OnlyOfficeEditorInstance | null>(null);
  const settledRef = useRef(false);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);
  const [docUrl, setDocUrl] = useState<string>('');

  useEffect(() => {
    let cancelled = false;
    let timeoutId: ReturnType<typeof setTimeout> | undefined;
    settledRef.current = false;
    setLoading(true);
    setError(null);

    const fail = (msg: string) => {
      console.error('[OfficeViewer]', msg);
      if (cancelled || settledRef.current) return;
      settledRef.current = true;
      setError(msg);
      setLoading(false);
    };

    (async () => {
      try {
        await loadOnlyOfficeApi();
      } catch (e) {
        fail(`加载 OnlyOffice 脚本失败：${e instanceof Error ? e.message : String(e)}`);
        return;
      }
      if (cancelled) return;

      const config = buildOnlyOfficeConfig(path, nonce);
      if (!config) {
        fail('不支持的文件类型');
        return;
      }
      setDocUrl(config.document.url);

      let token: string;
      try {
        token = await signOnlyOfficeToken(config);
      } catch (e) {
        fail(`签名失败：${e instanceof Error ? e.message : String(e)}`);
        return;
      }

      const mount = document.getElementById(editorId);
      if (cancelled || !mount) {
        fail('编辑器挂载点未就绪');
        return;
      }
      const DocsAPI = window.DocsAPI;
      if (!DocsAPI) {
        fail('OnlyOffice 脚本已加载，但 window.DocsAPI 不可用');
        return;
      }
      try {
        editorRef.current = new DocsAPI.DocEditor(editorId, {
          ...config,
          token,
          width: '100%',
          height: '100%',
          events: {
            onAppReady: () => {
              if (cancelled || settledRef.current) return;
              settledRef.current = true;
              if (timeoutId) clearTimeout(timeoutId);
              setLoading(false);
            },
            onError: (event: unknown) => {
              fail(`编辑器事件错误：${JSON.stringify(event) ?? String(event)}`);
            },
          },
        });
      } catch (e) {
        fail(`编辑器初始化抛错：${e instanceof Error ? e.message : String(e)}`);
        return;
      }

      timeoutId = setTimeout(() => {
        fail(
          `OnlyOffice ${LOAD_TIMEOUT_MS / 1000}s 内未就绪。请用 http://localhost:5173/oo-test.html 做隔离测试。`
        );
      }, LOAD_TIMEOUT_MS);
    })();

    return () => {
      cancelled = true;
      if (timeoutId) clearTimeout(timeoutId);
      try {
        editorRef.current?.destroyEditor();
      } catch {
        // editor may already be gone
      }
      editorRef.current = null;
    };
  }, [path, nonce, editorId]);

  return (
    <div className="relative h-full w-full">
      {loading && !error && (
        <div className="absolute inset-0 space-y-3 p-4">
          <Skeleton className="h-4 w-3/4" />
          <Skeleton className="h-4 w-1/2" />
          <Skeleton className="h-4 w-2/3" />
        </div>
      )}
      {error ? (
        <div className="flex h-full items-center justify-center overflow-auto p-4">
          <pre className="whitespace-pre-wrap break-words text-center text-xs text-destructive">
            {error}
            {docUrl ? `\n\n文档地址（DocumentServer 拉取）：\n${docUrl}` : ''}
          </pre>
        </div>
      ) : (
        <div id={editorId} className="h-full w-full" />
      )}
    </div>
  );
}
