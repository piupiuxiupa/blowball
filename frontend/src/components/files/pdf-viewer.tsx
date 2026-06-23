import { useEffect, useRef, useState } from 'react';
import * as pdfjs from 'pdfjs-dist';
import { useDownloadUrl } from '@/hooks/use-file-content';
import { Button } from '@/components/ui/button';
import { Skeleton } from '@/components/ui/skeleton';
import { ChevronLeft, ChevronRight } from 'lucide-react';

pdfjs.GlobalWorkerOptions.workerSrc = `https://cdnjs.cloudflare.com/ajax/libs/pdf.js/${pdfjs.version}/pdf.worker.min.mjs`;

interface PdfViewerProps {
  path: string;
}

export function PdfViewer({ path }: PdfViewerProps) {
  const canvasRef = useRef<HTMLCanvasElement>(null);
  const [pdf, setPdf] = useState<pdfjs.PDFDocumentProxy | null>(null);
  const [pageNum, setPageNum] = useState(1);
  const [numPages, setNumPages] = useState(0);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);
  const { data: url } = useDownloadUrl(path);

  useEffect(() => {
    if (!url) return;
    let cancelled = false;
    setLoading(true);
    setError(null);

    pdfjs
      .getDocument({
        url,
        useSystemFonts: true,
        cMapUrl: `https://unpkg.com/pdfjs-dist@${pdfjs.version}/cmaps/`,
        cMapPacked: true,
      })
      .promise.then((doc) => {
        if (cancelled) return;
        setPdf(doc);
        setNumPages(doc.numPages);
        setPageNum(1);
        setLoading(false);
      })
      .catch((err) => {
        if (cancelled) return;
        setError(err instanceof Error ? err.message : '加载 PDF 失败');
        setLoading(false);
      });

    return () => {
      cancelled = true;
    };
  }, [url]);

  useEffect(() => {
    if (!pdf || !canvasRef.current) return;
    let cancelled = false;

    pdf.getPage(pageNum).then((page) => {
      if (cancelled) return;
      const canvas = canvasRef.current!;
      const ctx = canvas.getContext('2d');
      if (!ctx) return;

      const viewport = page.getViewport({ scale: 1.5 });
      canvas.width = viewport.width;
      canvas.height = viewport.height;

      page.render({ canvasContext: ctx, viewport });
    });

    return () => {
      cancelled = true;
    };
  }, [pdf, pageNum]);

  if (loading) {
    return <Skeleton className="m-4 h-[600px] w-full max-w-3xl" />;
  }

  if (error || !url) {
    return (
      <div className="flex h-full items-center justify-center text-sm text-destructive">
        {error || '无法加载 PDF'}
      </div>
    );
  }

  return (
    <div className="flex h-full flex-col items-center overflow-auto p-4">
      <div className="sticky top-0 z-10 mb-4 flex items-center gap-2 rounded-md border bg-background p-2 shadow-sm">
        <Button
          variant="outline"
          size="icon"
          onClick={() => setPageNum((p) => Math.max(1, p - 1))}
          disabled={pageNum <= 1}
        >
          <ChevronLeft className="h-4 w-4" />
        </Button>
        <span className="min-w-[80px] text-center text-sm">
          {pageNum} / {numPages}
        </span>
        <Button
          variant="outline"
          size="icon"
          onClick={() => setPageNum((p) => Math.min(numPages, p + 1))}
          disabled={pageNum >= numPages}
        >
          <ChevronRight className="h-4 w-4" />
        </Button>
      </div>

      <canvas ref={canvasRef} className="shadow-sm" />
    </div>
  );
}
