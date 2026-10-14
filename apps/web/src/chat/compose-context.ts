import {
  locationLabel,
  type DocumentLocation,
  type ReaderAdapter,
  type ReaderSelection,
  type SourceReference,
} from "@reader/core";
import { contextReferences } from "../references";

const contextByteLimit = 64000;
const encoder = new TextEncoder();
const byteLength = (text: string) => encoder.encode(text).length;

type Anchor = { location: DocumentLocation; text?: string };
type Chapter = { anchors: Anchor[]; excerpts: SourceReference[] };

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
}): Promise<{
  context: string;
  references: SourceReference[];
  notice?: string;
}> {
  if (selected.length && !adapter?.getChapterContext)
    throw new Error("无法读取引用所在的完整章节，请等待文档加载后重试。");
  const references: SourceReference[] = [];
  const parts: string[] = [];
  const used = () =>
    parts.reduce((sum, part, i) => sum + byteLength(part) + (i ? 2 : 0), 0);
  for (const excerpt of selected) {
    if (!excerpt.text.trim()) continue;
    parts.push(`[选区 · ${locationLabel(excerpt.location)}]\n${excerpt.text}`);
    references.push({
      text: excerpt.text,
      location: excerpt.location,
      kind: "selection",
    });
  }
  // The quoted passages themselves are never shortened.
  if (used() > contextByteLimit)
    throw new Error(
      "选区超过 64 KB，无法完整发送。请减少引用的内容，或分开提问。",
    );
  if (!(selected.length || includeChapter || useSection) || !adapter)
    return { context: parts.join("\n\n"), references };

  const anchors: Anchor[] = selected.length
    ? selected
    : [{ location: adapter.getLocation() }];
  const chapters: Chapter[] = [];
  const owner = new Map<string, Chapter>();
  for (const anchor of anchors) {
    // Explicit page actions retain their scope. A quote always needs its chapter.
    const excerpts =
      adapter.getChapterContext && (!useSection || selected.length)
        ? await adapter.getChapterContext(anchor.location)
        : [{ text: await adapter.getContext(), location: anchor.location }];
    if (selected.length && !excerpts.some((excerpt) => excerpt.text.trim()))
      throw new Error("无法读取引用所在的完整章节，请等待文档解析完成后重试。");
    let chapter: Chapter | undefined;
    const fresh: SourceReference[] = [];
    for (const excerpt of excerpts) {
      if (!excerpt.text.trim()) continue;
      const ref = contextReferences(excerpt.text, excerpt.location)[0]!;
      const key = JSON.stringify([ref.location, ref.text]);
      const seen = owner.get(key);
      if (seen) chapter ??= seen;
      else fresh.push(ref);
    }
    if (!chapter) {
      if (!fresh.length) continue;
      chapter = { anchors: [], excerpts: [] };
      chapters.push(chapter);
    }
    chapter.anchors.push(anchor);
    for (const ref of fresh) {
      chapter.excerpts.push(ref);
      owner.set(JSON.stringify([ref.location, ref.text]), chapter);
    }
  }

  const tail = marks && !selected.length ? marks.slice(0, 4000) : "";
  const prefix = selected.length ? "辅助章节原文" : "原文";
  const header = (ref: SourceReference, trimmed: boolean) =>
    `[${prefix}${trimmed ? "（节选）" : ""} · ${locationLabel(ref.location)}]\n`;
  const full = chapters.flatMap((chapter) =>
    chapter.excerpts.map((ref) => header(ref, false) + ref.text),
  );
  const fixed = used() + (tail ? byteLength(tail) + 2 : 0);
  const fits =
    fixed +
      full.reduce(
        (sum, part, i) => sum + byteLength(part) + (parts.length || i ? 2 : 0),
        0,
      ) <=
    contextByteLimit;
  let notice: string | undefined;
  if (fits) {
    parts.push(...full);
    // Keep the user's chosen passages as the visible quotations.
    if (!selected.length)
      references.push(...chapters.flatMap((chapter) => chapter.excerpts));
  } else {
    // Long chapters keep the paragraphs nearest each quote, labelled as excerpts.
    let budget = contextByteLimit - fixed;
    chapters.forEach((chapter, i) => {
      const share = Math.floor(budget / (chapters.length - i));
      const kept = nearbyText(chapter, share, (ref) => header(ref, true));
      for (const ref of kept) {
        const part = header(ref, true) + ref.text;
        budget -= byteLength(part) + 2;
        parts.push(part);
        if (!selected.length) references.push(ref);
      }
    });
    notice = selected.length
      ? "引用所在章节超过 64 KB，已附带选区前后的原文。"
      : "本章超过 64 KB，已附带当前位置前后的原文。";
  }
  if (tail) parts.push(tail);
  return { context: parts.join("\n\n"), references, notice };
}

