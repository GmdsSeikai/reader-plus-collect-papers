import { expect, it, vi } from "vitest";
import type { ReaderAdapter, ReaderSelection } from "@reader/core";
import { composeChatContext } from "./compose-context";

function fixture() {
  const getContext = vi.fn(async () => "当前章的原文");
  const getLocation = vi.fn(() => ({
    type: "epub" as const,
    href: "chapter-2.xhtml",
  }));
  const adapter = { getContext, getLocation } as unknown as ReaderAdapter;
  return { adapter, getContext, getLocation };
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
    expect(result.context).toContain("当前章的原文");
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
it("bounds excerpts and keeps reference text equal to the text actually sent", async () => {
  const f = fixture();
  const result = await composeChatContext({
    adapter: f.adapter,
    selected: [{ ...quote, text: "原文".repeat(20000) }, quote],
  });
  expect(result.context.length).toBeLessThanOrEqual(21000);
  expect(new TextEncoder().encode(result.context).length).toBeLessThanOrEqual(
    64000,
  );
  expect(result.references).toHaveLength(1);
  expect(result.context.endsWith(result.references[0]!.text)).toBe(true);
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
