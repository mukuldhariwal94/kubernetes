# 0001 — Process-wide compiled-program cache

**Status:** drafted, not yet upstream.
**Side:** k8s (`staging/src/k8s.io/apiserver/pkg/admission/plugin/cel`).
**Headline impact (k8s side):** templated workloads — 60 %+ of compilation-subsystem heap.

## What it does

Wraps `compiler.CompileCELExpression` with a process-wide bounded LRU. The
cache key is a SHA-256 fingerprint of every input that affects the compiled
output:

- env-template pointer identity (so compilers built with custom program
  options like a non-default `cel.CostLimit` never share entries),
- normalized expression source,
- `OptionalVariableDeclarations` (`HasParams`, `HasAuthorizer`,
  `HasPatchTypes`),
- `environment.Type` (`StoredExpressions` / `NewExpressions`),
- sorted return-type signature from the `ExpressionAccessor`,
- sorted "name:type" snapshot of in-scope composition variables, only
  consulted when the expression text contains `variables`.

On a hit, the shared `cel.Program` is returned with the caller's
`ExpressionAccessor` rebound (so downstream type assertions like
`*ValidationCondition` still resolve to the right policy's metadata).
On a miss, the compile runs, and only successful compilations (`Error == nil
&& Program != nil`) are cached — failures are retriable.

## Why

`cel.Program` is documented goroutine-safe, and the per-policy
`compileFresh` was producing one per `(policy × expression)`. At
N=10 000 policies × 5 expressions each, that is 50 000 `cel.Program`
instances even when the expression text is identical across policies.
Templated multi-tenant clusters (the dominant real-world shape) collapse
trivially under text dedup.

## Files touched

Added:
- `staging/src/k8s.io/apiserver/pkg/admission/plugin/cel/compile_cache.go`
- `staging/src/k8s.io/apiserver/pkg/admission/plugin/cel/compile_cache_test.go`

Modified:
- `staging/src/k8s.io/apiserver/pkg/admission/plugin/cel/compile.go`
- `staging/src/k8s.io/apiserver/pkg/admission/plugin/cel/composition.go`

## Public API additions (within the `cel` package)

- `GetCompileCacheStats() CompileCacheStats`
- `SetCompileCacheSizeForTests(size int) func()`

## Tests added

- `TestCompileCache_DedupesIdenticalExpressions`
- `TestCompileCache_RespectsOptionalDecls`
- `TestCompileCache_RespectsEnvType`
- `TestCompileCache_DoesNotCacheCompilationErrors`
- `TestCompileCache_NormalizesLeadingTrailingWhitespace`
- `TestCompileCache_BoundedSize`

## Safety contract

- Compilers obtained via `NewCompiler(env)` (the existing public
  constructor) **opt out** of the cache. This preserves correctness for
  callers that build envs with custom program options (custom cost limits,
  custom decorators, etc.) — those compilers have call-site-specific
  behaviour and must not share cache entries with production compilers.
- Compilers built via `NewCompositedCompiler(envTemplate)` opt in, keyed
  by `envTemplate`'s pointer identity. In production this is always the
  singleton `getCompositionEnvTemplateWithStrictCost()`.
- Errors are not cached.

## Pairs with

- **0008** caps the LRU size at 1500 (was 5000). Empirically each cached
  entry retains ~150–250 KB on apiserver-shaped envs.
- **0009** (cel-go vendor) eliminates ~62 % of the per-entry retained cost
  by sharing the interpreter Dispatcher across programs of an env. After
  0009 lands, 0008's cap can be revisited upward.

## Apply

```
git apply patches/0001-program-cache/0001-cel-add-process-wide-compiled-program-cache.patch
```
