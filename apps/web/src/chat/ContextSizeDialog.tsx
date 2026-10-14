import {
  AlertDialog,
  AlertDialogAction,
  AlertDialogCancel,
  AlertDialogContent,
  AlertDialogDescription,
  AlertDialogFooter,
  AlertDialogHeader,
  AlertDialogTitle,
} from "@reader/ui/components/alert-dialog";
import type { OversizeChoice, OversizeContext } from "./compose-context";

/** Asks before sending a context above the soft limit. */
export function ContextSizeDialog({
  request,
  onChoose,
}: {
  request: OversizeContext | undefined;
  onChoose: (choice: OversizeChoice) => void;
}) {
  const kb = Math.round((request?.bytes ?? 0) / 1000);
  // Roughly 3 UTF-8 bytes per token across Chinese and English text.
  const tokens = Math.round((request?.bytes ?? 0) / 3000);
  const nearby = request?.quoted ? "选区附近" : "当前位置附近";
  return (
    <AlertDialog
      open={Boolean(request)}
      onOpenChange={(open) => {
        if (!open) onChoose("cancel");
      }}
    >
      <AlertDialogContent>
        <AlertDialogHeader>
          <AlertDialogTitle>上下文较长</AlertDialogTitle>
          <AlertDialogDescription>
            {request?.full
              ? `本次将发送约 ${kb} KB 原文（约 ${tokens}k token），超过建议的 64 KB，会增加费用和等待时间。仍然发送吗？`
              : `完整原文约 ${kb} KB，超过 256 KB 上限，无法完整发送。可以只发送${nearby}的原文。`}
          </AlertDialogDescription>
        </AlertDialogHeader>
        <AlertDialogFooter>
          <AlertDialogCancel>取消</AlertDialogCancel>
          {request?.trim && (
            <AlertDialogAction
              variant={request.full ? "outline" : "default"}
              onClick={() => onChoose("trim")}
            >
              只发送{nearby}原文
            </AlertDialogAction>
          )}
          {request?.full && (
            <AlertDialogAction onClick={() => onChoose("full")}>
              仍然发送
            </AlertDialogAction>
          )}
        </AlertDialogFooter>
      </AlertDialogContent>
    </AlertDialog>
  );
}
