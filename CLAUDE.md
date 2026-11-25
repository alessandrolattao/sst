## Landing a change on this fork

This repository is `alessandrolattao/sst`, a fork of sst-community. Its default branch, `alessandrolattao`, is upstream plus a stack of patches, one or more commits per change, and `fork-sync-upstream.yml` checks every day for a new upstream release and rebases the stack onto it. When your pull request is ready to land, do these three things, in this order.

1. **Land it by rebase, never with a merge commit**: `merge.sh <pr> --rebase`. Why: a merge commit breaks the stack and the daily rebase. `git rebase` drops a merge commit, so the sync counts fewer patches after the rebase than before (`git rev-list --count dev..HEAD`) and stops. Catching up works the same way: a branch that falls behind `alessandrolattao` is rebased onto it and pushed with `--force-with-lease`, never merged with it.
2. **Release**: `gh workflow run fork-publish.yml --ref alessandrolattao`. An empty `version` bumps the pre-release counter, `4.x.y-alessandrolattao.N`. Follow the run until it ends (`gh run watch <run-id> --exit-status`), then write the published version on the issue: the run's log says `publishing <version>`, and `npm view @alessandrolattao/sst version` shows it. Why: nothing else publishes a change, `fork-publish.yml` never runs on a push.
3. **Push a `feat/<slug>` ref** on the change's last commit on `alessandrolattao`: `git push origin <sha>:refs/heads/feat/<slug>`, with a short kebab-case `<slug>` like the other `feat/*` refs. Take `<sha>` from `alessandrolattao` after the landing, not from your pull request's branch: the rebase gives the commits new hashes. Why: every change of the stack has one, the daily sync carries them along with `git rebase --update-refs`, and they are what a pull request to upstream would be cut from.

## Maintaining the fork

This is sst-community, a fork of SST. `MAINTAINING.md` covers merging an SST release, versions, releasing, updating Pulumi and the providers, the docs site, and who can merge. Read it before a merge from upstream, a release, a Pulumi or provider update, or a change under `.github/`. `CONTRIBUTING.md` is the guide for sending a change.

## Layout

- `platform/` — TS Pulumi components embedded via `//go:embed` (`platform/platform.go:16`)
- `examples/` — sample apps
- `cmd/sst/` — Go CLI entry, orchestrates everything
- `cmd/sst/mosaic/` — live dev TUI
- `pkg/server/` — JSON-RPC bridge for custom Pulumi dynamic providers (`rpc/rpc.ts` ↔ `pkg/server`)
- `pkg/bus/` — pub/sub event bus
- `sdk/js/` — runtime SDK for reading linked resources
- `packages/svelte-kit-sst/` — SvelteKit adapter for AWS Lambda (SvelteKit 2 and 3). Not a Bun workspace: it has its own `package-lock.json` and uses npm. Run `npm test` in it.
- `packages/astro-sst/` — Astro adapter for AWS Lambda (Astro 5, 6 and 7), the same way: npm, its own lockfile, `npm test` in it.
- `www/` — docs site (auto-generated from JSDoc comments in platform and extracted from the Go CLI)

## Commands

- **Setup**: `bun run setup`
- **Build platform**: `bun run build:platform`
- **Build CLI**: `bun run build:cli`
- **Run CLI locally**: `cd examples/<app> && go run ../../cmd/sst <command>`
- **Go tests**: `bun run test:cli`
- **Typecheck**: `bun run typecheck`
- **Generate docs**: `bun run docs:generate`
- **Run docs locally**: `bun run docs:dev`

## Verification

1. Build the platform
2. `cd examples/<app> && bun install`
3. `go run ../../cmd/sst deploy`
4. Verify with `curl` or AWS CLI
5. Don't clean up unless told to
6. `sst dev --mode=basic` for dev mode
