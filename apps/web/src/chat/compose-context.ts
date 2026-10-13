import {
  locationLabel,
  type ReaderAdapter,
  type ReaderSelection,
  type SourceReference,
} from "@reader/core";
import { contextReferences } from "../references";

const contextByteLimit = 64000;

/** Explicit quotations carry their complete chapters as auxiliary context. */
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
  if (selected.length && !adapter?.getChapterContext)
    throw new Error("无法读取引用所在的完整章节，请等待文档加载后重试。");
  const references: SourceReference[] = [];
  const parts: string[] = [];
  let bytes = 0;
  const append = (text: string) => {
    bytes += new TextEncoder().encode(text).length + (parts.length ? 2 : 0);
    if (bytes > contextByteLimit)
      throw new Error(
        "选区及完整章节超过 64 KB，无法完整发送。请减少引用的章节，或分开提问。",
      );
    parts.push(text);
  };
  for (const excerpt of selected) {
    if (!excerpt.text.trim()) continue;
    append(`[选区 · ${locationLabel(excerpt.location)}]\n${excerpt.text}`);
    references.push({
      text: excerpt.text,
      location: excerpt.location,
      kind: "selection",
    });
  }
  if ((selected.length || includeChapter || useSection) && adapter) {
    const locations = selected.length
      ? selected.map((s) => s.location)
      : [adapter.getLocation()];
    const seen = new Set<string>();
    for (const location of locations) {
      // Explicit page actions retain their scope. A quote always needs its chapter.
      const excerpts =
        adapter.getChapterContext && (!useSection || selected.length)
          ? await adapter.getChapterContext(location)
          : [{ text: await adapter.getContext(), location }];
      if (selected.length && !excerpts.some((excerpt) => excerpt.text.trim()))
        throw new Error(
          "无法读取引用所在的完整章节，请等待文档解析完成后重试。",
        );
      for (const excerpt of excerpts) {
        if (!excerpt.text.trim()) continue;
        const ref = contextReferences(excerpt.text, excerpt.location)[0]!;
        const key = JSON.stringify([ref.location, ref.text]);
        if (seen.has(key)) continue;
        seen.add(key);
        append(
          `[${selected.length ? "辅助章节原文" : "原文"} · ${locationLabel(ref.location)}]\n${ref.text}`,
        );
        // Keep the user's chosen passages as the visible quotations.
        if (!selected.length) references.push(ref);
      }
    }
    if (marks && !selected.length) append(marks.slice(0, 4000));
  }
  return { context: parts.join("\n\n"), references };
}
