"use client";

import { useRef } from "react";
import { Paperclip } from "lucide-react";
import { cn } from "@multica/ui/lib/utils";

interface FileUploadButtonProps {
  /** Called once per selected file — caller handles upload. The native
   *  picker now allows multi-select by default, so this fires N times for
   *  N files in a single open. Callers don't need to change anything to
   *  opt in. Set `multiple={false}` to restore single-file behavior. */
  onSelect: (file: File) => void;
  disabled?: boolean;
  className?: string;
  size?: "sm" | "default";
  /** Allow multi-select in the native picker. Default true. */
  multiple?: boolean;
}

function FileUploadButton({
  onSelect,
  disabled,
  className,
  size = "default",
  multiple = true,
}: FileUploadButtonProps) {
  const inputRef = useRef<HTMLInputElement>(null);

  const handleChange = (e: React.ChangeEvent<HTMLInputElement>) => {
    const files = e.target.files;
    if (!files || files.length === 0) return;
    const list = Array.from(files);
    e.target.value = "";
    for (const file of list) onSelect(file);
  };

  const iconSize = size === "sm" ? "h-3.5 w-3.5" : "h-4 w-4";
  const btnSize = size === "sm" ? "h-6 w-6" : "h-7 w-7";

  return (
    <>
      <button
        type="button"
        onClick={() => inputRef.current?.click()}
        disabled={disabled}
        aria-label="Attach file"
        title="Attach file"
        className={cn(
          "inline-flex items-center justify-center rounded-full text-muted-foreground hover:bg-accent hover:text-foreground transition-colors disabled:opacity-50 disabled:pointer-events-none",
          btnSize,
          className,
        )}
      >
        <Paperclip className={iconSize} />
      </button>
      <input
        ref={inputRef}
        type="file"
        multiple={multiple}
        className="hidden"
        onChange={handleChange}
      />
    </>
  );
}

export { FileUploadButton, type FileUploadButtonProps };
