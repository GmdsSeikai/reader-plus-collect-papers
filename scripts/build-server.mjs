import { readFileSync } from "node:fs";
import { mkdir } from "node:fs/promises";
import { spawnSync } from "node:child_process";
import { fileURLToPath } from "node:url";

const readerVersion = JSON.parse(
  readFileSync(new URL("../package.json", import.meta.url), "utf8"),
).version;
const commitResult = spawnSync("git", ["rev-parse", "HEAD"], {
  cwd: new URL("../", import.meta.url),
  encoding: "utf8",
});
const buildCommit = /^[a-f0-9]{40}$/.test(commitResult.stdout?.trim() || "")
  ? commitResult.stdout.trim()
  : "unknown";
const platform =
  process.env.GOOS ||
  (process.platform === "win32" ? "windows" : process.platform);
const output = new URL(
  `../apps/desktop/bin/reader-server${platform === "windows" ? ".exe" : ""}`,
  import.meta.url,
);
await mkdir(new URL("../apps/desktop/bin/", import.meta.url), {
  recursive: true,
});
const result = spawnSync(
  "go",
  [
    "build",
    "-trimpath",
    // Omit the symbol table and DWARF; panics keep function names and lines.
    `-ldflags=-s -w -X main.readerVersion=${readerVersion} -X main.buildCommit=${buildCommit}`,
    "-o",
    fileURLToPath(output),
    "./cmd/reader-server",
  ],
  {
    cwd: new URL("../apps/server/", import.meta.url),
    stdio: "inherit",
  },
);
if (result.error) throw result.error;
if (result.status !== 0) process.exit(result.status ?? 1);
