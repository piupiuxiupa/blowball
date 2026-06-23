import { useEffect, useState } from 'react';
import { useDownloadUrl } from '@/hooks/use-file-content';
import { Skeleton } from '@/components/ui/skeleton';
import { Button } from '@/components/ui/button';
import { BinaryPlaceholder } from './binary-placeholder';

interface ExcelViewerProps {
  path: string;
}

export function ExcelViewer({ path }: ExcelViewerProps) {
  const { data: url, isLoading, isError } = useDownloadUrl(path);
  const [sheets, setSheets] = useState<string[]>([]);
  const [activeSheet, setActiveSheet] = useState<string>('');
  const [rows, setRows] = useState<unknown[][]>([]);
  const [error, setError] = useState(false);

  useEffect(() => {
    if (!url) return;
    let cancelled = false;
    fetch(url)
      .then((res) => res.arrayBuffer())
      .then(async (buf) => {
        const XLSX = await import('xlsx');
        const wb = XLSX.read(buf, { type: 'array' });
        if (cancelled) return;
        const names = wb.SheetNames;
        setSheets(names);
        const first = names[0] ?? '';
        setActiveSheet(first);
        if (first) {
          const ws = wb.Sheets[first];
          const data = XLSX.utils.sheet_to_json(ws, { header: 1, defval: '' }) as unknown[][];
          setRows(data);
        }
      })
      .catch(() => {
        if (!cancelled) setError(true);
      });
    return () => {
      cancelled = true;
    };
  }, [url]);

  const handleSwitchSheet = async (name: string) => {
    if (!url || name === activeSheet) return;
    setRows([]);
    setActiveSheet(name);
    try {
      const res = await fetch(url);
      const buf = await res.arrayBuffer();
      const XLSX = await import('xlsx');
      const wb = XLSX.read(buf, { type: 'array' });
      const ws = wb.Sheets[name];
      const data = XLSX.utils.sheet_to_json(ws, { header: 1, defval: '' }) as unknown[][];
      setRows(data);
    } catch {
      setError(true);
    }
  };

  if (isLoading || (url && rows.length === 0 && !error)) {
    return (
      <div className="space-y-3 p-4">
        <Skeleton className="h-4 w-3/4" />
        <Skeleton className="h-4 w-1/2" />
        <Skeleton className="h-4 w-2/3" />
      </div>
    );
  }

  if (isError || error) {
    return <BinaryPlaceholder path={path} />;
  }

  return (
    <div className="flex h-full flex-col">
      {sheets.length > 1 && (
        <div className="flex gap-2 border-b p-2">
          {sheets.map((name) => (
            <Button
              key={name}
              variant={name === activeSheet ? 'default' : 'outline'}
              size="sm"
              onClick={() => handleSwitchSheet(name)}
            >
              {name}
            </Button>
          ))}
        </div>
      )}
      <div className="flex-1 overflow-auto p-4">
        <table className="w-full border-collapse text-sm">
          <tbody>
            {rows.map((row, ri) => (
              <tr key={ri} className="border-b">
                {row.map((cell, ci) => (
                  <td key={ci} className="border px-2 py-1">
                    {String(cell)}
                  </td>
                ))}
              </tr>
            ))}
          </tbody>
        </table>
      </div>
    </div>
  );
}
