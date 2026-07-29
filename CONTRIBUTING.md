# Contributing to glassdb

glassdb is an educational engine: clarity beats cleverness, and every change
should keep the internals *watchable*.

## Ground rules

- **Engine stays dependency-free.** The Go module uses the standard library
  only. The server speaks SSE instead of WebSockets for exactly this reason.
- **New internals emit events.** If you add behavior (a merge, a vacuum, a
  new access path), emit events for it and handle them in the X-ray reducer
  (`xray/src/state/reducer.ts`) plus its tests. Unknown events don't break
  the UI, but invisible internals defeat the point of the project.
- **Tests first-class.** Storage changes need crash tests (see
  `vfs.Fault` and `engine/pager/pager_test.go`); B+tree changes need property
  tests against a mirror map.

## Getting started

```sh
cd engine && go test ./...        # everything should be green
cd ../xray && pnpm install && pnpm test
```

Good first issues live in the README's roadmap: JOINs, aggregates
(`SUM`/`AVG`/`MIN`/`MAX`), overflow pages, B+tree node merging, `DROP TABLE`.

## Style

- Go: `gofmt`, `go vet`, table-driven tests, comments explain *why*.
- TypeScript: strict mode, keep the reducer pure (replay depends on it).
- Conventional commit subjects (`feat:`, `fix:`, `docs:`) appreciated.
