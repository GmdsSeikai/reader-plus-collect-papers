import {
  locationLabel,
  type DocumentLocation,
  type ReaderAdapter,
  type ReaderSelection,
  type SourceReference,
} from "@reader/core";
import { contextReferences } from "../references";

/** Above this the user confirms the send; long chapters may be trimmed to it. */
export const contextSoftLimit = 64000;
/** Mirrors the server's hard cap on a single request's context. */
export const contextHardLimit = 256000;
const encoder = new TextEncoder();
const byteLength = (text: string) => encoder.encode(text).length;

type Anchor = { location: DocumentLocation; text?: string };
type Chapter = { anchors: Anchor[]; excerpts: SourceReference[] };

export interface OversizeContext {
  /** UTF-8 bytes of the complete context. */
  bytes: number;
  /** The complete context fits under the hard cap. */
  full: boolean;
  /** Chapters can be cut to the text near each quote. */
  trim: boolean;
  quoted: boolean;
}
export type OversizeChoice = "full" | "trim" | "cancel";

/** Explicit quotations carry their complete chapters as auxiliary context. */
export async function composeChatContext({
  adapter,
  selected,
  includeChapter = false,
  useSection = false,
  marks = "",
  confirmOversize,
}: {
  adapter: ReaderAdapter | null | undefined;
  selected: ReaderSelection[];
  includeChapter?: boolean;
  useSection?: boolean;
  marks?: string;
  /** Asks whether to send a context above the soft limit. */
  confirmOversize?: (request: OversizeContext) => Promise<OversizeChoice>;
}): Promise<{ context: string; references: SourceReference[] }> {
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
  if (used() > contextHardLimit)
    throw new Error(
      "选区超过 256 KB，无法发送。请减少引用的内容，或分开提问。",
    );

  const attach = Boolean(
    (selected.length || includeChapter || useSection) && adapter,
  );
  const chapters: Chapter[] = [];
  const owner = new Map<string, Chapter>();
  if (adapter && attach)
    for (const anchor of selected.length
      ? (selected as Anchor[])
      : [{ location: adapter.getLocation() }]) {
      // Explicit page actions retain their scope. A quote always needs its chapter.
      const excerpts =
        adapter.getChapterContext && (!useSection || selected.length)
          ? await adapter.getChapterContext(anchor.location)
          : [{ text: await adapter.getContext(), location: anchor.location }];
      if (selected.length && !excerpts.some((excerpt) => excerpt.text.trim()))
        throw new Error(
          "无法读取引用所在的完整章节，请等待文档解析完成后重试。",
        );
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

  const tail = marks && attach && !selected.length ? marks.slice(0, 4000) : "";
  const prefix = selected.length ? "辅助章节原文" : "原文";
  const header = (ref: SourceReference, trimmed: boolean) =>
    `[${prefix}${trimmed ? "（节选）" : ""} · ${locationLabel(ref.location)}]\n`;
  const full = chapters.flatMap((chapter) =>
    chapter.excerpts.map((ref) => header(ref, false) + ref.text),
  );
  const fixed = used() + (tail ? byteLength(tail) + 2 : 0);
  const bytes =
    fixed +
    full.reduce(
      (sum, part, i) => sum + byteLength(part) + (parts.length || i ? 2 : 0),
      0,
    );
  let choice: OversizeChoice = "full";
  if (bytes > contextSoftLimit) {
    const request = {
      bytes,
      full: bytes <= contextHardLimit,
      // Trimming only helps when the quotes leave room for nearby text.
      trim: chapters.length > 0 && fixed < contextSoftLimit,
      quoted: selected.length > 0,
    };
    if (!request.full && !request.trim)
      throw new Error(
        "上下文超过 256 KB，无法发送。请减少引用的内容，或分开提问。",
      );
    choice = confirmOversize
      ? await confirmOversize(request)
      : request.trim
        ? "trim"
        : "full";
    if (choice === "cancel")
      throw Object.assign(new Error("已取消发送"), { name: "AbortError" });
  }
  if (choice === "full") {
    parts.push(...full);
    // Keep the user's chosen passages as the visible quotations.
    if (!selected.length)
      references.push(...chapters.flatMap((chapter) => chapter.excerpts));
  } else {
    // Long chapters keep the paragraphs nearest each quote, labelled as excerpts.
    let budget = contextSoftLimit - fixed;
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
  }
  if (tail) parts.push(tail);
  return { context: parts.join("\n\n"), references };
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
  const quote = anchor.text ?? anchor.location.quote;
  const hit = candidates.find(({ unit }) => quoteIndex(unit.text, quote) >= 0);
  if (hit) return hit.i;
  // Without a quote, an EPUB reading position locates the paragraph by how far
  // through the chapter it is.
  const progression =
    anchor.location.type === "epub" ? anchor.location.progression : undefined;
  if (progression !== undefined && progression > 0) {
    const total = candidates.reduce((sum, c) => sum + c.unit.text.length, 0);
    let seen = 0;
    for (const c of candidates) {
      seen += c.unit.text.length;
      if (seen >= progression * total) return c.i;
    }
  }
  return candidates[0]!.i;
}

/** Finds a quote's start despite PDF text-layer whitespace differences. */
function quoteIndex(text: string, quote?: string) {
  const probe = quote?.replace(/\s+/g, " ").trim().slice(0, 24);
  if (!probe) return -1;
  return text.replace(/\s+/g, " ").indexOf(probe);
}
