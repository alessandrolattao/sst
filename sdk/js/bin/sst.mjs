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
      `This fork of SST only publishes Linux builds (x64 and arm64), and there is no "${name}" package to install.\n\n` +
        `To run it on another platform, build the CLI from https://github.com/alessandrolattao/sst and point SST_BIN_PATH at the binary:\n` +
        `  go build -o /tmp/sst ./cmd/sst && SST_BIN_PATH=/tmp/sst npx sst <command>\n\n` +
        `For everything except the Go build changes, upstream's "sst" package works the same and covers every platform.`,
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
