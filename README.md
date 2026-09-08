# Bil

**Bil** is a variant of Go that adds language and runtime support for parallel processors, see [`docs/guide.md`](https://github.com/bil-lang/bil/blob/main/docs/guide.md) for an introduction to the Bil language, expressed as worked examples.

## Install

Bil needs a Go toolchain on `PATH` at runtime - get Go from https://go.dev/dl/ if you don't already have it.

On macOS, Linux, etc.:

```
curl -fsSL https://bil-lang.org/install.sh | sh
```

On Windows (PowerShell):

```
irm https://bil-lang.org/install.ps1 | iex
```

These install a prebuilt `bil` binary for your platform, and place it on your `PATH`.

## Run Bil code

Run a `.bil` file with:

```
bil run examples/01-twoprocs.bil
```

`bil run` chains together Bil's two internal tools:

1. **`tools/bilc`** — a source-to-source preprocessor (a _transpiler_) that uses `go/scanner` and `go/token` to parse Bil code, apply certain heuristic rules and to provide a set of helper functions. Finally, it uses `go/format` to translate the `.bil` file into legal Go.
2. **`tools/vet`** — a `go/ast`/`go/types`-based static analyzer that checks the transpiled Go against Bil's usage rules (for example: a channel may only be used for input in one `par` branch, and output in one other).
3. Only if that passes: calls `go run` on the transpiled Go.

To run steps 1–2 only, without executing the program, use `bil vet` instead of `bil run`.

## Editor support

There's no dedicated `.bil` grammar yet, but since Bil is close enough to Go syntactically, telling your editor to highlight `.bil` files as Go gets you most of the way there. In VSCode, add this to `settings.json` (workspace or user): `"files.associations": { "*.bil": "go" }`. This just picks the highlighter — it doesn't run `gofmt`/`gopls` against `.bil` files, since they aren't valid Go until `bilc` rewrites them. Other editors / IDEs will have similar settings.

## Run tests

```
cd tools/bilc
go test ./...
```

This runs `tools/bilc/bilc_test.go` against the fixtures in `tools/bilc/testdata/`: `ok/*.bil` files are transpiled, run, and checked against a matching `*.golden` file; `err/*.bil` files are checked to fail with an error containing the matching `*.err` file's text. To add a regression test, drop a new `.bil` + `.golden` (or `.err`) pair into the right directory — no test code to touch.

`ok/*.bil` fixtures need deterministic output, since they're checked with an exact string match. An example whose output is inherently timing-dependent (e.g. `examples/03-server-alt.bil`, which races a timer against a sleep) is deliberately left out of `testdata/`, and is only meant to be run manually via `bil run`.

`tools/vet` and `tools/bil` have their own `go test ./...` suites, run the same way from within each directory.

## Build from source

To build it from source instead:

```
cd tools/bil && go build -o bil . && cd ../..
```

(the examples assume `bil` is on `PATH`; if you built from source without moving the binary, run `tools/bil/bil` in its place)

## License

Copyright (c) 2026 Stephen Roe. BSD 3-Clause license -- see `LICENSE`. See `NOTICE` for the occam/CSP and Go influences and attributions (Go's own license text is in `LICENSE-GO`).
