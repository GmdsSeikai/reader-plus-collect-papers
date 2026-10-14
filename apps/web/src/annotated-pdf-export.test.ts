import { beforeEach, expect, it, vi } from "vitest";
import type { Annotation, Document } from "@reader/core";

const mocks = vi.hoisted(() => ({
  annotations: vi.fn(),
  messages: vi.fn(),
  annotatedPDF: vi.fn(),
  downloadBlob: vi.fn(),
  toast: { loading: vi.fn(() => "t"), success: vi.fn(), error: vi.fn() },
}));
vi.mock("@reader/api", () => ({
  api: { annotations: mocks.annotations, messages: mocks.messages },
  publicationURL: (id: string, name: string) => `/pub/x/${id}/${name}`,
}));
vi.mock("./readers/pdf-export", () => ({ annotatedPDF: mocks.annotatedPDF }));
vi.mock("./download", async (original) => ({
  ...(await original<typeof import("./download")>()),
  downloadBlob: mocks.downloadBlob,
}));
vi.mock("sonner", () => ({ toast: mocks.toast }));
vi.mock("pdfjs-dist", () => ({
  GlobalWorkerOptions: { workerSrc: "" },
  getDocument: vi.fn(),
}));

import {
  exportAnnotatedPDF,
  exportAnnotatedPDFWithToast,
} from "./annotated-pdf-export";

const doc = { id: "d1", title: 'A/B: "C"', type: "pdf" } as Document;
const mark = (id: string, value: Partial<Annotation>) =>
  ({
    id,
    kind: "highlight",
    location: { type: "pdf", page: 1 },
    ...value,
  }) as Annotation;

beforeEach(() => {
  vi.clearAllMocks();
  mocks.messages.mockResolvedValue([{ id: "m1", content: "回答" }]);
  mocks.annotatedPDF.mockResolvedValue({
    bytes: new Uint8Array([37, 80, 68, 70]),
    exported: 2,
    skipped: 1,
  });
});

it("writes annotations and answers into a downloaded copy", async () => {
  mocks.annotations.mockResolvedValue([
    mark("h", {}),
    mark("q", { kind: "question", answerId: "m1" }),
    mark("b", { kind: "bookmark" }),
  ]);
  await expect(exportAnnotatedPDF(doc)).resolves.toMatchObject({
    exported: 2,
    skipped: 1,
  });
  const [, source, marks, answers] = mocks.annotatedPDF.mock.calls[0];
  expect(source.url).toBe("/pub/x/d1/original.pdf");
  expect(marks.map((a: Annotation) => a.id)).toEqual(["h", "q"]);
  expect(answers).toEqual({ m1: "回答" });
  const [blob, name] = mocks.downloadBlob.mock.calls[0];
  expect(blob.type).toBe("application/pdf");
  expect(name).toBe("A-B- -C--批注.pdf");
});

it("reports a PDF without annotations instead of saving a copy", async () => {
  mocks.annotations.mockResolvedValue([mark("b", { kind: "bookmark" })]);
  await expect(exportAnnotatedPDF(doc)).rejects.toThrow("还没有可导出的批注");
  expect(mocks.annotatedPDF).not.toHaveBeenCalled();
  expect(mocks.downloadBlob).not.toHaveBeenCalled();
});

it("does not save when nothing could be placed", async () => {
  mocks.annotations.mockResolvedValue([mark("h", {})]);
  mocks.annotatedPDF.mockResolvedValue({
    bytes: new Uint8Array(),
    exported: 0,
    skipped: 1,
  });
  await expect(exportAnnotatedPDF(doc)).rejects.toThrow("无法在原文中定位");
  expect(mocks.downloadBlob).not.toHaveBeenCalled();
});

it("shows progress, the count and skipped marks in one toast", async () => {
  mocks.annotations.mockResolvedValue([mark("h", {})]);
  await exportAnnotatedPDFWithToast(doc);
  expect(mocks.toast.loading).toHaveBeenCalled();
  expect(mocks.toast.success).toHaveBeenCalledWith("已导出 2 条批注", {
    id: "t",
    description: "1 条无法在原文中定位，未写入",
  });

  mocks.annotations.mockRejectedValue(new Error("网络错误"));
  await exportAnnotatedPDFWithToast(doc);
  expect(mocks.toast.error).toHaveBeenCalledWith("网络错误", { id: "t" });
});
