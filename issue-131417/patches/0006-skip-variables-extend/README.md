# 0006 — Skip `variables` Extend for zero-variable policies (Patch C)

**Status:** ready (rebuilt as a real diff; `git apply` + `go test` verified).
**Side:** k8s (`staging/src/k8s.io/apiserver/pkg/admission/plugin/cel` +
`policy/{validating,mutating}`).
**Headline impact:** small RSS, meaningful GC churn relief on the
admission hot path.

**Prerequisite:** patch **0001** (process-wide compiled-program cache).
`NewSimpleCompiler` calls `newCachedCompiler`, which 0001 introduces. Apply
0001 first. This patch does **not** touch `compile.go`, so it composes with
patches 0004 / 0005 / 0010 (verified: `0001+0004+0005+0006` and `0001+0010+0006`
both apply, build, vet, and test clean).

**Overlaps with patch 0002:** both rewrite the `compilePolicy` functions in
`policy/validating/plugin.go` and `policy/mutating/compilation.go`. 0002 and
0006 therefore do **not** stack without a manual merge of that region — pick
one, or merge by hand.

## Problem

`NewCompositedCompiler` unconditionally extends the template with the
`variables` overlay:

```go
newEnvSet, err := envSet.Extend(environment.VersionedOptions{
    EnvOptions: []cel.EnvOption{cel.Variable("variables", newMapType.CelType())},
    DeclTypes:  []*apiservercel.DeclType{newMapType},
})
```

even when the caller has **zero variables**. Consequences:

1. Two extra cel-go env clones (`NewExpressions` + `StoredExpressions`).
2. `apiservercel.NewDeclTypeProvider(declTypes...)` chain gains an extra
   link, deepening every type lookup at compile *and* runtime.
3. The 8-env matrix is built on top of an env with the unused
   `variables` decl.
4. At runtime, `compositionContext.Variables(activation)` builds a
   `lazy.MapValue` even when the expression doesn't reference
   `variables`.

A meaningful fraction of in-the-wild VAPs declare zero `variables`.

## What it does

Split the constructor: keep `NewCompositedCompiler` for variable-bearing
policies; add `NewSimpleCompiler` for zero-variable policies. `NewSimpleCompiler`
leaves `compositionState.mapType` nil — the sentinel meaning "composition
disabled" — and skips the `variables` `Extend` entirely; the cached `compiler`
is still keyed by the base env template so it shares program-cache entries with
the composited path for expressions that don't reference `variables`.
`compositionContext.Variables()` returns a process-wide empty `lazy.MapValue`
singleton (`emptyVariablesMap`) when `mapType` is nil. `validating/plugin.go`
and `mutating/compilation.go` pick `NewSimpleCompiler` when `Spec.Variables` is
empty and skip the (then no-op) `CompileAndStoreVariables` call.

When `Spec.Variables == nil/empty`, no expression can legally reference
`variables.x` — that's a compile error today. Skipping the `variables`
declType cannot cause an expression that compiles today to fail to
compile, and vice versa.

## Expected savings

- The non-`mustBuildEnvs` `Extend` slice (the variables overlay path)
  is ~3.7 % direct in the reference profile (~15.6 MB / 1 000 policies).
  This patch eliminates it for zero-variable policies.
- Per-policy: ~10–20 KB retained heap saved.
- Per-request: 1 `lazy.MapValue` allocation + closure per validation
  expression saved. At 10 000 policies and 1 000 admission RPS this is
  the biggest contribution of this patch — **GC churn reduction**, not
  steady-state RSS.
- If 50 % of policies are zero-variable in a 10 000-policy cluster:
  **~50–150 MB** retained heap saved, plus a meaningful drop in
  allocation rate on the admission hot path.

## Tradeoffs

- **CPU:** marginal improvement.
- **Latency:** per-request slightly faster (skip the lazy map rebuild).
- **Complexity:** low. One sentinel value in `compositionState`. Two
  callsite branches.

## Risks

- The shared `emptyVariablesMap` is constructed once at package init from an
  empty `*apiservercel.DeclType` (no fields ever added). `lazy.MapValue` only
  exposes read/append paths and nothing in the zero-variable path appends, so
  sharing it across requests is safe — but if a future change makes `Variables()`
  reachable for a policy that *does* declare variables, this sentinel must not be
  hit (it is gated strictly on `mapType == nil`, which `NewSimpleCompiler` is the
  only producer of).
- `NewSimpleCompiler` and `NewCompositedCompiler` built from the same env
  template share the same `templateID` in the program cache. This is correct
  because the cache key also folds in the variable signature (empty for the
  simple compiler; non-empty for the composited compiler whenever the expression
  text contains `variables`), so the only entries they can share are those for
  expressions that genuinely don't reference `variables` — for which the two
  envs produce identical compiled programs.

## Apply

```
git apply patches/0001-program-cache/0001-cel-add-process-wide-compiled-program-cache.patch
git apply patches/0006-skip-variables-extend/0006-cel-skip-variables-extend-for-zero-variable-policies.patch
```

## See also

Detailed rationale in [`../../analysis/extended-analysis.md`](../../analysis/extended-analysis.md), section 3.3.
