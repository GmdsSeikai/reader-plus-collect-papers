import { useEffect, useState } from "react";
import { Copy, Download, FileDown, LoaderCircle } from "lucide-react";
import type { Annotation, Document, Message } from "@reader/core";
import { api } from "@reader/api";
import { Button } from "@reader/ui/components/button";
import { Checkbox } from "@reader/ui/components/checkbox";
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogHeader,
  DialogTitle,
} from "@reader/ui/components/dialog";
import { toast } from "sonner";
import { copyText } from "./chat/clipboard";
import {
  defaultExportParts,
  exportCounts,
  exportParts,
  notesMarkdown,
  type ExportPart,
} from "./notes-export";
import { downloadText, fileTitle } from "./download";
import { exportAnnotatedPDFWithToast } from "./annotated-pdf-export";

const storageKey = "reader.notes-export";
function savedParts(): ExportPart[] {
  try {
    const value: unknown = JSON.parse(localStorage.getItem(storageKey) || "");
    if (Array.isArray(value))
      return value.filter((part): part is ExportPart => part in exportParts);
  } catch {
    /* fall back to defaults */
  }
  return defaultExportParts;
}

export function ExportNotesDialog({
  document: doc,
  annotations,
  messages,
  link,
  onClose,
}: {
  document: Document;
  annotations: Annotation[];
  messages: Message[];
  link?: (annotation: Annotation) => string;
  onClose: () => void;
}) {
  const [parts, setParts] = useState<ExportPart[]>(savedParts);
  const [paperNote, setPaperNote] = useState<string | null>(null);
  const [exportingPDF, setExportingPDF] = useState(false);
  useEffect(() => {
    let alive = true;
    void api
      .documentNote(doc.id)
      .then((note) => alive && setPaperNote(note.body))
      .catch((e) => {
        if (alive) setPaperNote("");
        toast.error((e as Error).message);
      });
    return () => {
      alive = false;
    };
  }, [doc.id]);
  const data = { annotations, messages, paperNote: paperNote ?? "" };
  const counts = exportCounts(data);
  const toggle = (part: ExportPart, on: boolean) => {
    const next = on ? [...parts, part] : parts.filter((item) => item !== part);
    setParts(next);
    try {
      localStorage.setItem(storageKey, JSON.stringify(next));
    } catch {
      /* the choice only lasts for this dialog */
    }
  };
  const markdown = () => notesMarkdown(doc, data, parts, link);
  return (
    <Dialog open onOpenChange={(open) => !open && onClose()}>
      <DialogContent className="sm:max-w-sm">
        <DialogHeader>
          <DialogTitle>导出批注与笔记</DialogTitle>
          <DialogDescription>
            Markdown，可放进 Obsidian 或 Notion
          </DialogDescription>
        </DialogHeader>
        <div className="space-y-2.5">
          {(Object.keys(exportParts) as ExportPart[])
            .filter((part) => doc.library === "papers" || part !== "info")
            .map((part) => (
              <label key={part} className="flex items-center gap-2 text-sm">
                <Checkbox
                  checked={parts.includes(part)}
                  disabled={part !== "info" && !counts[part]}
                  onCheckedChange={(checked) => toggle(part, !!checked)}
                />
                <span className="flex-1">{exportParts[part]}</span>
                {part !== "info" && (
                  <span className="text-xs text-muted-foreground">
                    {counts[part]}
                  </span>
                )}
              </label>
            ))}
        </div>
        <div className="flex justify-end gap-2">
          <Button
            variant="outline"
            disabled={paperNote === null}
            onClick={() => void copyText(markdown(), "已复制 Markdown")}
          >
            <Copy />
            复制
          </Button>
          <Button
            disabled={paperNote === null}
            onClick={() => {
              downloadText(
                markdown(),
                `${fileTitle(doc.title)}-notes.md`,
                "text/markdown",
              );
              onClose();
            }}
          >
            <Download />
            导出
          </Button>
        </div>
        {doc.type === "pdf" && (
          <div className="flex items-center gap-3 border-t pt-4">
            <p className="flex-1 text-xs leading-5 text-muted-foreground">
              或把高亮、下划线、笔记和问答写进 PDF
              副本，可在其他阅读器中查看，原文件不变
            </p>
            <Button
              variant="outline"
              disabled={exportingPDF}
              onClick={async () => {
                setExportingPDF(true);
                await exportAnnotatedPDFWithToast(doc);
                setExportingPDF(false);
              }}
            >
              {exportingPDF ? (
                <LoaderCircle className="animate-spin motion-reduce:animate-none" />
              ) : (
                <FileDown />
              )}
              导出 PDF
            </Button>
          </div>
        )}
      </DialogContent>
    </Dialog>
  );
}
