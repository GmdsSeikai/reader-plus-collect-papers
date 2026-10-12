import { afterEach, expect, it, vi } from "vitest";
import { createHash } from "node:crypto";
import { mkdtemp, readFile, readdir, rm, writeFile } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { model } from "./model";
import { prepareModel } from "./prepare-model";

const contents = "verified model";
const specification = {
  ...model,
  bytes: Buffer.byteLength(contents),
  sha256: createHash("sha256").update(contents).digest("hex"),
};
const filename = `${specification.name}-${specification.revision}.onnx`;
const roots: string[] = [];
async function temporary() {
  const root = await mkdtemp(join(tmpdir(), "reader-model-build-"));
  roots.push(root);
  return root;
}
afterEach(async () => {
  vi.unstubAllGlobals();
  await Promise.all(
    roots.splice(0).map((root) => rm(root, { recursive: true, force: true })),
  );
});

it("downloads the pinned revision and publishes only a verified model", async () => {
  const root = await temporary();
  const fetch = vi.fn(async () => new Response(contents));
  vi.stubGlobal("fetch", fetch);
  const path = await prepareModel(root, undefined, specification);
  expect(path).toBe(join(root, filename));
  expect(await readFile(path, "utf8")).toBe(contents);
  expect(await readdir(root)).toEqual([filename]);
  expect(fetch).toHaveBeenCalledWith(
    `https://huggingface.co/${model.repository}/resolve/${model.revision}/${model.file}`,
    expect.objectContaining({ signal: expect.any(AbortSignal) }),
  );
});

it("reuses a verified build cache without network access", async () => {
  const root = await temporary();
  const path = join(root, filename);
  await writeFile(path, contents);
  const fetch = vi.fn();
  vi.stubGlobal("fetch", fetch);
  await expect(prepareModel(root, undefined, specification)).resolves.toBe(
    path,
  );
  expect(fetch).not.toHaveBeenCalled();
});

it("preserves a corrupt cache and requires it to be repaired explicitly", async () => {
  const root = await temporary();
  const path = join(root, filename);
  await writeFile(path, "corrupt");
  const fetch = vi.fn();
  vi.stubGlobal("fetch", fetch);
  await expect(prepareModel(root, undefined, specification)).rejects.toThrow(
    "版面模型校验失败",
  );
  expect(fetch).not.toHaveBeenCalled();
  expect(await readFile(path, "utf8")).toBe("corrupt");
});

it("accepts an offline build source only after checking its integrity", async () => {
  const root = await temporary();
  const source = join(await temporary(), "source.onnx");
  await writeFile(source, contents);
  const fetch = vi.fn();
  vi.stubGlobal("fetch", fetch);
  const path = await prepareModel(root, source, specification);
  expect(await readFile(path, "utf8")).toBe(contents);
  expect(await readFile(source, "utf8")).toBe(contents);
  expect(await readdir(root)).toEqual([filename]);
  expect(fetch).not.toHaveBeenCalled();
});

it("does not download when an offline source is invalid", async () => {
  const root = await temporary();
  const source = join(await temporary(), "source.onnx");
  await writeFile(source, "corrupt");
  const fetch = vi.fn();
  vi.stubGlobal("fetch", fetch);
  await expect(prepareModel(root, source, specification)).rejects.toThrow(
    "版面模型校验失败",
  );
  expect(fetch).not.toHaveBeenCalled();
  expect(await readdir(root)).toEqual([]);
});

it.each([
  ["truncated", "short", "版面模型校验失败"],
  ["oversized", contents + "extra", "模型文件过大"],
  ["wrong hash", "untrusted data", "版面模型校验失败"],
])(
  "rejects a %s download and removes partial files",
  async (_, data, error) => {
    const root = await temporary();
    vi.stubGlobal(
      "fetch",
      vi.fn(async () => new Response(data)),
    );
    await expect(prepareModel(root, undefined, specification)).rejects.toThrow(
      error,
    );
    expect(await readdir(root)).toEqual([]);
  },
);

it("cleans up after a stream fails partway through the download", async () => {
  const root = await temporary();
  let reads = 0;
  vi.stubGlobal(
    "fetch",
    vi.fn(
      async () =>
        new Response(
          new ReadableStream({
            pull(controller) {
              if (reads++ === 0)
                controller.enqueue(new TextEncoder().encode("half"));
              else controller.error(new Error("connection closed"));
            },
          }),
        ),
    ),
  );
  await expect(prepareModel(root, undefined, specification)).rejects.toThrow(
    "connection closed",
  );
  expect(await readdir(root)).toEqual([]);
});

it("fails the build on a download HTTP error", async () => {
  const root = await temporary();
  vi.stubGlobal(
    "fetch",
    vi.fn(async () => new Response(null, { status: 503 })),
  );
  await expect(prepareModel(root, undefined, specification)).rejects.toThrow(
    "HTTP 503",
  );
  expect(await readdir(root)).toEqual([]);
});
