import { expect, it } from "vitest";
import type { PDFBlock, TOCItem } from "@reader/core";
import { chapterRange, headingPages, quoteSectionRange } from "./chapters";

const entry = (
  id: string,
  page: number,
  children: TOCItem[] = [],
): TOCItem => ({
  id,
  label: id,
  location: { type: "pdf", page },
  children,
});

it("spans from the chapter start to the page before the next chapter", () => {
  const toc = [entry("1", 5), entry("2", 20), entry("3", 41)];
  expect(chapterRange(toc, 25, 100)).toEqual({ fromPage: 20, toPage: 40 });
  expect(chapterRange(toc, 41, 100)).toEqual({ fromPage: 41, toPage: 100 });
  expect(chapterRange(toc, 2, 100)).toEqual({ fromPage: 1, toPage: 4 });
});

it("uses the children of a single title entry and ignores sections", () => {
  const toc = [
    entry("book", 1, [
      entry("0", 3, [entry("0-0", 4)]),
      entry("1", 30, [entry("1-0", 35)]),
    ]),
  ];
  expect(chapterRange(toc, 36, 80)).toEqual({ fromPage: 30, toPage: 80 });
});

it("keeps a chapter sharing its start page with the next one", () => {
  const toc = [entry("1", 10), entry("2", 10), entry("3", 12)];
  expect(chapterRange(toc, 10, 20)).toEqual({ fromPage: 10, toPage: 11 });
});

it("uses the page and its neighbours for a PDF without an outline", () => {
  const pages = [1, 2, 3].map((page) => entry(`page-${page}`, page));
  expect(chapterRange(pages, 2, 3)).toEqual({ fromPage: 1, toPage: 3 });
  expect(chapterRange([], 25, 90)).toEqual({ fromPage: 24, toPage: 26 });
  expect(chapterRange([], 90, 90)).toEqual({ fromPage: 89, toPage: 90 });
});

it("quote sections split parts into their chapters", () => {
  const toc = [
    entry("part-1", 1, [entry("0-0", 3), entry("0-1", 12)]),
    entry("part-2", 30, [entry("1-0", 31), entry("1-1", 44)]),
  ];
  expect(quoteSectionRange(toc, 15, 80)).toEqual({
    fromPage: 12,
    toPage: 29,
  });
  expect(quoteSectionRange(toc, 50, 80)).toEqual({
    fromPage: 44,
    toPage: 80,
  });
});

it("quote sections without an outline span the quoted page and its neighbours", () => {
  const pages = Array.from({ length: 20 }, (_, i) =>
    entry(`page-${i + 1}`, i + 1),
  );
  expect(quoteSectionRange(pages, 7, 20)).toEqual({ fromPage: 6, toPage: 8 });
  expect(quoteSectionRange([], 1, 20)).toEqual({ fromPage: 1, toPage: 2 });
  expect(quoteSectionRange([], 20, 20)).toEqual({ fromPage: 19, toPage: 20 });
});

it("parsed headings mark sections when the PDF has no outline", () => {
  const heading = (page: number, text: string): PDFBlock => ({
    id: `${page}-${text}`,
    page,
    label: "paragraph_title",
    bounds: { x: 0, y: 0, width: 1, height: 1 },
    text,
  });
  const blocks = [
    heading(1, "Abstract"),
    heading(2, "1 Introduction"),
    heading(4, "2 Method"),
    heading(6, "2.1 Data"),
    heading(9, "3 Results"),
    { ...heading(5, "Figure 1"), label: "figure_title" },
  ];
  expect(headingPages(blocks, true)).toEqual([1, 2, 4, 6, 9]);
  expect(headingPages(blocks, false)).toEqual([1, 2, 4, 9]);
  // Translation keeps the whole chapter; quotes stop at the subsection.
  expect(chapterRange([], 7, 20, headingPages(blocks, false))).toEqual({
    fromPage: 4,
    toPage: 8,
  });
  expect(quoteSectionRange([], 7, 20, headingPages(blocks, true))).toEqual({
    fromPage: 6,
    toPage: 8,
  });
  // An outline takes precedence over parsed headings.
  expect(chapterRange([entry("1", 3), entry("2", 12)], 7, 20, [5])).toEqual({
    fromPage: 3,
    toPage: 11,
  });
});
