// @vitest-environment jsdom
import { expect, it, vi } from "vitest";
import type { TOCItem } from "@reader/core";
vi.mock("pdfjs-dist", () => ({ GlobalWorkerOptions: {} }));
vi.mock("pdfjs-dist/web/pdf_viewer.mjs", () => ({
  EventBus: class {},
  PDFLinkService: class {},
  PDFViewer: class {},
}));
import { PDFReaderAdapter } from "./pdf";

function fixture(toc: TOCItem[], text = (page: number) => `原文页${page}`) {
  const getPage = vi.fn(async (page: number) => ({
    getTextContent: async () => ({ items: [{ str: text(page) }] }),
  }));
  // Exercise extraction without mounting the viewer; PDF.js is the text source.
  const reader = Object.create(PDFReaderAdapter.prototype) as PDFReaderAdapter;
  Object.assign(reader, {
    pdf: { numPages: 45, getPage },
    texts: new Map(),
    blockData: [],
    location: { type: "pdf", page: 1 },
  });
  vi.spyOn(reader, "getTOC").mockResolvedValue(toc);
  return { reader, getPage };
}
it("extracts the visible chapter across pages using its outline boundaries", async () => {
  const f = fixture(
    [3, 6, 10].map((page) => ({
      id: `chapter-${page}`,
      label: `章${page}`,
      location: { type: "pdf", page },
      children: [],
    })),
  );
  const excerpts = await f.reader.getChapterContext({ type: "pdf", page: 8 });
  expect(excerpts.map((e) => e.location)).toEqual(
    [6, 7, 8, 9].map((page) => ({ type: "pdf", page })),
  );
  expect(excerpts.map((e) => e.text)).toEqual(
    [6, 7, 8, 9].map((page) => `原文页${page}`),
  );
  expect(f.getPage.mock.calls.map(([page]) => page)).toEqual([6, 7, 8, 9]);
});
it("preserves full chapter text beyond the previous character limit", async () => {
  const f = fixture([], () => "文".repeat(25000));
  Object.assign(f.reader, {
    blockData: [21, 24, 30].map((page) => ({
      id: `h${page}`,
      page,
      label: "paragraph_title",
      text: `标题${page}`,
    })),
  });
  const excerpts = await f.reader.getChapterContext({ type: "pdf", page: 25 });
  expect(excerpts.map((e) => e.location)).toEqual(
    [24, 25, 26, 27, 28, 29].map((page) => ({ type: "pdf", page })),
  );
  expect(excerpts[0]!.text).toHaveLength(25000);
});
it("sends only the quoted page of an unparsed PDF without an outline", async () => {
  const f = fixture([]);
  const excerpts = await f.reader.getChapterContext({ type: "pdf", page: 25 });
  expect(excerpts.map((e) => e.location)).toEqual([{ type: "pdf", page: 25 }]);
});
