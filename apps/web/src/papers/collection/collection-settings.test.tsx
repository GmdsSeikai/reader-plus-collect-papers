// @vitest-environment jsdom
import { act } from "react";
import { createRoot, type Root } from "react-dom/client";
import { afterEach, beforeEach, expect, it, vi } from "vitest";
import { CollectionSettingsDialog } from "./CollectionSettingsDialog";

const { settings, saveSettings, aiConfig, saveAIConfig, agentModels } =
  vi.hoisted(() => ({
    settings: vi.fn(),
    saveSettings: vi.fn(),
    aiConfig: vi.fn(),
    saveAIConfig: vi.fn(),
    agentModels: vi.fn(),
  }));
vi.mock("@reader/api", () => ({
  collectionApi: { settings, saveSettings },
  api: {
    aiConfig,
    saveAIConfig,
    agentModels,
    settings: vi.fn(async () => ({ autoTranslatePDF: false })),
  },
}));
const cfg = {
  analysisEnabled: false,
  dailyLimit: 100,
  requestRpm: 0,
  paused: false,
  configVersion: 1,
  hasCitationsKey: true,
  quota: { utcDay: "2026-10-13", sent: 5, reserved: 1, uncertain: 1 },
};
const ai = {
  primary: "codex",
  models: {},
  api: { url: "", model: "", hasKey: false },
  textAPI: { url: "", model: "", hasKey: false },
  imageAPI: { url: "", model: "", hasKey: false },
  capabilities: {},
  hasCitationsKey: true,
};
let root: Root, host: HTMLDivElement;
const onSaved = vi.fn();
const button = (label: string) =>
  Array.from(document.querySelectorAll("button")).find(
    (item) => item.textContent === label,
  )!;

beforeEach(async () => {
  vi.stubGlobal("IS_REACT_ACT_ENVIRONMENT", true);
  vi.stubGlobal(
    "PointerEvent",
    class extends MouseEvent {
      pointerType = "mouse";
      pointerId = 1;
      isPrimary = true;
    },
  );
  vi.resetAllMocks();
  settings.mockResolvedValue(cfg);
  aiConfig.mockResolvedValue(ai);
  agentModels.mockResolvedValue([]);
  saveSettings.mockImplementation(async (body) => body);
  saveAIConfig.mockResolvedValue(ai);
  host = document.createElement("div");
  document.body.append(host);
  root = createRoot(host);
  await act(async () =>
    root.render(
      <CollectionSettingsDialog onClose={() => {}} onSaved={onSaved} />,
    ),
  );
});
afterEach(async () => {
  await act(async () => root.unmount());
  host.remove();
  document.body.innerHTML = "";
  vi.unstubAllGlobals();
});

it("requires confirmation before enabling paid analysis and saving settings", async () => {
  await act(async () =>
    (document.querySelector('[role="checkbox"]') as HTMLElement).click(),
  );
  expect(document.querySelector('[role="alertdialog"]')?.textContent).toContain(
    "启用摘要分析？",
  );
  expect(saveSettings).not.toHaveBeenCalled();
  await act(async () => button("启用").click());
  await act(async () => button("保存").click());
  expect(saveSettings).toHaveBeenCalledWith(
    expect.objectContaining({ analysisEnabled: true }),
  );
  expect(onSaved).toHaveBeenCalledOnce();
});

it("uses the latest protected config when explicitly clearing the citation key", async () => {
  aiConfig.mockResolvedValue({ ...ai, primary: "claude" });
  await act(async () => button("清除 Key").click());
  expect(saveAIConfig).toHaveBeenCalledWith(
    expect.objectContaining({
      primary: "claude",
      citationsApiKey: "",
      clearCitationsKey: true,
    }),
  );
  expect(saveSettings).not.toHaveBeenCalled();
});

it("shows actual quota and the independent full-document translation state", () => {
  expect(document.body.textContent).toContain("已发 5");
  expect(document.body.textContent).toContain("预占 1");
  expect(document.body.textContent).toContain("发送不确定 1");
  expect(document.body.textContent).toContain("入库后全文翻译：已关闭");
});

it("keeps the dialog open and reports a rejected settings update", async () => {
  saveSettings.mockRejectedValue(new Error("设置保存失败"));
  await act(async () => button("保存").click());
  expect(document.querySelector('[role="alert"]')?.textContent).toBe(
    "设置保存失败",
  );
  expect(onSaved).not.toHaveBeenCalled();
});

it("aborts model metadata loading when the dialog is unmounted", async () => {
  const signal = agentModels.mock.calls[0]?.[1] as AbortSignal;
  expect(signal.aborted).toBe(false);
  await act(async () => root.render(<div />));
  expect(signal.aborted).toBe(true);
});
