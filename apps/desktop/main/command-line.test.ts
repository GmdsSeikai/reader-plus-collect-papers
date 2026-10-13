import { mkdtemp, readlink, rm, writeFile } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { afterEach, describe, expect, it } from "vitest";
import {
  commandLineStatus,
  isReaderLink,
  macInstall,
  macStatus,
  macUninstall,
  pathWith,
  pathWithout,
  setCommandLine,
} from "./command-line";

const app = "/Applications/Reader.app/Contents/Resources/bin/reader";
let directory = "";
afterEach(async () => {
  if (directory) await rm(directory, { recursive: true, force: true });
  directory = "";
});
async function link() {
  directory = await mkdtemp(join(tmpdir(), "reader-cli-"));
  return join(directory, "bin", "reader");
}

describe.skipIf(process.platform === "win32")("macOS link", () => {
  it("installs, recognises and removes its own link", async () => {
    const path = await link();
    expect(await macStatus(path, app)).toEqual({
      supported: true,
      installed: false,
      location: path,
    });
    expect((await macInstall(path, app)).installed).toBe(true);
    expect(await readlink(path)).toBe(app);
    expect((await macInstall(path, app)).installed).toBe(true);
    expect((await macUninstall(path, app)).installed).toBe(false);
  });
  it("replaces a link left by a moved Reader.app", async () => {
    const path = await link();
    await macInstall(
      path,
      "/Users/a/Downloads/Reader.app/Contents/Resources/bin/reader",
    );
    const status = await macStatus(path, app);
    expect(status).toMatchObject({ installed: false });
    expect(status.conflict).toBeUndefined();
    await macInstall(path, app);
    expect(await readlink(path)).toBe(app);
  });
  it("leaves another program's reader alone", async () => {
    const path = await link();
    await macInstall(path, "/opt/other/reader");
    expect((await macStatus(path, app)).conflict).toBe(true);
    await expect(macInstall(path, app)).rejects.toThrow("已被其他程序占用");
    await expect(macUninstall(path, app)).rejects.toThrow("已被其他程序占用");
    expect(await readlink(path)).toBe("/opt/other/reader");

    await rm(path);
    await writeFile(path, "#!/bin/sh\n");
    expect((await macStatus(path, app)).conflict).toBe(true);
  });
  it("recognises Reader links by their bundle path", () => {
    expect(isReaderLink(app)).toBe(true);
    expect(
      isReaderLink("/Volumes/x/Reader 2.app/Contents/Resources/bin/reader"),
    ).toBe(true);
    expect(isReaderLink("/usr/local/Cellar/reader/1/bin/reader")).toBe(false);
  });
});

describe("Windows user PATH", () => {
  const bin = "C:\\Users\\a\\AppData\\Local\\Programs\\Reader\\resources\\bin";
  it("adds the directory once and keeps other entries verbatim", () => {
    const raw = "%USERPROFILE%\\.local\\bin;C:\\Tools;";
    const added = pathWith(raw, bin);
    expect(added).toBe(`%USERPROFILE%\\.local\\bin;C:\\Tools;${bin}`);
    expect(pathWith(added, bin.toUpperCase() + "\\")).toBe(added);
    expect(pathWith("", bin)).toBe(bin);
  });
  it("removes every spelling of the directory", () => {
    expect(pathWithout(`C:\\Tools;"${bin}";${bin}\\;D:\\x`, bin)).toBe(
      "C:\\Tools;D:\\x",
    );
  });
});

describe("development builds", () => {
  it("do not install the worktree binary", async () => {
    const host = {
      platform: process.platform,
      packaged: false,
      binary: "/repo/apps/desktop/bin/reader",
      env: {},
    };
    expect(await commandLineStatus(host)).toEqual({
      supported: false,
      installed: false,
      location: host.binary,
    });
    await expect(setCommandLine(host, true)).rejects.toThrow("开发版");
  });
  it("and unsupported platforms report no installer", async () => {
    const host = {
      platform: "linux" as const,
      packaged: true,
      binary: "/opt/Reader/resources/bin/reader",
      env: {},
    };
    expect((await commandLineStatus(host)).supported).toBe(false);
  });
});
