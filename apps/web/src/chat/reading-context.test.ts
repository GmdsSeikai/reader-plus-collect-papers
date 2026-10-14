import { expect, it, vi } from "vitest";
import type { EPUBLocation, PDFLocation, ReaderSelection } from "@reader/core";
import { readingContext } from "./reading-context";

it.each(["codex", "api"])(
  "%s sends a PDF position without extracting or attaching page text",
  async (provider) => {
    const position: PDFLocation = {
      type: "pdf",
      page: 5,
      y: 0.6,
      quote: "not an explicit quote",
    };
    const reader = {
      getLocation: () => position,
      getContext: vi.fn(async () => "page text"),
    };
    const pending = readingContext(reader, provider, [], false, false);
    position.page = 9;
    const result = await pending;
    expect(result).toMatchObject({
      context: "",
      references: [],
      positionOnly: true,
      readingLocation: { type: "pdf", page: 5, y: 0.6 },
    });
    expect(result.readingLocation).not.toHaveProperty("quote");
    expect(reader.getContext).not.toHaveBeenCalled();
  },
);

it("keeps an EPUB native position for explicit current-chapter questions without attaching the chapter", async () => {
  const position: EPUBLocation = {
    type: "epub",
    href: "ch2.xhtml#section",
    progression: 0.7,
    locator: '{"href":"ch2.xhtml","type":"application/xhtml+xml"}',
    blockId: "b7",
  };
  const reader = {
    getLocation: () => position,
    getContext: vi.fn(async () => "whole chapter"),
  };
  const result = await readingContext(reader, "codex", [], true, false);
  position.href = "ch3.xhtml";
  expect(result.readingLocation).toMatchObject({
    href: "ch2.xhtml#section",
    progression: 0.7,
    locator: position.locator,
    blockId: "b7",
  });
  expect(result.context).toBe("");
  expect(result.references).toEqual([]);
  expect(reader.getContext).not.toHaveBeenCalled();
});

it.each(["codex", "api", "claude", "kimi"])(
  "%s sends explicitly selected text and freezes its source",
  async (provider) => {
    const selected: ReaderSelection[] = [
      { text: "exact selected sentence", location: { type: "pdf", page: 2 } },
    ];
    const reader = {
      getLocation: () => ({ type: "pdf" as const, page: 8 }),
      getContext: vi.fn(async () => "unrelated page"),
      getChapterContext: vi.fn(async () => [
        { text: "full chapter", location: { type: "pdf" as const, page: 2 } },
      ]),
    };
    const result = await readingContext(
      reader,
      provider,
      selected,
      false,
      false,
    );
    selected[0].location = { type: "pdf", page: 3 };
    expect(result.context).toContain("exact selected sentence");
    expect(result.context).toContain("full chapter");
    expect(result.references).toEqual([
      {
        text: "exact selected sentence",
        location: { type: "pdf", page: 2 },
        kind: "selection",
      },
    ]);
    expect(result.positionOnly).toBe(false);
    expect(reader.getContext).not.toHaveBeenCalled();
  },
);

it.each([
  ["claude", true],
  ["kimi", true],
  ["codex", true],
  ["api", true],
] as const)(
  "preserves automatic text for legacy/image path %s (images=%s)",
  async (provider, hasImages) => {
    const position: PDFLocation = { type: "pdf", page: 4 };
    const reader = {
      getLocation: () => position,
      getContext: vi.fn(async () => {
        position.page = 7;
        return "page four text";
      }),
    };
    const result = await readingContext(reader, provider, [], false, hasImages);
    expect(result.context).toContain("page four text");
    expect(result.readingLocation).toMatchObject({ page: 4 });
    expect(result.references[0]).toMatchObject({
      kind: "section",
      location: { page: 4 },
    });
  },
);

it.each(["codex", "api", "claude", "kimi"])(
  "%s honors explicit chapter preference",
  async (provider) => {
    const getChapterContext = vi.fn(async () => [
      {
        text: "requested chapter",
        location: { type: "pdf" as const, page: 3 },
      },
    ]);
    const result = await readingContext(
      {
        getLocation: () => ({ type: "pdf", page: 3 }),
        getContext: async () => "page",
        getChapterContext,
      },
      provider,
      [],
      false,
      false,
      { includeChapter: true },
    );
    expect(result.context).toContain("requested chapter");
    expect(getChapterContext).toHaveBeenCalledOnce();
  },
);
it.each(["claude", "kimi"])(
  "%s keeps main's default without implicit page text",
  async (provider) => {
    const getContext = vi.fn(async () => "page");
    const result = await readingContext(
      { getLocation: () => ({ type: "pdf", page: 3 }), getContext },
      provider,
      [],
      false,
      false,
    );
    expect(result.context).toBe("");
    expect(getContext).not.toHaveBeenCalled();
  },
);
