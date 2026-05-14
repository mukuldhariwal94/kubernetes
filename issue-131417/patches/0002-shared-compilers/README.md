# 0002 — Process-wide `CompositedCompiler` reuse

**Status:** ready (applies cleanly on bare master; `git apply` verified).
**Side:** k8s (`staging/src/k8s.io/apiserver/pkg/admission/plugin/cel` +
`policy/{validating,mutating}`).
**Headline impact:** meaningful when policies share variable shapes;
collapses the per-policy fixed `F` term in the memory model.

**Base:** bare master (no prerequisite).
**Overlaps with patch 0006:** both rewrite the `compilePolicy` functions in
`policy/validating/plugin.go` and `policy/mutating/compilation.go`, so the two
patches do **not** apply on top of each other without a manual merge of that
region. Pick one, or merge the `compilePolicy` change by hand if you want both.
(Independent of 0001, 0003, 0004, 0005, 0009, 0010, 0011, 0012.)

## What it does

Introduces `cel.GetOrCreateCompositedCompiler(envTemplate, variables, opts,
envType)` — a process-wide cache (default size 1000) keyed by:

- env-template pointer,
- order-insensitive variable signature (sorted by name, collision-safe
  framing on `(name, expr)` pairs),
- `OptionalVariableDeclarations`,
- `environment.Type`.

The cache value is a `*CompositedCompiler` whose variables are already
compiled and stored. On a hit, both the per-policy `envSet.Extend(...)`
chain **and** the `mustBuildEnvs` 8-env build are skipped — the dominant
fixed-per-policy cost identified in the analysis.

## Why

Each VAP policy compile today does ~13 `EnvSet.Extend` calls
(= 26 cel-go `cel.Env.Extend` clones) before any expression is
compiled. That's the constant `F` term: 150–500 KB per unique policy
shape. After 0001, programs are deduped — but every distinct
`*CompositedCompiler` still pays `F`. 0002 collapses `F` for policies
with structurally identical `variables`.

## Files touched

Added:
- `staging/src/k8s.io/apiserver/pkg/admission/plugin/cel/composition_cache.go`
- `staging/src/k8s.io/apiserver/pkg/admission/plugin/cel/composition_cache_test.go`

Modified:
- `staging/src/k8s.io/apiserver/pkg/admission/plugin/policy/validating/plugin.go`
- `staging/src/k8s.io/apiserver/pkg/admission/plugin/policy/mutating/compilation.go`

## Public API additions

- `GetOrCreateCompositedCompiler(...)`
- `GetCompositedCompilerCacheStats() CompositedCompilerCacheStats`
- `SetCompositedCompilerCacheSizeForTests(size int) func()`

## Tests added

- `TestGetOrCreateCompositedCompiler_ReusesAcrossPoliciesWithSameVariables`
- `TestGetOrCreateCompositedCompiler_NoVariablesAllShare`
- `TestGetOrCreateCompositedCompiler_DifferentVariablesDoNotShare`
- `TestGetOrCreateCompositedCompiler_OrderInsensitiveVariables`
- `TestGetOrCreateCompositedCompiler_DifferentOptsDoNotShare`

## Safety contract

Callers must not call `CompileAndStoreVariables` on the returned compiler;
the factory has already done so. Per-policy `CompileCondition` /
`CompileMutatingEvaluator` calls are unaffected and continue to produce
per-call evaluators that do not mutate the shared state.

## Apply

```
git apply patches/0002-shared-compilers/0002-cel-share-composited-compilers-across-policies.patch
```
