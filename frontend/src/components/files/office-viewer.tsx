import { useEffect, useId, useRef, useState } from 'react';
import { Button } from '@/components/ui/button';
import { Skeleton } from '@/components/ui/skeleton';
import { loadOnlyOfficeApi } from '@/lib/onlyoffice';
import { useOfficeConfig } from '@/hooks/use-office-config';

interface OfficeViewerProps {
  path: string;
}

// How long to wait for the editor's onAppReady before assuming it stalled.
const LOAD_TIMEOUT_MS = 20000;

// Renders an office file (docx/xlsx/pptx, and legacy doc/xls/ppt) via a local or
// remote OnlyOffice DocumentServer in EDIT mode, persisting saves back to the
// workspace through the backend's save-callback endpoint.
//
// All OnlyOffice config (secret, server_url, internal_backend) is owned by the
// backend — this component just fetches a signed editor config and hands
// {...config, token} to DocsAPI.DocEditor. Refresh re-fetches the config so the
// backend mints a new random document.key, forcing OnlyOffice to re-convert
// (never a stale cached document).
//
// Switching files (or pressing Refresh) remounts <EditorMount> (keyed by
// path+nonce) so OnlyOffice never reuses a stale element/session across
// documents — reusing it crashes the editor (blank screen). Each document gets
// its own fresh mount + lifecycle.
export function OfficeViewer({ path }: OfficeViewerProps) {
  const [nonce, setNonce] = useState(0);
  return (
    <div className="flex h-full w-full flex-col">
      <div className="flex items-center justify-between border-b px-3 py-1.5 text-xs text-muted-foreground">
        <span className="truncate">OnlyOffice 编辑 · {path}</span>
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

  const { data, error: queryError } = useOfficeConfig(path, nonce);
  const [ready, setReady] = useState(false);
  const [editorError, setEditorError] = useState<string | null>(null);
  const [docUrl, setDocUrl] = useState<string>('');

  useEffect(() => {
    // Wait for the signed config before doing anything; the loading skeleton is
    // shown while data is absent.
    if (!data) return;
    let cancelled = false;
    let settled = false;
    let timeoutId: ReturnType<typeof setTimeout> | undefined;

    setReady(false);
    setEditorError(null);

    const fail = (msg: string) => {
      if (cancelled || settled) return;
      settled = true;
      if (timeoutId) clearTimeout(timeoutId);
      setEditorError(msg);
    };

    (async () => {
      try {
        await loadOnlyOfficeApi(data.server_url);
      } catch (e) {
        fail(`加载 OnlyOffice 脚本失败：${e instanceof Error ? e.message : String(e)}`);
        return;
      }
      if (cancelled) return;

      setDocUrl(data.config.document.url);

      const mount = document.getElementById(editorId);
      const DocsAPI = window.DocsAPI;
      if (cancelled || !mount) {
        fail('编辑器挂载点未就绪');
        return;
      }
      if (!DocsAPI) {
        fail('OnlyOffice 脚本已加载，但 window.DocsAPI 不可用');
        return;
      }
      try {
        editorRef.current = new DocsAPI.DocEditor(editorId, {
          ...data.config,
          token: data.token,
          width: '100%',
          height: '100%',
          events: {
            onAppReady: () => {
              if (cancelled || settled) return;
              settled = true;
              if (timeoutId) clearTimeout(timeoutId);
              setReady(true);
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
        fail(`OnlyOffice ${LOAD_TIMEOUT_MS / 1000}s 内未就绪。`);
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
  }, [data, editorId]);

  const errorMsg = queryError
    ? `获取编辑配置失败：${queryError instanceof Error ? queryError.message : String(queryError)}`
    : editorError;
  const loading = !errorMsg && !ready;

  return (
    <div className="relative h-full w-full">
      {loading && (
        <div className="absolute inset-0 space-y-3 p-4">
          <Skeleton className="h-4 w-3/4" />
          <Skeleton className="h-4 w-1/2" />
          <Skeleton className="h-4 w-2/3" />
        </div>
      )}
      {errorMsg ? (
        <div className="flex h-full items-center justify-center overflow-auto p-4">
          <pre className="whitespace-pre-wrap break-words text-center text-xs text-destructive">
            {errorMsg}
            {docUrl ? `\n\n文档地址（DocumentServer 拉取）：\n${docUrl}` : ''}
          </pre>
        </div>
      ) : (
        <div id={editorId} className="h-full w-full" />
      )}
    </div>
  );
}
