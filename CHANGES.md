# Changes

## Unreleased

- Constant-arity replicated `alt` compiles to a native `select`, no `reflect` — fixes tinygo builds
- Warn when replicated `alt` still needs `reflect.Select` (non-constant arity)
