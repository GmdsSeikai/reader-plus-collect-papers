// @vitest-environment jsdom
import { act } from "react";
import { createRoot, type Root } from "react-dom/client";
import { afterEach, beforeEach, expect, it, vi } from "vitest";
import { CollectionPage } from "./CollectionPage";

// vi.mock factories are hoisted above imports; the mocks they reference must
// be created with vi.hoisted to exist before the factory runs.
const { candidates, tasks, activeImports, createTask, api } = vi.hoisted(() => {
  const api = {
    run: vi.fn(async () => ({ id: "r1", taskId: "t1", status: "running" })),
    cancelRun: vi.fn(async () => ({ status: "cancelled" })),
    candidate: vi.fn(async () => ({ candidate: {}, citation: {} })),
    setIgnored: vi.fn(async () => ({ candidate: {}, citation: {} })),
    importItems: vi.fn(async () => ({ items: [] })),
    importRequest: vi.fn(async () => ({ status: "queued" })),
    confirmImport: vi.fn(async () => ({})),
    cancelImport: vi.fn(async () => ({ status: "cancelled" })),
    settings: vi.fn(async () => ({})),
    saveSettings: vi.fn(async () => ({})),
    pause: vi.fn(async () => ({})),
    resume: vi.fn(async () => ({})),
    runTask: vi.fn(async () => ({ id: "r1", taskId: "t1", status: "running" })),
    updateTask: vi.fn(async () => ({})),
  };
  return {
    candidates: vi.fn(
      async (
        _params: Record<string, string>,
      ): Promise<{
        items: { candidate: Record<string, unknown>; score: number }[];
        nextCursor: string;
      }> => ({ items: [], nextCursor: "" }),
    ),
    tasks: vi.fn(async (): Promise<Record<string, unknown>[]> => []),
    activeImports: vi.fn(
      async (): Promise<Record<string, unknown>[]> => [],
    ),
    createTask: vi.fn(async () => ({})),
    api,
  };
});

vi.mock("@reader/api", () => ({
  collectionApi: {
    candidates,
    tasks,
    activeImports,
    createTask,
    ...api,
  },
}));
vi.mock("sonner", () => ({
  toast: { success: vi.fn(), error: vi.fn(), info: vi.fn() },
}));

let root: Root, host: HTMLDivElement;

beforeEach(async () => {
  vi.stubGlobal("IS_REACT_ACT_ENVIRONMENT", true);
  vi.clearAllMocks();
  host = document.createElement("div");
  document.body.append(host);
  root = createRoot(host);
  await act(async () => {
    root.render(
      <CollectionPage navOpen onToggleNav={() => {}} onOpenDocument={() => {}} />,
    );
  });
});
afterEach(async () => {
  await act(async () => root.unmount());
  host.remove();
  document.body.innerHTML = "";
});

it("requests the latest channel on the latest tab", () => {
  const last = candidates.mock.calls.at(-1)?.[0] as unknown as Record<
    string,
    string
  >;
  expect(last.channel).toBe("latest");
});

it("switches the requested channel when the tab changes", async () => {
  candidates.mockClear();
  const hotTab = host.querySelector('[role="tab"]')?.parentElement
    ?.children[1] as HTMLElement;
  await act(async () => {
    hotTab.click();
  });
  const last = candidates.mock.calls.at(-1)?.[0] as unknown as Record<
    string,
    string
  >;
  expect(last.channel).toBe("hot");
});

it("restores open runs and import state after switching pages", async () => {
  tasks.mockImplementation(async () => [
    { id: "t1", name: "视觉", enabled: true, config: {}, activeRun: { id: "r9", taskId: "t1", status: "running" } },
  ]);
  activeImports.mockImplementation(async () => [
    {
      requestId: "req-1",
      arxivId: "2301.12345",
      status: "awaiting_confirmation",
      condition: "pages",
      confirmPages: 120,
      downloadVersion: 3,
    },
  ]);
  candidates.mockImplementation(async () => ({
    items: [
      {
        candidate: {
          arxivId: "2301.12345",
          title: "论文",
          abstract: "摘要",
          authors: [],
          categories: [],
          publishedAt: "2023-01-02T00:00:00Z",
          latestVersion: 3,
          contentHash: "h",
        },
        score: 10,
      },
    ],
    nextCursor: "",
  }));
  await act(async () => {
    root.unmount();
  });
  host.remove();
  host = document.createElement("div");
  document.body.append(host);
  root = createRoot(host);
  await act(async () => {
    root.render(
      <CollectionPage navOpen onToggleNav={() => {}} onOpenDocument={() => {}} />,
    );
  });
  const tasksTab = host.querySelector('[role="tab"]')?.parentElement
    ?.children[2] as HTMLElement;
  await act(async () => {
    tasksTab.click();
  });
  expect(host.textContent).toContain("运行中");
  const listTab = host.querySelector('[role="tab"]')?.parentElement
    ?.children[0] as HTMLElement;
  await act(async () => {
    listTab.click();
  });
  expect(host.textContent).toContain("确认入库（120 页）");
});

