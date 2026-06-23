import { useRef } from 'react';
import { Upload } from 'lucide-react';
import { useWorkspace } from '@/hooks/use-workspace';
import { Button } from '@/components/ui/button';

export function UploadButton() {
  const inputRef = useRef<HTMLInputElement>(null);
  const { uploadFile, isUploading } = useWorkspace();

  const handleChange = async (e: React.ChangeEvent<HTMLInputElement>) => {
    const file = e.target.files?.[0];
    if (!file) return;
    await uploadFile({ file });
    e.target.value = '';
  };

  return (
    <>
      <input
        ref={inputRef}
        type="file"
        className="hidden"
        onChange={handleChange}
        disabled={isUploading}
      />
      <Button
        variant="ghost"
        size="icon"
        className="h-6 w-6"
        onClick={() => inputRef.current?.click()}
        disabled={isUploading}
        title="上传文件"
      >
        <Upload className="h-4 w-4" />
      </Button>
    </>
  );
}
