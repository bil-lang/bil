# Changes

## Unreleased

- `bil run --dry-run [-rows N] [-cols N]`: ignore placement directives and run a `placed par` program locally as goroutines instead of refusing it
- Constant-arity replicated `alt` compiles to a native `select`, no `reflect` — fixes tinygo builds
- Warn when replicated `alt` still needs `reflect.Select` (non-constant arity)
