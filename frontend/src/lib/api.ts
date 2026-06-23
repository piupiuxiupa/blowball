import type { paths } from './openapi';

const API_BASE = import.meta.env.VITE_API_BASE_URL || 'http://localhost:8080';

export type ApiError = {
  error: {
    code: string;
    message: string;
  };
};

export class ApiRequestError extends Error {
  code: string;

  constructor(code: string, message: string) {
    super(message);
    this.name = 'ApiRequestError';
    this.code = code;
  }
}

export function getApiBase(): string {
  return API_BASE.replace(/\/$/, '');
}

export function getToken(): string | null {
  try {
    const raw = localStorage.getItem('blowball-auth');
    if (!raw) return null;
    const parsed = JSON.parse(raw) as { state?: { token?: string } };
    return parsed.state?.token ?? null;
  } catch {
    return null;
  }
}

async function handleResponse<T>(response: Response): Promise<T> {
  if (!response.ok) {
    let errorBody: ApiError | null = null;
    try {
      errorBody = (await response.json()) as ApiError;
    } catch {
      // ignore parse failure
    }
    const code = errorBody?.error?.code ?? `HTTP_${response.status}`;
    const message = errorBody?.error?.message ?? response.statusText;
    throw new ApiRequestError(code, message);
  }

  const contentType = response.headers.get('content-type') || '';
  if (contentType.includes('application/json')) {
    return (await response.json()) as T;
  }
  return (await response.text()) as unknown as T;
}

export async function apiGet<T>(
  path: string,
  options: { params?: Record<string, string | number | undefined>; token?: string | null } = {}
): Promise<T> {
  const url = new URL(getApiBase() + path);
  if (options.params) {
    Object.entries(options.params).forEach(([key, value]) => {
      if (value !== undefined && value !== '') {
        url.searchParams.set(key, String(value));
      }
    });
  }

  const token = options.token ?? getToken();
  const response = await fetch(url.toString(), {
    headers: {
      Accept: 'application/json',
      ...(token ? { Authorization: `Bearer ${token}` } : {}),
    },
  });

  return handleResponse<T>(response);
}

export async function apiPost<T>(
  path: string,
  options: { body?: unknown; token?: string | null } = {}
): Promise<T> {
  const token = options.token ?? getToken();
  const response = await fetch(getApiBase() + path, {
    method: 'POST',
    headers: {
      Accept: 'application/json',
      'Content-Type': 'application/json',
      ...(token ? { Authorization: `Bearer ${token}` } : {}),
    },
    body: options.body ? JSON.stringify(options.body) : undefined,
  });

  return handleResponse<T>(response);
}

export async function apiPostStream(
  path: string,
  options: { body?: unknown; token?: string | null } = {}
): Promise<Response> {
  const token = options.token ?? getToken();
  const response = await fetch(getApiBase() + path, {
    method: 'POST',
    headers: {
      Accept: 'text/event-stream',
      'Content-Type': 'application/json',
      ...(token ? { Authorization: `Bearer ${token}` } : {}),
    },
    body: options.body ? JSON.stringify(options.body) : undefined,
  });

  if (!response.ok) {
    let errorBody: ApiError | null = null;
    try {
      errorBody = (await response.clone().json()) as ApiError;
    } catch {
      // ignore
    }
    const code = errorBody?.error?.code ?? `HTTP_${response.status}`;
    const message = errorBody?.error?.message ?? response.statusText;
    throw new ApiRequestError(code, message);
  }

  return response;
}

export async function apiUpload<T>(
  path: string,
  options: { file: File; subdir?: string; token?: string | null } = { file: new File([], '') }
): Promise<T> {
  const token = options.token ?? getToken();
  const formData = new FormData();
  formData.append('file', options.file);
  if (options.subdir) {
    formData.append('path', options.subdir);
  }

  const response = await fetch(getApiBase() + path, {
    method: 'POST',
    headers: {
      ...(token ? { Authorization: `Bearer ${token}` } : {}),
    },
    body: formData,
  });

  return handleResponse<T>(response);
}

export function getDownloadUrl(path: string): string {
  return `${getApiBase()}/api/v1/workspace/files/${encodeURIComponent(path)}`;
}

// Re-export generated types for convenience
export type LoginRequest = paths['/api/v1/auth/login']['post']['requestBody']['content']['application/json'];
export type LoginResponse = paths['/api/v1/auth/login']['post']['responses']['200']['content']['application/json'];
export type SessionListResponse = paths['/api/v1/sessions']['get']['responses']['200']['content']['application/json'];
export type CreateSessionResponse = paths['/api/v1/sessions']['post']['responses']['200']['content']['application/json'];
export type SessionMessagesResponse =
  paths['/api/v1/sessions/{session_id}/messages']['get']['responses']['200']['content']['application/json'];
export type SendMessageRequest =
  paths['/api/v1/sessions/{session_id}/messages']['post']['requestBody']['content']['application/json'];
export type FileListResponse = paths['/api/v1/workspace/files']['get']['responses']['200']['content']['application/json'];
export type UploadResponse = paths['/api/v1/workspace/upload']['post']['responses']['200']['content']['application/json'];
export type FileContentResponse =
  paths['/api/v1/workspace/files/{path}/content']['get']['responses']['200']['content']['application/json'];
export type Message = NonNullable<SessionMessagesResponse['messages']>[number];
export type FileEntry = NonNullable<FileListResponse['files']>[number];
