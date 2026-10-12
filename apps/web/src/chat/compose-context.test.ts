import { expect, it, vi } from "vitest";
import type { ReaderAdapter, ReaderSelection } from "@reader/core";
import { composeChatContext } from "./compose-context";

function fixture() {
  const getContext = vi.fn(async () => "当前章的原文");
  const getLocation = vi.fn(() => ({
    type: "epub" as const,
    href: "chapter-2.xhtml",
  }));
  const getChapterContext = vi.fn(
    async (location: ReaderSelection["location"]) => [
      { text: "完整章节原文，直到章末。", location },
    ],
  );
  const adapter = {
    getContext,
    getLocation,
    getChapterContext,
  } as unknown as ReaderAdapter;
  return { adapter, getContext, getLocation, getChapterContext };
}
const quote: ReaderSelection = {
  text: "主动引用",
  location: { type: "pdf", page: 7 },
};

it("ordinary questions do not read or attach chapter text or annotations by default", async () => {
  const f = fixture();
  const result = await composeChatContext({
    adapter: f.adapter,
    selected: [],
    marks: "我的批注与原文",
  });
  expect(result).toEqual({ context: "", references: [] });
  expect(f.getContext).not.toHaveBeenCalled();
  expect(f.getLocation).not.toHaveBeenCalled();
});
it("manual quotations remain available with automatic chapter context disabled", async () => {
  const f = fixture();
  const result = await composeChatContext({
    adapter: f.adapter,
    selected: [quote],
    marks: "其他批注",
  });
  expect(result.context).toContain(quote.text);
  expect(result.context).not.toContain("其他批注");
  expect(result.context).toContain("完整章节原文，直到章末。");
  expect(f.getChapterContext).toHaveBeenCalledWith(quote.location);
  expect(f.getLocation).not.toHaveBeenCalled();
  expect(result.references).toEqual([{ ...quote, kind: "selection" }]);
  expect(f.getContext).not.toHaveBeenCalled();
});
it.each([{ includeChapter: true }, { useSection: true }])(
  "opt-in and explicit chapter actions attach original text: %j",
  async (options) => {
    const f = fixture();
    const result = await composeChatContext({
      adapter: f.adapter,
      selected: [],
      marks: "批注",
      ...options,
    });
    expect(result.context).toContain(
      "useSection" in options ? "当前章的原文" : "完整章节原文，直到章末。",
    );
    expect(result.context).toContain("批注");
    expect(result.references[0]).toMatchObject({
      kind: "section",
      location: { type: "epub", href: "chapter-2.xhtml", progression: 0 },
    });
  },
);
it("uses the visible PDF chapter location and preserves each page's reference", async () => {
  const f = fixture();
  f.adapter.getLocation = () => ({ type: "pdf", page: 8 });
  const getChapterContext = vi.fn(async () => [
    { text: "本章第一页", location: { type: "pdf" as const, page: 6 } },
    { text: "本章第二页", location: { type: "pdf" as const, page: 7 } },
  ]);
  f.adapter.getChapterContext = getChapterContext;
  const result = await composeChatContext({
    adapter: f.adapter,
    selected: [],
    includeChapter: true,
  });
  expect(getChapterContext).toHaveBeenCalledWith({ type: "pdf", page: 8 });
  expect(f.getContext).not.toHaveBeenCalled();
  expect(result.references.map((r) => r.location)).toEqual([
    { type: "pdf", page: 6 },
    { type: "pdf", page: 7 },
  ]);
});
it("sends a whole long chapter beyond the old character cutoff", async () => {
  const f = fixture();
  const chapter = "a".repeat(30000) + "章末";
  f.getChapterContext.mockResolvedValue([
    { text: chapter, location: quote.location },
  ]);
  const result = await composeChatContext({
    adapter: f.adapter,
    selected: [quote],
  });
  expect(result.context).toContain(chapter);
  expect(result.references).toEqual([{ ...quote, kind: "selection" }]);
});
it("rejects oversized full context instead of silently truncating it", async () => {
  const f = fixture();
  f.getChapterContext.mockResolvedValue([
    { text: "文".repeat(22000), location: quote.location },
  ]);
  await expect(
    composeChatContext({ adapter: f.adapter, selected: [quote] }),
  ).rejects.toThrow("无法完整发送");
});
it("includes each quoted chapter once even after navigating elsewhere", async () => {
  const f = fixture();
  f.getChapterContext.mockImplementation(async (location) => [
    {
      text:
        location.type === "pdf" && location.page < 10
          ? "完整第一章"
          : "完整第二章",
      location: {
        type: "pdf",
        page: location.type === "pdf" && location.page < 10 ? 1 : 10,
      },
    },
  ]);
  const quotes: ReaderSelection[] = [
    quote,
    { text: "另一选区", location: { type: "pdf", page: 8 } },
    { text: "第二章选区", location: { type: "pdf", page: 12 } },
  ];
  const result = await composeChatContext({
    adapter: f.adapter,
    selected: quotes,
  });
  expect(result.context.match(/完整第一章/g)).toHaveLength(1);
  expect(result.context.match(/完整第二章/g)).toHaveLength(1);
  expect(result.references.map((r) => r.text)).toEqual(
    quotes.map((q) => q.text),
  );
  expect(f.getLocation).not.toHaveBeenCalled();
});

it("the explicit current-page action keeps its page scope even when chapter context is enabled", async () => {
  const f = fixture();
  f.adapter.getLocation = () => ({ type: "pdf", page: 8 });
  f.adapter.getChapterContext = vi.fn(async () => [quote]);
  f.getContext.mockResolvedValue("当前页原文");
  const result = await composeChatContext({
    adapter: f.adapter,
    selected: [],
    includeChapter: true,
    useSection: true,
  });
  expect(result.context).toContain("当前页原文");
  expect(f.adapter.getChapterContext).not.toHaveBeenCalled();
  expect(result.references[0]!.location).toEqual({ type: "pdf", page: 8 });
});

it("does not send a quote alone when its chapter cannot be read", async () => {
  const f = fixture();
  f.getChapterContext.mockResolvedValue([]);
  await expect(
    composeChatContext({ adapter: f.adapter, selected: [quote] }),
  ).rejects.toThrow("无法读取引用所在的完整章节");
});
