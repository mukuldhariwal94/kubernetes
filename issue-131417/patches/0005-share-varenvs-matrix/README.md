# 0005 — Share `varEnvs` matrix across policies (Patch B)

**Status:** ready (rebuilt as a real diff; `git apply` + full `go test` of the
admission/policy/cel suites verified).
**Side:** k8s (`staging/src/k8s.io/apiserver/pkg/admission/plugin/cel`).
**Headline impact:** bounded — caps at ~11 % of total heap; closes the
residual `F` term that 0002 cannot collapse.

**Prerequisites:** patch **0001** (process-wide compiled-program cache) *and*
patch **0004** (lazy per-compiler `varEnvs`). This patch refactors 0004's
`compiler.envFor` and reuses the struct shape it introduced. Apply in order:
0001 → 0004 → 0005. **Incompatible with patch 0010** (the alternative to 0004,
which reshapes the same `varEnvs` region differently).

## Problem

After 0004, the `varEnvs` matrix is built lazily but **per `compiler`**.
The only thing that varies between two compilers built from the same env
template is the `variables` declType. The matrix construction is
otherwise identical.

When 0002 *misses* (different variables), each policy still recomputes
the matrix from scratch — 8 cel-go env clones whose
function/library/declType machinery is byte-for-byte identical to
thousands of others.

## What it does

- **`compile_matrix_pool.go`** (new): `sharedVarEnvsPool`, a process-wide map
  (`matrixPoolCache`) keyed by env-template pointer. Each `(template, opts)`
  EnvSet is built once under `sync.Once` and reused thereafter. Built from the
  **base template** (without `variables`).
- **`compile.go`**: `compiler` grows `sharedMatrix *sharedVarEnvsPool` and
  `variablesOverlay *environment.VersionedOptions`. `envFor` is refactored to
  memoize generically; the new `buildEnv` either (a) pulls the `(opts)` entry
  from the shared pool and `Extend`s the per-policy `variables` overlay on top,
  or (b) — for standalone `NewCompiler` — builds from the compiler's own base
  exactly as 0004 did.
- **`composition.go`**: `NewCompositedCompiler` captures the `variables` overlay
  declaratively and points the cached compiler at `sharedMatrixForTemplate(envSet)`.
  `state.EnvSet` still carries the variables-extended env, so callers that
  consult `CompositedCompiler.Env` / `compositionState` directly are unaffected.

```
Pre-patch:  8-entry matrix rebuilt per compiler  (O(policies) retained envs)
Post-patch: 8-entry matrix built per template    (O(templates) — one in prod)
            + one thin `variables` Extend per (opts) combo per policy
```

The set of declarations in the compilation env is identical either way:
in both the old `template → variables → opts` and the new
`template → opts → variables` orderings you end up with
`(template) ∪ (opts decls) ∪ (variables decl) ∪ (declTypes)`. cel-go's
`Extend` treats added options as additive and order-independent for
declarations of disjoint names, and `variables` is disjoint from every opts
variable name (`object`, `oldObject`, `params`, `request`, `namespaceObject`,
`authorizer`, `authorizer.requestResource`).

## Expected savings

| Workload | Before | After |
|---|---|---|
| 1 000 policies, 100 unique variable signatures | 26 000 envs | ~1 116 |
| 10 000 policies, 100 unique signatures | 260 000 envs | ~10 116 |

At 10 000 policies the absolute saving is a few hundred MB — bounded by
the `mustBuildEnvs` slice (~11 % of heap in the reference profile). Real
but not multi-GB; do not oversell.

## Tradeoffs

- **CPU:** slightly improved.
- **Latency:** cold-start improves; first-policy compile pays the matrix
  once.
- **Complexity:** medium. Pool needs careful invariants (templates must
  be pointer-stable; `getCompositionEnvTemplateWithStrictCost()` already
  is).

## Risks

- **Pointer-keyed cache** assumes templates are stable. The production caller
  uses the `sync.Once`-guarded singleton `getCompositionEnvTemplateWithStrictCost()`,
  so there is exactly one pool entry in a running apiserver. Tests that build
  per-test templates accumulate a handful of small extra entries — harmless,
  never freed, but the pool is process-wide so test pollution is bounded by the
  number of distinct templates a test binary creates.
- **Order-of-Extend semantics:** the existing `cel` / `policy/{validating,mutating}`
  / `cel/...` / `webhook/matchconditions` test suites pass with this change
  (they exercise type-checking, compilation, and evaluation of variable-bearing
  and variable-free policies). The argument that `template → opts → variables`
  ≡ `template → variables → opts` rests on cel-go's order-independent additive
  `Extend` for disjoint declaration names, which the variable names here satisfy.
- The per-policy `variables` overlay is applied to the shared matrix entry
  inside `buildEnv` and the result is memoized in the compiler's own `envs`
  table, so each `(opts)` combo costs exactly one extra `Extend` per policy
  (not one per `compileFresh` call).

## Apply

```
git apply patches/0001-program-cache/0001-cel-add-process-wide-compiled-program-cache.patch
git apply patches/0004-lazy-varenvs-matrix/0004-cel-lazy-varenvs-matrix.patch
git apply patches/0005-share-varenvs-matrix/0005-cel-share-varenvs-matrix-across-policies.patch
```

## See also

Detailed rationale in [`../../analysis/extended-analysis.md`](../../analysis/extended-analysis.md), section 3.2.
