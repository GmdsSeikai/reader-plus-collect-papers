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
    candidate: vi.fn(
      async (
        _id: string,
      ): Promise<Record<string, unknown>> => ({
        candidate: {},
        citation: {},
      }),
    ),
    setIgnored: vi.fn(async () => ({ candidate: {}, citation: {} })),
    importItems: vi.fn(
      async (): Promise<{
        items: { requestId: string; arxivId: string; error?: string }[];
      }> => ({ items: [] }),
    ),
    importRequest: vi.fn(
      async (_id: string): Promise<Record<string, unknown>> => ({
        status: "queued",
      }),
    ),
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
  // Base UI checkbox clicks construct a PointerEvent; jsdom has none.
  if (typeof globalThis.PointerEvent === "undefined") {
    vi.stubGlobal("PointerEvent", MouseEvent);
  }
  vi.clearAllMocks();
  // clearAllMocks keeps mockImplementation; restore defaults so one test's
  // fixtures cannot occupy another test's import slot.
  candidates.mockImplementation(async () => ({ items: [], nextCursor: "" }));
  tasks.mockImplementation(async () => []);
  activeImports.mockImplementation(async () => []);
  api.candidate.mockImplementation(async () => ({ candidate: {}, citation: {} }));
  api.importItems.mockImplementation(async () => ({ items: [] }));
  api.importRequest.mockImplementation(async () => ({ status: "queued" }));
  api.confirmImport.mockImplementation(async () => ({}));
  api.cancelImport.mockImplementation(async () => ({ status: "cancelled" }));
  api.run.mockImplementation(async () => ({
    id: "r1",
    taskId: "t1",
    status: "running",
  }));
  api.cancelRun.mockImplementation(async () => ({ status: "cancelled" }));
  api.runTask.mockImplementation(async () => ({
    id: "r1",
    taskId: "t1",
    status: "running",
  }));
  api.setIgnored.mockImplementation(async () => ({
    candidate: {},
    citation: {},
  }));
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
    {
      id: "t1",
      name: "视觉",
      enabled: true,
      config: {},
      activeRun: { id: "r9", taskId: "t1", status: "running" },
    },
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
  const cancel = Array.from(host.querySelectorAll("button")).find(
    (b) => b.textContent === "取消",
  ) as HTMLElement;
  await act(async () => {
    cancel.click();
  });
  expect(api.cancelImport).toHaveBeenCalledOnce();
  expect(host.textContent).toContain("入库中");
  expect(host.textContent).not.toContain("已取消");
});

// terminalRows renders two candidate rows. Terminal import states are restored
// through the detail contract: activeImports only carries non-terminal
// requests and must not be faked with failed/imported returns.
const terminalRows = {
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
};

it("offers retry for failed imports and blocks opening a trashed document", async () => {
  candidates.mockImplementation(async () => terminalRows);
  api.candidate.mockImplementation(async (id: string) =>
    id === "2301.12345"
      ? {
          candidate: {
            arxivId: id,
            title: "论文甲",
            authors: [],
            categories: [],
          },
          citation: {},
          analyses: [],
          analysisJob: null,
          latestImport: {
            requestId: "req-3",
            arxivId: id,
            status: "failed",
            error: "下载失败",
            downloadVersion: 2,
          },
        }
      : {
          candidate: {
            arxivId: id,
            title: "论文乙",
            authors: [],
            categories: [],
          },
          citation: {},
          analyses: [],
          analysisJob: null,
          latestImport: {
            requestId: "req-4",
            arxivId: id,
            status: "imported",
            documentId: "d1",
            documentState: "trash",
          },
        },
  );
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
  const rows = Array.from(host.querySelectorAll("button.min-w-0"));
  await act(async () => {
    (rows[0] as HTMLElement).click();
  });
  expect(
    Array.from(host.querySelectorAll("button")).some(
      (b) => b.textContent === "重试",
    ),
  ).toBe(true);
  await act(async () => {
    (Array.from(host.querySelectorAll("button.min-w-0"))[1] as HTMLElement).click();
  });
  expect(host.textContent).toContain("文档在回收站");
  expect(
    Array.from(host.querySelectorAll("button")).some((b) =>
      b.textContent?.includes("打开"),
    ),
  ).toBe(false);
});

function deferred<T>() {
  let resolve!: (v: T) => void;
  const promise = new Promise<T>((r) => {
    resolve = r;
  });
  return { promise, resolve };
}

async function remount() {
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
}

it("keeps a retry claimed after a detail fetch when the old detail lands late", async () => {
  candidates.mockImplementation(async () => terminalRows);
  const slow = deferred<Record<string, unknown>>();
  let calls = 0;
  api.candidate.mockImplementation(async (id: string) => {
    calls++;
    if (calls === 1) {
      return {
        candidate: {
          arxivId: id,
          title: "论文甲",
          authors: [],
          categories: [],
        },
        citation: {},
        analyses: [],
        analysisJob: null,
        latestImport: {
          requestId: "req-old",
          arxivId: id,
          status: "failed",
          error: "下载失败",
          downloadVersion: 2,
        },
      };
    }
    return slow.promise;
  });
  api.importItems.mockImplementation(async () => ({
    items: [{ requestId: "req-new", arxivId: "2301.12345" }],
  }));
  await remount();
  const rows = Array.from(host.querySelectorAll("button.min-w-0"));
  // First detail restores the old failed request.
  await act(async () => {
    (rows[0] as HTMLElement).click();
  });
  expect(host.textContent).toContain("下载失败");
  // A second detail query is delayed; then the user retries and the new
  // request claims the slot.
  await act(async () => {
    (rows[0] as HTMLElement).click();
  });
  const retry = Array.from(host.querySelectorAll("button")).find(
    (b) => b.textContent === "重试",
  ) as HTMLElement;
  await act(async () => {
    retry.click();
  });
  expect(host.textContent).toContain("排队");
  // The old detail response must not roll the slot back to failed.
  slow.resolve({
    candidate: {
      arxivId: "2301.12345",
      title: "论文甲",
      authors: [],
      categories: [],
    },
    citation: {},
    analyses: [],
    analysisJob: null,
    latestImport: {
      requestId: "req-old",
      arxivId: "2301.12345",
      status: "failed",
      error: "下载失败",
      downloadVersion: 2,
    },
  });
  await act(async () => {
    await Promise.resolve();
  });
  expect(host.textContent).toContain("排队");
  expect(host.textContent).not.toContain("下载失败");
});

