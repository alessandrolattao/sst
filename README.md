<div align="center">
<pre>
  ░██████     ░██████   ░██████████
 ░██   ░██   ░██   ░██      ░██    
░██         ░██             ░██    
 ░████████   ░████████      ░██    
        ░██         ░██     ░██    
 ░██   ░██   ░██   ░██      ░██    
  ░██████     ░██████       ░██    
</pre>
</div>

<p align="center">
  <b>Alessandro Lattao version</b><br />
  <sub>Unofficial fork of <a href="https://github.com/sst/sst">SST</a> &middot; parallel Go builds, working dev rebuilds, deploys that skip what did not change</sub>
</p>

<p align="center">
  <a href="https://www.npmjs.com/package/@alessandrolattao/sst"><img alt="npm" src="https://img.shields.io/npm/v/@alessandrolattao/sst.svg?style=flat-square" /></a>
  <a href="https://github.com/alessandrolattao/sst"><img alt="fork of sst/sst" src="https://img.shields.io/badge/fork%20of-sst%2Fsst-blue?style=flat-square" /></a>
  <a href="./LICENSE"><img alt="MIT" src="https://img.shields.io/badge/license-MIT-green?style=flat-square" /></a>
</p>

---

A personal fork of [SST](https://github.com/sst/sst) that makes Go projects build faster and behave properly in dev mode.

Everything SST does, it still does. The changes live entirely in the Go runtime and in the dev rebuild loop, and they matter most on a monorepo with many handlers: this fork was built against one with **438 Go Lambda functions**.

## What is different

- **Go builds run in parallel.** The Go runtime held an exclusive lock around the whole build, so it compiled one function at a time no matter how many cores were available. The lock now guards only its own bookkeeping.

- **`SST_BUILD_CONCURRENCY_FUNCTION` works for Go.** The documented knob was implemented for Node and Python only, and silently ignored for Go handlers. It now caps Go builds too, at 4 by default.

- **Rebuilds follow real imports.** In dev, SST decided what to rebuild from the directory a changed file sits in. It now uses the handler's actual import graph, so editing a shared package rebuilds every function that imports it, and nothing else.

- **Invalidated handlers rebuild together.** Once rebuild scope follows imports, one save can invalidate hundreds of handlers. They used to be recompiled one after another; now they are compiled concurrently, under a limit.

- **A deploy stops recompiling handlers one at a time.** Every deploy drops the dev build cache, and each worker then restarts and compiles whatever it no longer finds there, one after another on the event loop. On a stage with 18 Go handlers in dev that was 50 seconds of wall clock, for source that had not changed. The same set now goes through the concurrent rebuild above, and handlers whose runtime starts lazily are left to compile when something invokes them.

- **Editing a shared package no longer redeploys every function.** Go stamps each binary with a hash of every source file that fed the build, including the ones the linker then discards as unreachable. Touch one file in a package that hundreds of handlers import and every artifact gets a new hash, so the deploy uploads all of them and updates every Function, for programs whose code is byte-identical to what is already deployed. Deploy builds now link with `-buildid=`, so two builds differ when the program differs and not before. What that gives up is `NT_GNU_BUILD_ID`, which `-s -w` had already made useless here.

- **Deploys stop paying for dev-only work.** The import graph is only ever read by the dev file watcher, but it was captured after every build, roughly doubling the cost of every deploy build. It is now captured only in dev.

- **Every Go build says how long it took.** A deploy printed nothing between asking for a build and the Function resource turning up minutes later, so the compile time of a single handler was only visible by reading the debug log. Each finished build now prints a `Built` line with its handler and its seconds, in the same shape as the `Created`/`Updated` lines that already carry a duration. It rides on the one event SST's UI prints verbatim, so this stays a change to the Go runtime: no new event type, no case added to the printer, nothing for an upstream merge to land on.

- **A big deploy stops flooding the terminal.** The deploy footer lists every resource in flight and redraws in place every 100ms, by moving the cursor back up to its first line. With more resources in flight than the terminal has rows, the cursor stops at the top row, so every redraw left the whole list in the scrollback again: the same `Creating` line tens of thousands of times. The footer now shows only the lines that fit, with the status line still last. What it says and when is unchanged.

- **The state is half the size.** SST runs Pulumi against its local DIY backend, which rewrote the whole state as indented JSON after every resource it changed, while SST uploaded that same file to the state bucket during the deploy. Pulumi has written it compact since 3.238, and the fork is on Pulumi 3.270: on a test app with 40 components the state went from 164 KB to 82 KB. Pulumi's Node.js SDK now needs Node.js 22 or later.

- **Pulumi can journal its steps, as it does with Pulumi Cloud.** Compact or not, that DIY backend still rewrites the whole state file after every resource it creates, updates or deletes, one write at a time, and on a large stack that write is what a deploy waits on: with a 53 MB state (11,000 resources) every step costs about two seconds on a CI runner, however fast the cloud answered, and Pulumi's parallel workers all queue behind it. Pulumi Cloud is spared because the CLI sends it one small journal entry per step instead, over its public HTTP API. Set `SST_PULUMI_JOURNALING=true` and SST serves that API itself, on `127.0.0.1`, for each `deploy`, `refresh` and `remove`, every call as [Pulumi's published OpenAPI spec](https://api.pulumi.com/api/openapi/pulumi-spec.json) defines it. Pulumi journals, the entries are applied with Pulumi's own replayer, and the state file is rebuilt from them right before each push to the state bucket, so what lands there, and when, is what landed there before. Rebuilding that 53 MB state takes 0.7s, once per push rather than once per step; on a 1,000-resource stack, replacing 50 resources went from 45s to 11s. Nothing leaves the machine, there is no Pulumi Cloud account or token, and the secrets stay encrypted with SST's passphrase. `sst diff` keeps the DIY backend. The deploy output says what the backend does and how long each part took, and `sst dev` gives it a pane of its own, under SST:

  ```
  |  Journal     saved 11003 resources, 53.2 MB, after 22008 entries (build 0.05s, encode 0.62s, write 0.03s)
  |  Journal     succeeded: 22008 entries in 506 batches, applied in 1.15s; 1 save in 0.70s, slowest 0.70s
  ```

  It is opt-in while it proves itself on real deploys.

- **An interrupted deploy pushes its state at once.** On Ctrl+C or SIGTERM, SST interrupts Pulumi and waits for it to exit, which can take minutes while it finishes what is in flight, but it stopped pushing the state at the interrupt: whatever killed the process next, like a CI runner that gives it about ten seconds, left the bucket with the state from before. The interrupt now pushes the state right away, and pushes go on until Pulumi has exited.

- **Smaller fixes.** Resolving `GOMODCACHE` is cancellable instead of outliving a Ctrl-C, `go list` output is decoded through explicit JSON tags, and an unparsable concurrency value warns and keeps the default instead of quietly falling back to serial builds.

## What it costs

Measured on the 438-handler monorepo this was written for, on a no-op deploy where nothing changed:

| | before | after |
|---|---|---|
| build concurrency | 1 (peak 2) | up to the configured limit |
| per-handler build | ~1.4s, of which half was the dev-only graph capture | ~0.6s |

And on a deploy that did change something: one file edited in the shared handler framework, which 434 of those modules import. Before, all 434 were uploaded again and had their Function updated. After, only the handlers whose linked program actually changed, which for that edit was one.

## Installation

```bash
npm install @alessandrolattao/sst
# bun add @alessandrolattao/sst
```

Built for Linux (x64 and arm64) and macOS (Apple silicon and Intel). The binary is still called `sst`, so `bunx sst`, `npx sst` and every existing script keep working unchanged.

Versions are published as pre-releases (`4.17.1-alessandrolattao.1`), which means they must be pinned explicitly and will never be picked up by a `^4` range by accident. If your `sst.config.ts` declares a `version` constraint, it needs a pre-release floor to accept one, since SST checks it with Masterminds/semver where a plain `>=` never matches a pre-release:

```ts
version: ">= 4.13.1-0",
```

## Starting a new project

`sst init` writes an `sst.config.ts` for whatever it finds in the current
directory, so install the CLI first and let it generate the config:

```bash
mkdir my-app && cd my-app
npm init -y
npm install @alessandrolattao/sst@4.17.1-alessandrolattao.1
npx sst init
```

Then open the generated `sst.config.ts` and add the pre-release floor to its
`app()` return, otherwise the CLI refuses to run against its own config:

```ts
export default $config({
  app(input) {
    return {
      name: "my-app",
      version: ">= 4.13.1-0",
      home: "aws",
    };
  },
  async run() {},
});
```

From here everything is upstream SST: `npx sst deploy --stage dev`,
`npx sst dev`, and the [getting started guides](https://sst.dev/docs/start/aws/api)
apply unchanged.

## Relationship with upstream

This fork tracks [`sst/sst`](https://github.com/sst/sst) and is kept in sync with it automatically: a daily job rebases the patches onto each new upstream release, verifies the result builds and passes, and publishes a matching build. When a rebase conflicts it stops and opens an issue instead of publishing something nobody read. Branches:

- `dev` — a plain mirror of upstream, never committed to directly
- `feat/…` — one branch per change, rebased on `dev`, each self-contained enough to be sent upstream
- `alessandrolattao` — what actually gets built and published: upstream plus the patches plus the fork's own naming

The published package is renamed so it can sit next to the real one without claiming its name. Everything else, including the `SST_BIN_PATH` escape hatch, behaves exactly as upstream.

## Upstream documentation

The fork changes no behaviour you interact with, so upstream's docs apply as they are:

- [Docs](https://sst.dev/docs/)
- [CLI reference](https://sst.dev/docs/reference/cli/)
- [Components](https://sst.dev/docs/components/)

## Running locally

Run `bun run setup`. You need [Go](https://go.dev/) and [Bun](https://bun.sh/) installed.

```bash
cd examples/aws-api
go run ../../cmd/sst <command>
```

To try a build of this fork against a real project without publishing anything, point `SST_BIN_PATH` at it:

```bash
go build -o /tmp/sst ./cmd/sst
SST_BIN_PATH=/tmp/sst bunx sst deploy --stage dev
```

## License

MIT, same as upstream. See [LICENSE](./LICENSE), which keeps the original SST copyright.
