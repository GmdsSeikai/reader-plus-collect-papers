import type { PDFBlock, TOCItem } from "@reader/core";

export interface PageRange {
  fromPage: number;
  toPage: number;
}

/**
 * Pages where parsed headings start a section. Chapters skip numbered
 * subsections such as "2.1"; quotes keep them for a narrower section.
 */
export function headingPages(blocks: PDFBlock[], subsections: boolean) {
  return blocks
    .filter(
      (b) =>
        (b.label === "paragraph_title" || b.label === "doc_title") &&
        (subsections || !/^\s*[\dA-Z]+\.\d/.test(b.text)),
    )
    .map((b) => b.page);
}

/**
 * The outline chapter containing `page`, as an inclusive page range. A single
 * root entry (the book title) is skipped for its children. Without an outline
 * (the PDF adapter then lists every page) parsed headings mark the chapters,
 * and an unparsed PDF uses the page and its neighbours.
 */
export function chapterRange(
  toc: TOCItem[],
  page: number,
  pages: number,
  headings: number[] = [],
): PageRange {
  return outlineRange(toc, page, pages, headings, (item) => [item]);
}

/**
 * The section around a quoted page, sent as chat context. Outline entries two
 * levels deep split parts into chapters.
 */
export function quoteSectionRange(
  toc: TOCItem[],
  page: number,
  pages: number,
  headings: number[] = [],
): PageRange {
  return outlineRange(toc, page, pages, headings, (item) => [
    item,
    ...item.children,
  ]);
}

function outlineRange(
  toc: TOCItem[],
  page: number,
  pages: number,
  headings: number[],
  expand: (item: TOCItem) => TOCItem[],
): PageRange {
  const entries = toc.length === 1 ? toc[0].children : toc;
  const outline = entries
    .flatMap(expand)
    .flatMap((item) =>
      item.location.type === "pdf" && !item.id.startsWith("page-")
        ? [item.location.page]
        : [],
    );
  const starts = [...new Set(outline.length ? outline : headings)].sort(
    (a, b) => a - b,
  );
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
