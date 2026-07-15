// Ambient types for the OnlyOffice DocsAPI injected by the dynamically-loaded
// http://localhost/web-apps/apps/api/documents/api.js script. Only the surface
// the viewer uses is declared; the real object is far larger.

interface OnlyOfficeEditorInstance {
  destroyEditor(): void;
}

interface OnlyOfficeDocEditorConfig {
  documentType?: string;
  document?: Record<string, unknown>;
  editorConfig?: Record<string, unknown>;
  token?: string;
  events?: Record<string, ((event: unknown) => void) | undefined>;
  [key: string]: unknown;
}

interface DocsAPI {
  DocEditor: new (
    element: string | HTMLElement,
    config: OnlyOfficeDocEditorConfig
  ) => OnlyOfficeEditorInstance;
}

interface Window {
  DocsAPI?: DocsAPI;
}
