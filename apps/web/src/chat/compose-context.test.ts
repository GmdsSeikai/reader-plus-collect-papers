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
it("keeps the pages around a quote when its chapter exceeds the limit", async () => {
  const f = fixture();
  // 40 dense pages of about 6 KB each, quoting one sentence on page 20.
  f.getChapterContext.mockResolvedValue(
    Array.from({ length: 40 }, (_, i) => ({
      text: Array.from(
        { length: 20 },
        (_, j) => `第${i + 1}页第${j + 1}段` + "文".repeat(90),
      ).join("\n"),
      location: { type: "pdf" as const, page: i + 1 },
    })),
  );
  const sentence: ReaderSelection = {
    text: "第20页第10段文文文",
    location: { type: "pdf", page: 20 },
  };
  const confirmOversize = vi.fn(async () => "trim" as const);
  const result = await composeChatContext({
    adapter: f.adapter,
    selected: [sentence],
    confirmOversize,
  });
  expect(confirmOversize).toHaveBeenCalledWith(
    expect.objectContaining({ full: true, trim: true, quoted: true }),
  );
  expect(new TextEncoder().encode(result.context).length).toBeLessThanOrEqual(
    64000,
  );
  expect(result.context).toContain(sentence.text);
  expect(result.context).toContain("第20页第1段");
  expect(result.context).toContain("第20页第20段");
  expect(result.context).toContain("辅助章节原文（节选）");
  expect(result.context).not.toContain("第1页第1段");
  expect(result.context).not.toContain("第40页第20段");
  expect(result.references).toEqual([{ ...sentence, kind: "selection" }]);
});
it("keeps the paragraphs around a quote in a long EPUB chapter", async () => {
  const f = fixture();
  const paragraphs = Array.from(
    { length: 600 },
    (_, i) => `段落${i}：` + "文".repeat(100),
  );
  f.getChapterContext.mockResolvedValue([
    { text: paragraphs.join("\n\n"), location: quote.location },
  ]);
  const result = await composeChatContext({
    adapter: f.adapter,
    selected: [{ text: "段落300：文文", location: quote.location }],
    confirmOversize: async () => "trim",
  });
  expect(new TextEncoder().encode(result.context).length).toBeLessThanOrEqual(
    64000,
  );
  expect(result.context).toContain("段落299：");
  expect(result.context).toContain("段落301：");
  expect(result.context).not.toContain("段落0：");
  expect(result.context).not.toContain("段落599：");
});
it("trims an EPUB chapter around the reading position without a quote", async () => {
  const f = fixture();
  const paragraphs = Array.from(
    { length: 600 },
    (_, i) => `段落${i}：` + "文".repeat(100),
  );
  const location = {
    type: "epub" as const,
    href: "chapter-2.xhtml",
    progression: 0.75,
  };
  f.adapter.getLocation = () => location;
  f.getChapterContext.mockResolvedValue([
    { text: paragraphs.join("\n\n"), location },
  ]);
  const result = await composeChatContext({
    adapter: f.adapter,
    selected: [],
    includeChapter: true,
    confirmOversize: async () => "trim",
  });
  expect(result.context).toContain("段落450：");
  expect(result.context).not.toContain("段落0：");
});
it("cuts a single oversized paragraph around the quote", async () => {
  const f = fixture();
  f.getChapterContext.mockResolvedValue([
    {
      text: "前".repeat(30000) + "引用句" + "后".repeat(30000),
      location: quote.location,
    },
  ]);
  const result = await composeChatContext({
    adapter: f.adapter,
    selected: [{ text: "引用句", location: quote.location }],
    confirmOversize: async () => "trim",
  });
  expect(new TextEncoder().encode(result.context).length).toBeLessThanOrEqual(
    64000,
  );
  expect(result.context.match(/引用句/g)).toHaveLength(2);
});
it("sends a long chapter whole when the user confirms", async () => {
  const f = fixture();
  const chapter = "文".repeat(40000);
  f.getChapterContext.mockResolvedValue([
    { text: chapter, location: quote.location },
  ]);
  const confirmOversize = vi.fn(async () => "full" as const);
  const result = await composeChatContext({
    adapter: f.adapter,
    selected: [quote],
    confirmOversize,
  });
  expect(confirmOversize).toHaveBeenCalledOnce();
  expect(result.context).toContain(chapter);
  expect(result.context).not.toContain("节选");
});
it("cancels quietly when the user declines a long context", async () => {
  const f = fixture();
  f.getChapterContext.mockResolvedValue([
    { text: "文".repeat(40000), location: quote.location },
  ]);
  await expect(
    composeChatContext({
      adapter: f.adapter,
      selected: [quote],
      confirmOversize: async () => "cancel",
    }),
  ).rejects.toMatchObject({ name: "AbortError" });
});
it("asks before sending a selection above 64 KB and never shortens it", async () => {
  const f = fixture();
  const text = "文".repeat(22000);
  const confirmOversize = vi.fn(async () => "full" as const);
  const result = await composeChatContext({
    adapter: f.adapter,
    selected: [{ text, location: quote.location }],
    confirmOversize,
  });
  expect(confirmOversize).toHaveBeenCalledWith(
    expect.objectContaining({ full: true, trim: false }),
  );
  expect(result.context).toContain(text);
});
it("offers only nearby text when a chapter exceeds the hard cap", async () => {
  const f = fixture();
  f.getChapterContext.mockResolvedValue([
    { text: "文".repeat(90000), location: quote.location },
  ]);
  const confirmOversize = vi.fn(async () => "trim" as const);
  await composeChatContext({
    adapter: f.adapter,
    selected: [quote],
    confirmOversize,
  });
  expect(confirmOversize).toHaveBeenCalledWith(
    expect.objectContaining({ full: false, trim: true }),
  );
});
it("rejects selections above the hard cap without reading chapters", async () => {
  const f = fixture();
  await expect(
    composeChatContext({
      adapter: f.adapter,
      selected: [{ text: "文".repeat(90000), location: quote.location }],
      confirmOversize: async () => "full",
    }),
  ).rejects.toThrow("无法发送");
  expect(f.getChapterContext).not.toHaveBeenCalled();
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
