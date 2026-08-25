#!/usr/bin/env node
import { createRequire } from "node:module";
const require = createRequire(import.meta.url);
import path from "path";
import { execFileSync } from "child_process";

let resolved = process.env.SST_BIN_PATH;

if (!resolved) {
  const name = `@alessandrolattao/sst-${process.platform}-${process.arch}`;
  const binary = process.platform === "win32" ? "sst.exe" : "sst";

  try {
    resolved = require.resolve(path.join(name, "bin", binary));
  } catch (ex) {
    console.error(
      `No "${name}" package: this fork builds for Linux (x64, arm64) and macOS (arm64, x64).\n\n` +
        `On any other platform, build the CLI from https://github.com/alessandrolattao/sst and point SST_BIN_PATH at it:\n` +
        `  go build -o /tmp/sst ./cmd/sst && SST_BIN_PATH=/tmp/sst npx sst <command>\n\n` +
        `Upstream's "sst" package covers every platform and behaves the same, minus the Go build changes.`,
    );
    process.exit(1);
  }
}

process.on("SIGINT", () => {});

try {
  execFileSync(resolved, process.argv.slice(2), {
    stdio: "inherit",
  });
} catch (ex) {
  process.exit(1);
}
