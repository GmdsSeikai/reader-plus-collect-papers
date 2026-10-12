// @vitest-environment jsdom
import { act } from "react";
import { createRoot, type Root } from "react-dom/client";
import { afterEach, beforeEach, expect, it, vi } from "vitest";
import { CollectionPage } from "./CollectionPage";

const candidates = vi.fn(async (_params: Record<string, string>) => ({
  items: [],
  nextCursor: "",
}));
const tasks = vi.fn(async () => []);
const activeImports = vi.fn(async () => []);
const createTask = vi.fn(async () => ({}));

vi.mock("@reader/api", () => ({
  collectionApi: {
    candidates,
    tasks,
    activeImports,
    createTask,
    updateTask: vi.fn(async () => ({})),
    runTask: vi.fn(async () => ({})),
    run: vi.fn(async () => ({})),
    cancelRun: vi.fn(async () => ({})),
    candidate: vi.fn(async () => ({ candidate: {}, citation: {} })),
    setIgnored: vi.fn(async () => ({ candidate: {}, citation: {} })),
    importItems: vi.fn(async () => ({ items: [] })),
    importRequest: vi.fn(async () => ({ status: "queued" })),
    confirmImport: vi.fn(async () => ({})),
    cancelImport: vi.fn(async () => ({})),
    settings: vi.fn(async () => ({})),
    saveSettings: vi.fn(async () => ({})),
    pause: vi.fn(async () => ({})),
    resume: vi.fn(async () => ({})),
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