/** Grows contiguous paragraph windows around each anchor until the budget is spent. */
function nearbyText(
  chapter: Chapter,
  budget: number,
  header: (ref: SourceReference) => string,
): SourceReference[] {
  const units = chapter.excerpts.flatMap((ref, excerpt) =>
    ref.text
      .split(/\n+/)
      .filter((text) => text.trim())
      .map((text) => ({ excerpt, text })),
  );
  if (!units.length || budget <= 0) return [];
  const seeds = [
    ...new Set(
      chapter.anchors.map((anchor) => anchorUnit(units, chapter, anchor)),
    ),
  ].sort((a, b) => a - b);
  const kept = new Map<number, string>();
  const opened = new Set<number>();
  let spent = 0;
  const cost = (i: number, text: string) =>
    // Paragraph joiner, counted as the wider "\n…\n" gap marker.
    byteLength(text) +
    5 +
    (opened.has(units[i]!.excerpt)
      ? 0
      : byteLength(header(chapter.excerpts[units[i]!.excerpt]!)) + 2);
  const take = (i: number, text = units[i]!.text) => {
    if (kept.has(i)) return true;
    const bytes = cost(i, text);
    if (spent + bytes > budget) return false;
    spent += bytes;
    kept.set(i, text);
    opened.add(units[i]!.excerpt);
    return true;
  };
  for (const seed of seeds) {
    if (take(seed)) continue;
    // A single oversized paragraph is cut around the quote.
    const unit = units[seed]!;
    const anchor = chapter.anchors.find(
      (a) => anchorUnit(units, chapter, a) === seed,
    );
    const at = Math.max(0, quoteIndex(unit.text, anchor?.text));
    const room = Math.floor((budget - spent - cost(seed, "")) / 3);
    if (room <= 0) continue;
    const start = Math.max(
      0,
      Math.min(at - Math.floor(room / 2), unit.text.length - room),
    );
    take(seed, unit.text.slice(start, start + room));
  }
  const windows = seeds
    .filter((seed) => kept.has(seed))
    .map((seed) => ({ lo: seed, hi: seed }));
  for (let grew = true; grew;) {
    grew = false;
    for (const w of windows) {
      if (w.hi + 1 < units.length && take(w.hi + 1)) {
        w.hi++;
        grew = true;
      }
      if (w.lo > 0 && take(w.lo - 1)) {
        w.lo--;
        grew = true;
      }
    }
  }
  const out: SourceReference[] = [];
  let last = -2;
  for (const i of [...kept.keys()].sort((a, b) => a - b)) {
    const unit = units[i]!;
    const ref = out.at(-1);
    if (ref && ref.location === chapter.excerpts[unit.excerpt]!.location)
      ref.text += (i === last + 1 ? "\n" : "\n…\n") + kept.get(i);
    else out.push({ ...chapter.excerpts[unit.excerpt]!, text: kept.get(i)! });
    last = i;
  }
  return out;
}

function anchorUnit(
  units: { excerpt: number; text: string }[],
  chapter: Chapter,
  anchor: Anchor,
) {
  const page = (ref: SourceReference) =>
    ref.location.type === "pdf" ? ref.location.page : undefined;
  const target =
    anchor.location.type === "pdf" ? anchor.location.page : undefined;
  const inExcerpt = units
    .map((unit, i) => ({ unit, i }))
    .filter(
      ({ unit }) =>
        target === undefined ||
        page(chapter.excerpts[unit.excerpt]!) === target,
    );
  const candidates = inExcerpt.length
    ? inExcerpt
    : units.map((unit, i) => ({ unit, i }));
  const hit = candidates.find(
    ({ unit }) => quoteIndex(unit.text, anchor.text) >= 0,
  );
  return (hit ?? candidates[0]!).i;
}

/** Finds a quote's start despite PDF text-layer whitespace differences. */
function quoteIndex(text: string, quote?: string) {
  const probe = quote?.replace(/\s+/g, " ").trim().slice(0, 24);
  if (!probe) return -1;
  return text.replace(/\s+/g, " ").indexOf(probe);
}
