import type { TOCItem } from "@reader/core";

export interface PageRange {
  fromPage: number;
  toPage: number;
}

/**
 * The outline chapter containing `page`, as an inclusive page range. A single
 * root entry (the book title) is skipped for its children. Without an outline
 * (the PDF adapter then lists every page) the page and its neighbours are used.
 */
export function chapterRange(
  toc: TOCItem[],
  page: number,
  pages: number,
): PageRange {
  return outlineRange(toc, page, pages, (item) => [item]);
}

/**
 * The section around a quoted page, sent as chat context. Outline entries two
 * levels deep split parts into chapters.
 */
export function quoteSectionRange(
  toc: TOCItem[],
  page: number,
  pages: number,
): PageRange {
  return outlineRange(toc, page, pages, (item) => [item, ...item.children]);
}

function outlineRange(
  toc: TOCItem[],
  page: number,
  pages: number,
  expand: (item: TOCItem) => TOCItem[],
): PageRange {
  const entries = toc.length === 1 ? toc[0].children : toc;
  const starts = [
    ...new Set(
      entries
        .flatMap(expand)
        .flatMap((item) =>
          item.location.type === "pdf" && !item.id.startsWith("page-")
            ? [item.location.page]
            : [],
        ),
    ),
  ].sort((a, b) => a - b);
  const current = Math.min(Math.max(1, page), pages);
  if (!starts.length)
    return {
      fromPage: Math.max(1, current - 1),
      toPage: Math.min(pages, current + 1),
    };
  const fromPage = starts.filter((start) => start <= current).at(-1) ?? 1;
  const next = starts.find((start) => start > current);
  return { fromPage, toPage: next ? Math.max(fromPage, next - 1) : pages };
}