it("does not report a committing import as cancelled", async () => {
  api.cancelImport.mockImplementation(async () => ({ status: "importing" }));
  activeImports.mockImplementation(async () => [
    { requestId: "req-2", arxivId: "2301.12345", status: "downloading" },
  ]);
  candidates.mockImplementation(async () => ({
    items: [
      {
        candidate: {
          arxivId: "2301.12345",
          title: "论文",
          abstract: "摘要",
          authors: [],
          categories: [],
          publishedAt: "2023-01-02T00:00:00Z",
          latestVersion: 2,
          contentHash: "h",
        },
        score: 10,
      },
    ],
    nextCursor: "",
  }));
  await act(async () => {
    root.unmount();
  });
  host.remove();
  host = document.createElement("div");
  document.body.append(host);
  root = createRoot(host);
  await act(async () => {
    root.render(
      <CollectionPage navOpen onToggleNav={() => {}} onOpenDocument={() => {}} />,
    );
  });
  const cancel = Array.from(host.querySelectorAll("button")).find((b) =>
    b.textContent === "取消",
  ) as HTMLElement;
  await act(async () => {
    cancel.click();
  });
  expect(api.cancelImport).toHaveBeenCalledOnce();
  expect(host.textContent).toContain("入库中");
  expect(host.textContent).not.toContain("已取消");
});

it("offers retry for failed imports and blocks opening a trashed document", async () => {
  activeImports.mockImplementation(async () => [
    { requestId: "req-3", arxivId: "2301.12345", status: "failed", error: "下载失败", downloadVersion: 2 },
    { requestId: "req-4", arxivId: "2301.00002", status: "imported", documentId: "d1" },
  ]);
  candidates.mockImplementation(async () => ({
    items: [
      {
        candidate: {
          arxivId: "2301.12345",
          title: "论文甲",
          abstract: "摘要",
          authors: [],
          categories: [],
          publishedAt: "2023-01-02T00:00:00Z",
          latestVersion: 2,
          contentHash: "h1",
        },
        score: 10,
      },
      {
        candidate: {
          arxivId: "2301.00002",
          title: "论文乙",
          abstract: "摘要",
          authors: [],
          categories: [],
          publishedAt: "2023-01-03T00:00:00Z",
          latestVersion: 1,
          contentHash: "h2",
        },
        score: 8,
      },
    ],
    nextCursor: "",
  }));
  await act(async () => {
    root.unmount();
  });
  host.remove();
  host = document.createElement("div");
  document.body.append(host);
  root = createRoot(host);
  await act(async () => {
    root.render(
      <CollectionPage navOpen onToggleNav={() => {}} onOpenDocument={() => {}} />,
    );
  });
  const buttons = Array.from(host.querySelectorAll("button")).map((b) => b.textContent);
  expect(buttons).toContain("重试");
  // imported without a live document state still offers open here (state
  // unknown); once the detail reports trash, the open button disappears.
  api.candidate.mockImplementation(async () => ({
    candidate: { arxivId: "2301.00002", title: "论文乙", authors: [], categories: [] },
    citation: {},
    analyses: [],
    analysisJob: null,
    latestImport: {
      requestId: "req-4",
      arxivId: "2301.00002",
      status: "imported",
      documentId: "d1",
      documentState: "trash",
    },
  }));
  await act(async () => {
    (Array.from(host.querySelectorAll("button.min-w-0"))[1] as HTMLElement).click();
  });
  expect(host.textContent).toContain("文档在回收站");
  expect(
    Array.from(host.querySelectorAll("button")).some((b) => b.textContent?.includes("打开")),
  ).toBe(false);
});
