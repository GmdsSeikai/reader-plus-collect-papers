import { afterEach, expect, it, vi } from "vitest";
import { mkdtemp, readFile, readdir, rm, writeFile } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { resolveModel } from "./model";

vi.mock("../model.json", async () => {
  const { createHash } = await import("node:crypto");
  return {
    default: {
      name: "fixture",
      revision: "pinned",
      bytes: 7,
      sha256: createHash("sha256").update("current").digest("hex"),
    },
  };
});

const roots: string[] = [];
async function temporary() {
  const root = await mkdtemp(join(tmpdir(), "reader-model-"));
  roots.push(root);
  return root;
}
afterEach(async () => {
  vi.unstubAllGlobals();
  await Promise.all(
    roots.splice(0).map((root) => rm(root, { recursive: true, force: true })),
  );
});

it("uses a verified local model without downloading or writing to the library", async () => {
  const root = await temporary();
  const path = join(root, "layout.onnx");
  await writeFile(path, "current");
  const fetch = vi.fn();
  vi.stubGlobal("fetch", fetch);
  await expect(resolveModel(path)).resolves.toBe(path);
  expect(fetch).not.toHaveBeenCalled();
  expect(await readdir(root)).toEqual(["layout.onnx"]);
});

it.each(["truncated", "changed"])(
  "rejects %s model contents without replacing the file or downloading",
  async (contents) => {
    const root = await temporary();
    const path = join(root, "layout.onnx");
    await writeFile(path, contents);
    const fetch = vi.fn();
    vi.stubGlobal("fetch", fetch);
    await expect(resolveModel(path)).rejects.toThrow("版面模型校验失败");
    expect(fetch).not.toHaveBeenCalled();
    expect(await readFile(path, "utf8")).toBe(contents);
  },
);

it("fails locally when the model is missing", async () => {
  const root = await temporary();
  const fetch = vi.fn();
  vi.stubGlobal("fetch", fetch);
  await expect(resolveModel(join(root, "missing.onnx"))).rejects.toThrow(
    "未找到内置版面模型",
  );
  expect(fetch).not.toHaveBeenCalled();
  expect(await readdir(root)).toEqual([]);
});
