# Changes

## Unreleased

- `bil emu -target none [-rows N] [-cols N]`: run a `placed par` program locally as goroutines, ignoring placement, instead of refusing it
- Constant-arity replicated `alt` compiles to a native `select`, no `reflect` — fixes tinygo builds
- Warn when replicated `alt` still needs `reflect.Select` (non-constant arity)
