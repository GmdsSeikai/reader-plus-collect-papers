import {
  locationLabel,
  type ReaderAdapter,
  type ReaderSelection,
  type SourceReference,
} from "@reader/core";
import { contextReferences } from "../references";

const contextLimit = 21000;

/** Only explicit quotations or opted-in chapter text become source references. */
export async function composeChatContext({
  adapter,
  selected,
  includeChapter = false,
  useSection = false,
  marks = "",
}: {
  adapter: ReaderAdapter | null | undefined;
  selected: ReaderSelection[];
  includeChapter?: boolean;
  useSection?: boolean;
  marks?: string;
}): Promise<{ context: string; references: SourceReference[] }> {
  const chapter = useSection || (!selected.length && includeChapter);
  let excerpts = selected;
  if (chapter && adapter) {
    const location = adapter.getLocation();
    excerpts =
      adapter.getChapterContext && !useSection
        ? await adapter.getChapterContext(location)
        : [{ text: await adapter.getContext(), location }];
  }
  const references: SourceReference[] = [];
  const parts: string[] = [];
  // Notes accompany opted-in chapter text, never an ordinary unquoted question.
  const notes = chapter ? marks.slice(0, 4000) : "";
  let remaining = contextLimit - (notes ? notes.length + 2 : 0);
  for (const excerpt of excerpts) {
    if (!excerpt.text.trim()) continue;
    const prefix = `[${chapter ? "原文" : "选区"} · ${locationLabel(excerpt.location)}]\n`;
    const available = remaining - prefix.length - (parts.length ? 2 : 0);
    if (available <= 0) break;
    const text = excerpt.text.slice(0, available);
    const part = prefix + text;
    remaining -= part.length + (parts.length ? 2 : 0);
    parts.push(part);
    if (chapter) references.push(...contextReferences(text, excerpt.location));
    else
      references.push({ text, location: excerpt.location, kind: "selection" });
  }
  if (notes) parts.push(notes);
  return { context: parts.join("\n\n"), references };
}
