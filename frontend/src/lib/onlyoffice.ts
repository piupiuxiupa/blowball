// Local-development OnlyOffice integration.
//
// This module is HARDCODED for local dev on purpose: the DocumentServer is
// assumed to be running locally (default http://localhost) with JWT enabled and
// the shared secret below, and the backend is assumed to be running on the host
// at :8080. A real deployment MUST move config signing to the backend — the
// OnlyOffice secret must never ship to browsers in production. The browser only
// ever holds the blowball auth JWT; here we additionally sign the editor config
// with the OnlyOffice secret purely because there is no backend endpoint to do
// it for us yet.

import { getToken } from '@/lib/api';
import { getFileExtension } from '@/lib/file-type';

// Hardcoded local-dev configuration. Tweak these values if your setup differs.
export const ONLYOFFICE = {
  // Script the browser loads to bootstrap the editor (served by DocumentServer).
  // Use http://localhost (NOT the LAN IP) when the browser is on the same
  // machine: the editor iframe opens a long-lived websocket back to this origin,
  // and a LAN IP routed through a proxy/VPN commonly breaks websockets while
  // passing plain HTTP — that leaves the editor stuck (no open command ever
  // reaches DocService). localhost bypasses the proxy. The DocumentServer still
  // fetches the document via internalBackend (10.1.152.201:8080) below.
  apiScript: 'http://localhost/web-apps/apps/api/documents/api.js',
  // Base URL DocumentServer uses when it fetches document.url. The browser-facing
  // http://localhost:8080 is NOT reachable from inside the DocumentServer
  // container (its own localhost is itself, port 80), so we use the host's LAN
  // IP, which the container can route to. This must be an address the
  // DocumentServer container can actually reach AND that serves the blowball
  // API — if the backend is down or bound to localhost-only, document loading
  // fails with a download error in the editor.
  internalBackend: 'http://10.1.152.201:8080',
  // Shared OnlyOffice JWT secret. The local container's local.json sets the same
  // value for browser/inbox/outbox/session, and token.enable.browser is on, so an
  // unsigned editor config is rejected — this token is required.
  // NOTE: this is NOT blowball's config.yaml jwt.secret. See the two-secrets
  // note below — they are independent and must never be swapped.
  secret: '4Cor7UDd0GmWdrKWH3VoNya2xDjv3JAF',
} as const;

export type OfficeDocumentType = 'word' | 'cell' | 'slide';

// Map a file path to OnlyOffice's documentType, or null if it is not an office
// document (caller should fall back to another renderer).
export function officeDocumentType(path: string): OfficeDocumentType | null {
  const ext = getFileExtension(path);
  if (ext === 'docx' || ext === 'doc') return 'word';
  if (ext === 'xlsx' || ext === 'xls') return 'cell';
  if (ext === 'pptx' || ext === 'ppt') return 'slide';
  return null;
}

// Minimal shape of the config handed to DocsAPI.DocEditor. OnlyOffice's full
// schema is large; we declare only the fields we set.
export interface OnlyOfficeConfig {
  documentType: OfficeDocumentType;
  document: {
    fileType: string;
    key: string;
    title: string;
    url: string;
    permissions: { edit: boolean; download: boolean };
  };
  editorConfig: {
    mode: 'view' | 'edit';
    lang: string;
    user: { id: string; name: string };
  };
}

// Build a stable-but-bustable document key. OnlyOffice caches converted
// documents by this key, so it must change when content changes. We derive it
// from the path (so reopening an unchanged file is fast) and append a
// caller-provided nonce so a freshly-written file (e.g. an agent overwriting
// it) can be force-refreshed. OnlyOffice keys must be <=128 chars.
function buildKey(path: string, nonce: number): string {
  const safe = path.replace(/[^a-zA-Z0-9._-]/g, '_').slice(-100);
  return `${safe}__${nonce}`.slice(0, 128);
}

// The URL the DocumentServer (NOT the browser) will GET to download the file.
// Uses the query-token download endpoint so no custom Authorization header is
// needed, and internalBackend so the container can actually reach the backend.
function buildDocumentUrl(path: string): string {
  const token = getToken();
  const params = new URLSearchParams({ inline: '1' });
  if (token) params.set('token', token);
  return `${ONLYOFFICE.internalBackend}/api/v1/workspace/files/download/${encodeURIComponent(
    path
  )}?${params.toString()}`;
}

// Build a read-only editor config for the given workspace file. Returns null if
// the path is not an office document.
export function buildOnlyOfficeConfig(
  path: string,
  nonce: number
): OnlyOfficeConfig | null {
  const documentType = officeDocumentType(path);
  if (!documentType) return null;
  return {
    documentType,
    document: {
      fileType: getFileExtension(path),
      key: buildKey(path, nonce),
      title: path.split('/').pop() || path,
      url: buildDocumentUrl(path),
      permissions: { edit: false, download: true },
    },
    editorConfig: {
      mode: 'view',
      lang: 'zh',
      user: { id: 'local', name: '本地用户' },
    },
  };
}

// --- HS256 JWT signing via the Web Crypto API (no external dependency). ---
// OnlyOffice's `token` field must be a JWT whose payload is the config object
// itself (minus the token field), signed with the browser secret. Web Crypto
// requires a secure context; Vite's localhost dev server qualifies.

function base64Url(bytes: Uint8Array): string {
  let bin = '';
  for (let i = 0; i < bytes.length; i++) bin += String.fromCharCode(bytes[i]);
  return btoa(bin).replace(/\+/g, '-').replace(/\//g, '_').replace(/=+$/, '');
}

export async function signOnlyOfficeToken(
  config: OnlyOfficeConfig
): Promise<string> {
  const enc = new TextEncoder();
  const headerB64 = base64Url(enc.encode(JSON.stringify({ alg: 'HS256', typ: 'JWT' })));
  const payloadB64 = base64Url(enc.encode(JSON.stringify(config)));
  const signingInput = `${headerB64}.${payloadB64}`;

  const key = await crypto.subtle.importKey(
    'raw',
    enc.encode(ONLYOFFICE.secret),
    { name: 'HMAC', hash: 'SHA-256' },
    false,
    ['sign']
  );
  const signature = new Uint8Array(
    await crypto.subtle.sign('HMAC', key, enc.encode(signingInput))
  );
  return `${signingInput}.${base64Url(signature)}`;
}

// --- api.js loader, injected once and cached across mounts. ---

let apiPromise: Promise<void> | null = null;

// Load DocumentServer's api.js exactly once. Resolves once window.DocsAPI is
// available; rejects (and clears the cache so a later mount can retry) on error.
export function loadOnlyOfficeApi(): Promise<void> {
  if (apiPromise) return apiPromise;
  apiPromise = new Promise<void>((resolve, reject) => {
    if (window.DocsAPI) {
      resolve();
      return;
    }
    const script = document.createElement('script');
    script.src = ONLYOFFICE.apiScript;
    script.async = true;
    script.onload = () => {
      if (window.DocsAPI) resolve();
      else reject(new Error('OnlyOffice api.js loaded but DocsAPI is missing'));
    };
    script.onerror = () => {
      apiPromise = null; // allow a subsequent mount to retry
      reject(new Error(`Failed to load ${ONLYOFFICE.apiScript}`));
    };
    document.head.appendChild(script);
  });
  return apiPromise;
}
