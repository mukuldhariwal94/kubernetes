# 0013 — cel-go: reduce function overload bindings in `newProgram`

**Status:** prototype, applied to local working tree (vendored cel-go v0.26.0).  
**Side:** cel-go vendor (`vendor/github.com/google/cel-go/cel/program.go`).  
**Headline impact:** **~98 % reduction in `*functions.Overload` closure allocations per
`env.Program()` call** for type-checked expressions — from 345 closures down to 5–15
depending on expression complexity.

## What this patch addresses

Every call to `env.Program(ast, ...)` in `newProgram()` iterates over **all** registered
functions and unconditionally calls `fn.Bindings()` for each:

```go
// before (vendor/github.com/google/cel-go/cel/program.go:192-200)
for _, fn := range e.functions {
    bindings, err := fn.Bindings()
    if err != nil {
        return nil, err
    }
    err = disp.Add(bindings...)
    if err != nil {
        return nil, err
    }
}
```

`fn.Bindings()` allocates a new `*functions.Overload` struct per declared overload,
wrapping Go closures around each implementation. The dispatcher then holds references to
all of them for the lifetime of the `cel.Program`.

In a production Kubernetes admission webhook environment the base `cel.Env` contains
**157 functions / 345 overloads** sourced from 17 libraries (stdlib, ext.Strings, ext.Lists,
library.Authz, library.IP, library.CIDR, library.Quantity, library.Format, etc.). A
typical policy expression references only 5–15 of those 345 overloads.

Observed from production logs (`logFunctionBindingAnalysis` diagnostic in `compile.go`):

```
expression="has(object.metadata.labels) && object.metadata.labels['env1'].matches('^prod-[a-z]+')"
envOverloads=345  usedOverloads=5  wastedOverloads=340   (98.5 % waste)

expression="(!has(object.spec.replicas) || object.spec.replicas <= 3) && size(...) <= 11"
envOverloads=345  usedOverloads=11  wastedOverloads=334  (96.8 % waste)
```

At steady state with 5 000 policies × 5 expressions, this is **~8.5 million** unnecessary
`*functions.Overload` allocations per full policy refresh, each holding a Go closure.

## The fix — `addNeededBindings`

Replace the unconditional loop with `addNeededBindings`, which uses the AST's
`ReferenceMap()` (populated by the type-checker) to find the exact set of overload IDs
resolved for this expression, then calls `fn.Bindings()` only for the functions that have
at least one referenced overload:

```
Step 1  collect usedOIDs from ast.ReferenceMap()         e.g. {"matches_string", "logical_and", ...}
Step 2  build overloadID → functionName reverse index    e.g. {"matches_string" → "matches", ...}
Step 3  derive neededFns from usedOIDs                   e.g. {"matches", "_&&_", ...}
Step 4  for each fn in neededFns: call fn.Bindings()
        — add singleton binding if b.Operator == fnName
        — add overload binding  if b.Operator ∈ usedOIDs
```

For unchecked ASTs (parse-only mode) the function falls back to the original behaviour and
binds all functions.

### Dispatch key mechanics

The `interpreter.Dispatcher` uses two key styles, which step 4 handles explicitly:

| Binding type | `b.Operator` value | Dispatcher key |
|---|---|---|
| Singleton (one impl handles all type combos) | function name, e.g. `"_&&_"` | function name |
| Per-overload (separate impl per type pair) | overload ID, e.g. `"int_add_int"` | overload ID |

The planner resolves calls by first trying the overload ID, then falling back to the
function name. Both cases are correctly handled.

## Files touched

```
M  vendor/github.com/google/cel-go/cel/program.go   (+87 lines — addNeededBindings function + call site)
```

## Memory impact

| Metric | Before | After | Delta |
|---|---|---|---|
| `*functions.Overload` allocs per `env.Program()` | 345 | 5–15 | **−330 to −340 (−96–98%)** |
| `fn.Bindings()` calls per `env.Program()` | 157 | 3–10 | **−144 to −154 (−92–98%)** |
| Closures held in dispatcher per `cel.Program` | 345 | 5–15 | **−330 to −340** |

At 5 000 policies × 5 expressions per refresh:

| | Before | After | Delta |
|---|---|---|---|
| Overload allocs / refresh | ~8 625 000 | ~187 500 | **−8 437 500 (−97.8 %)** |

## Correctness analysis

- `ast.ReferenceMap()` is produced by the type-checker (`env.Compile`) on the line
  immediately before `env.Program()`. It is stable, complete, and already available with
  no extra cost.
- Every overload ID in `ReferenceMap` corresponds exactly to a key used by the
  `interpreter.Planner` to look up the binding. Filtering to these IDs is therefore
  semantically equivalent to binding all of them.
- Singleton functions (e.g. logical operators `_&&_`, `_||_`) register a single binding
  keyed by the **function name**, not an overload ID. Step 4 correctly detects and
  includes these via the `b.Operator == name` check.
- Unchecked ASTs fall back to full binding, preserving the existing behaviour for any
  code path that calls `env.Program` on a parse-only AST.
- The `decls` import added to `program.go` is already present in the same module; it
  introduces no new dependency.

## Reproduction

Use the `logFunctionBindingAnalysis` diagnostic already in `compile.go` to confirm the
before/after overload counts. Before the patch:

```
envOverloads=345  usedOverloads=5  wastedOverloads=340
```

After the patch, the number of closures actually allocated matches `usedOverloads` (5–15),
not `envOverloads` (345). The `wastedOverloads` log value is unchanged — it measures
*declared* overloads vs *referenced* overloads, independent of the binding path.

To benchmark allocation delta:

```bash
go test -run='^$' \
  -bench='BenchmarkCompileFresh\|BenchmarkProgram' \
  -benchmem -count=3 \
  ./staging/src/k8s.io/apiserver/pkg/admission/plugin/cel/
```

## What this patch does *not* change

- Does not change CEL evaluation semantics or results.
- Does not change what is *declared* in the env (all 157 functions remain available for
  type-checking and future `env.Program()` calls with different expressions).
- Does not address the `p.interpreter` / `p.dispatcher` retention across the lifetime of a
  cached `cel.Program` — that is a separate orthogonal change.
- Does not require any changes to Kubernetes code outside the vendor directory.

## Apply

```bash
git apply issue-131417/patches/0013-cel-reduce-overload-bindings/0013-celgo-reduce-overload-bindings.patch
```

## Upstream path

The same change can be proposed upstream to `cel-go` as a `cel.ProgramOption` or a
change to `newProgram` itself. The `ast.ReferenceMap()` API is public and stable in
v0.26+. A cleaner upstream API would be `cel.BindingFilter(func(fnName, overloadID string) bool)`
passed as a `ProgramOption`, allowing callers to customise the filtering strategy without
forking `program.go`.
