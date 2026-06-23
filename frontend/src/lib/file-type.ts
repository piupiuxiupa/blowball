export function getFileExtension(path: string): string {
  return path.split('.').pop()?.toLowerCase() || '';
}

export function isMarkdown(ext: string): boolean {
  return ext === 'md' || ext === 'markdown';
}

export function isImage(ext: string): boolean {
  return ['png', 'jpg', 'jpeg', 'gif', 'webp', 'svg', 'bmp', 'ico'].includes(ext);
}

export function isPdf(ext: string): boolean {
  return ext === 'pdf';
}

export function isWord(ext: string): boolean {
  return ext === 'docx' || ext === 'doc';
}

export function isExcel(ext: string): boolean {
  return ext === 'xls' || ext === 'xlsx';
}