it("keeps a detail-restored import when the mount snapshot lands late and empty", async () => {
  const snapshot = deferred<Record<string, unknown>[]>();
  activeImports.mockImplementation(() => snapshot.promise);
  candidates.mockImplementation(async () => terminalRows);
  api.candidate.mockImplementation(async (id: string) => ({
    candidate: {
      arxivId: id,
      title: "论文乙",
      authors: [],
      categories: [],
    },
    citation: {},
    analyses: [],
    analysisJob: null,
    latestImport: {
      requestId: "req-4",
      arxivId: id,
      status: "imported",
      documentId: "d1",
      documentState: "active",
    },
  }));
  await remount();
  const rows = Array.from(host.querySelectorAll("button.min-w-0"));
  await act(async () => {
    (rows[1] as HTMLElement).click();
  });
  expect(
    Array.from(host.querySelectorAll("button")).some((b) =>
      b.textContent?.includes("打开"),
    ),
  ).toBe(true);
  // Terminal imports are absent from activeImports; the late empty snapshot
  // must not erase the restored state.
  snapshot.resolve([]);
  await act(async () => {
    await Promise.resolve();
  });
  expect(
    Array.from(host.querySelectorAll("button")).some((b) =>
      b.textContent?.includes("打开"),
    ),
  ).toBe(true);
});

it("ignores a stale poll response after a new import claims the slot", async () => {
  activeImports.mockImplementation(async () => [
    {
      requestId: "req-old",
      arxivId: "2301.12345",
      status: "downloading",
      downloadVersion: 2,
    },
  ]);
  candidates.mockImplementation(async () => terminalRows);
  // Only the superseded request answers with the stale failure; the new
  // request stays queued, mirroring the real per-request endpoint.
  api.importRequest.mockImplementation((id: string) =>
    Promise.resolve(
      id === "req-old"
        ? {
            requestId: id,
            arxivId: "2301.12345",
            status: "failed",
            error: "旧响应",
            downloadVersion: 2,
          }
        : { requestId: id, arxivId: "2301.12345", status: "queued" },
    ),
  );
  api.importItems.mockImplementation(async () => ({
    items: [{ requestId: "req-new", arxivId: "2301.12345" }],
  }));
  await remount();
  const select = host.querySelector('[role="checkbox"]') as HTMLElement | null;
  if (select) {
    await act(async () => {
      select.click();
    });
  }
  const download = Array.from(host.querySelectorAll("button")).find((b) =>
    b.textContent?.includes("下载并入库"),
  ) as HTMLElement;
  await act(async () => {
    download.click();
  });
  expect(host.textContent).toContain("排队");
  // Let the poll loop for the superseded request deliver its stale failure;
  // ownership keeps it out of the slot.
  await act(async () => {
    await new Promise((r) => setTimeout(r, 700));
  });
  expect(host.textContent).toContain("排队");
  expect(host.textContent).not.toContain("旧响应");
});

it("keeps a terminal import when the same request's stale snapshot lands late", async () => {
  // Mount GET imports captures the same request as downloaded and delays;
  // meanwhile the server commits and the detail restores imported/active.
  const snapshot = deferred<Record<string, unknown>[]>();
  activeImports.mockImplementation(() => snapshot.promise);
  candidates.mockImplementation(async () => terminalRows);
  api.candidate.mockImplementation(async (id: string) => ({
    candidate: { arxivId: id, title: "论文乙", authors: [], categories: [] },
    citation: {},
    analyses: [],
    analysisJob: null,
    latestImport: {
      requestId: "req-1",
      arxivId: id,
      status: "imported",
      documentId: "d1",
      documentState: "active",
      downloadVersion: 2,
    },
  }));
  await remount();
  const rows = Array.from(host.querySelectorAll("button.min-w-0"));
  await act(async () => {
    (rows[1] as HTMLElement).click();
  });
  expect(
    Array.from(host.querySelectorAll("button")).some((b) =>
      b.textContent?.includes("打开"),
    ),
  ).toBe(true);
  // The delayed snapshot carries the same requestId but an older status;
  // it must not roll the slot back to downloaded.
  snapshot.resolve([
    {
      requestId: "req-1",
      arxivId: "2301.00002",
      status: "downloaded",
      downloadVersion: 2,
    },
  ]);
  await act(async () => {
    await Promise.resolve();
  });
  expect(host.textContent).not.toContain("已下载");
  expect(
    Array.from(host.querySelectorAll("button")).some((b) =>
      b.textContent?.includes("打开"),
    ),
  ).toBe(true);
});
