# 0006 — Skip `variables` Extend for zero-variable policies (Patch C)

**Status:** drafted.
**Side:** k8s (`staging/src/k8s.io/apiserver/pkg/admission/plugin/cel` +
`policy/{validating,mutating}`).
**Headline impact:** small RSS, meaningful GC churn relief on the
admission hot path.

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
policies; add `NewSimpleCompiler` (or a sentinel `mapType == nil`) for
zero-variable policies. The runtime `Variables()` method returns a
process-wide empty singleton.

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

- The empty-singleton `lazy.MapValue` must be returned by-value or as
  immutable. Returning a shared mutable map across requests would be a
  correctness bug. Easy to enforce with a wrapper type.

## Apply

```
git apply patches/0006-skip-variables-extend/0006-cel-skip-variables-extend-for-zero-variable-policies.patch
```

## See also

Detailed rationale in [`../../analysis/extended-analysis.md`](../../analysis/extended-analysis.md), section 3.3.
