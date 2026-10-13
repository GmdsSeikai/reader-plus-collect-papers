// @vitest-environment jsdom
import { act } from "react";
import { createRoot, type Root } from "react-dom/client";
import { afterEach, beforeEach, expect, it, vi } from "vitest";
import { AnalysisPanel } from "./AnalysisPanel";
import type { CollectionAnalysis, CollectionAnalysisJob } from "@reader/api";

const { candidate, analyze, cancelAnalysis } = vi.hoisted(() => ({
  candidate: vi.fn(),
  analyze: vi.fn(),
  cancelAnalysis: vi.fn(),
}));
vi.mock("@reader/api", () => ({
  collectionApi: { candidate, analyze, cancelAnalysis },
}));

let root: Root, host: HTMLDivElement;
const job: CollectionAnalysisJob = {
  id: "job-one",
  arxivId: "2301.12345",
  expectedContentHash: "hash",
  status: "queued",
  createdAt: "2026-10-13T00:00:00Z",
};
const analysis: CollectionAnalysis = {
  cacheKey: "cache",
  arxivId: job.arxivId,
  contentHash: "hash",
  profileHash: "profile",
  modelFingerprint: "model",
  promptVersion: 1,
  resultVersion: 1,
  status: "succeeded",
  attempt: 1,
  createdAt: "",
  updatedAt: "",
  result: {
    schema_version: 1,
    title_zh: "完整中文标题",
    abstract_zh: "完整中文摘要。",
    assessments: [
      {
        taskId: "task-one",
        profileHash: "profile",
        relevance_score: 80,
        reason_zh: "研究方向相符。",
        evidence: [{ field: "abstract", quote: "Original abstract quote" }],
      },
    ],
  },
};
const render = async (
  initialJob: CollectionAnalysisJob | null = null,
  analyses: CollectionAnalysis[] = [],
) => {
  await act(async () => {
    root.render(
      <AnalysisPanel
        candidateId={job.arxivId}
        analyses={analyses}
        job={initialJob}
        taskNames={{ "task-one": "视觉研究" }}
      />,
    );
  });
};
beforeEach(() => {
  vi.stubGlobal("IS_REACT_ACT_ENVIRONMENT", true);
  vi.useFakeTimers();
  vi.resetAllMocks();
  host = document.createElement("div");
  document.body.append(host);
  root = createRoot(host);
});
afterEach(async () => {
  await act(async () => root.unmount());
  host.remove();
  vi.useRealTimers();
});

it("submits a persistent job and displays only the returned valid translation and quotes", async () => {
  analyze.mockResolvedValue(job);
  candidate.mockResolvedValue({
    analyses: [analysis],
    analysisJob: { ...job, status: "succeeded" },
  });
  await render();
  await act(async () => host.querySelector("button")!.click());
  expect(analyze).toHaveBeenCalledExactlyOnceWith(job.arxivId);
  expect(host.textContent).toContain("等待分析");
  await act(async () => {
    await vi.advanceTimersByTimeAsync(300);
  });
  expect(host.textContent).toContain("完整中文标题");
  expect(host.textContent).toContain("完整中文摘要。");
  expect(host.textContent).toContain("视觉研究 · 80 分");
  expect(host.textContent).toContain("Original abstract quote");
  expect(host.textContent).toContain("分析完成");
});

it("stops polling on page exit without cancelling the server-owned job", async () => {
  await render(job);
  await act(async () => root.render(<div>other page</div>));
  await act(async () => {
    await vi.advanceTimersByTimeAsync(3000);
  });
  expect(candidate).not.toHaveBeenCalled();
  expect(cancelAnalysis).not.toHaveBeenCalled();
});

it("cancels the exact active job and renders its persisted terminal status", async () => {
  cancelAnalysis.mockResolvedValue({ id: job.id });
  candidate.mockResolvedValue({
    analyses: [],
    analysisJob: { ...job, status: "cancelled", error: "用户取消" },
  });
  await render(job);
  await act(async () => host.querySelector("button")!.click());
  expect(cancelAnalysis).toHaveBeenCalledExactlyOnceWith(job.id);
  expect(host.textContent).toContain("分析已取消");
  expect(host.textContent).not.toContain("完整中文摘要。");
});

it("displays rejected analysis without inventing a translation", async () => {
  analyze.mockRejectedValue(new Error("摘要分析今日额度已用完"));
  await render();
  await act(async () => host.querySelector("button")!.click());
  expect(host.querySelector('[role="alert"]')?.textContent).toBe(
    "摘要分析今日额度已用完",
  );
  expect(host.textContent).not.toContain("完整中文摘要。");
  expect(candidate).not.toHaveBeenCalled();
});
