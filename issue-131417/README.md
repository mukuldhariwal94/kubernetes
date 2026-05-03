# Patches for kubernetes/kubernetes#131417

> High memory consumption caused by CEL in `ValidatingAdmissionPolicy` and
> `ValidatingAdmissionPolicyBinding`.

This directory contains three independent, additive patches that each
attack the per-policy memory blow-up from a different angle. All three
respect the constraint that the generic admission policy framework
(`plugin/policy/generic/**`) is **not modified** — every change is
contained to:

- `staging/src/k8s.io/apiserver/pkg/admission/plugin/cel/**` (the CEL
  compiler façade)
- `staging/src/k8s.io/apiserver/pkg/admission/plugin/policy/validating/plugin.go`
- `staging/src/k8s.io/apiserver/pkg/admission/plugin/policy/mutating/compilation.go`

## Patches at a glance

| # | File | Approach | What it shares | Memory hot-spot it kills |
|---|---|---|---|---|
| 1 | `0001-cel-add-process-wide-compiled-program-cache.patch` | **A. Per-expression program cache** | `cel.Program` (the compiled CEL bytecode + AST) | Duplicate `cel.Program` instances across every policy that has the same expression text |
| 2 | `0002-cel-share-composited-compilers-across-policies.patch` | **B. Per-policy compiler reuse** | `*CompositedCompiler` (env chain + 9 sub-envs + variable programs) | Per-policy `envSet.Extend(...)` chain + `mustBuildEnvs` (8 envs) |
| 3 | `0003-cel-add-expression-normalization-utility.patch` | **C. Expression normalization utility** | (utility only — not wired by default) | Cosmetic differences (whitespace, line endings) that prevent cache hits in 1 / 2 |

Patches 1 and 2 are **complementary** and **orthogonal**: applying both
is recommended for maximum benefit. Patch 3 is a small, standalone
canonicalization library that can be wired into the cache key of either
patch (or future caches) in a follow-up.

## Application

```bash
# Apply all three (recommended order):
git apply patches/issue-131417/0001-cel-add-process-wide-compiled-program-cache.patch
git apply patches/issue-131417/0002-cel-share-composited-compilers-across-policies.patch
git apply patches/issue-131417/0003-cel-add-expression-normalization-utility.patch

# Or any subset — they have no interdependencies:
git apply patches/issue-131417/0001-cel-add-process-wide-compiled-program-cache.patch
```

Each patch passes `git apply --check` cleanly against `master` at the
time of authoring.

## Patch 1 — Process-wide compiled-program cache (Approach A)

**Files added**

- `staging/src/k8s.io/apiserver/pkg/admission/plugin/cel/compile_cache.go`
- `staging/src/k8s.io/apiserver/pkg/admission/plugin/cel/compile_cache_test.go`

**Files modified**

- `staging/src/k8s.io/apiserver/pkg/admission/plugin/cel/compile.go`
- `staging/src/k8s.io/apiserver/pkg/admission/plugin/cel/composition.go`

**Mechanism.** Wraps `compiler.CompileCELExpression` with a process-wide
bounded LRU (default size **5000**, atomic-pointer-swappable for tests).
The cache key is a SHA-256 fingerprint of:

- env-template identity (so compilers built from custom envs don't share
  with production compilers — critical for tests with custom `CostLimit`
  or other `cel.ProgramOption`s),
- normalized expression source,
- `OptionalVariableDeclarations` (3 bools),
- `environment.Type` (`StoredExpressions` / `NewExpressions`),
- sorted return-type signature,
- sorted "name:type" snapshot of in-scope composition variables (only
  consulted when the expression text contains the substring `variables`).

On a cache hit, a shared `cel.Program` is returned and the caller's
`ExpressionAccessor` is rebound so downstream type assertions
(e.g. `*ValidationCondition`) still resolve to the right policy's
metadata.

**Safety contract.** Compilers obtained via `NewCompiler(env)` (the
existing public constructor) **opt out** of the cache, preserving
correctness for callers that build envs with custom program options.
Compilers built via `NewCompositedCompiler(envTemplate)` opt in, keyed
by `envTemplate`'s pointer identity — which in production is always
the singleton `getCompositionEnvTemplateWithStrictCost()`.

**Errors are not cached** — a compile failure can be retried after the
underlying error is fixed.

**Tests added** (6):

- `TestCompileCache_DedupesIdenticalExpressions`
- `TestCompileCache_RespectsOptionalDecls`
- `TestCompileCache_RespectsEnvType`
- `TestCompileCache_DoesNotCacheCompilationErrors`
- `TestCompileCache_NormalizesLeadingTrailingWhitespace`
- `TestCompileCache_BoundedSize`

**Public API additions** (within the `cel` package):

- `GetCompileCacheStats() CompileCacheStats`
- `SetCompileCacheSizeForTests(size int) func()`

## Patch 2 — Process-wide `CompositedCompiler` reuse (Approach B)

**Files added**

- `staging/src/k8s.io/apiserver/pkg/admission/plugin/cel/composition_cache.go`
- `staging/src/k8s.io/apiserver/pkg/admission/plugin/cel/composition_cache_test.go`

**Files modified**

- `staging/src/k8s.io/apiserver/pkg/admission/plugin/policy/validating/plugin.go`
- `staging/src/k8s.io/apiserver/pkg/admission/plugin/policy/mutating/compilation.go`

**Mechanism.** Introduces `cel.GetOrCreateCompositedCompiler(envTemplate,
variables, opts, envType)` — a process-wide cache (default size **1000**)
keyed by:

- env-template pointer,
- order-insensitive variable signature (sorted by name; collision-safe
  framing on `name`/`expr`),
- `OptionalVariableDeclarations`,
- `environment.Type`.

The cache value is a `*CompositedCompiler` whose variables are already
compiled and stored. On a hit, both **the per-policy `envSet.Extend(...)`
chain** and **the `mustBuildEnvs` 8-env build** are skipped entirely —
this is the dominant fixed-per-policy cost identified in the analysis.

**Safety contract.** Callers must not call `CompileAndStoreVariables`
on the returned compiler; the factory has already done so. Per-policy
`CompileCondition` / `CompileMutatingEvaluator` calls are unaffected and
continue to produce per-call evaluators that do not mutate the shared
state.

**Tests added** (5):

- `TestGetOrCreateCompositedCompiler_ReusesAcrossPoliciesWithSameVariables`
- `TestGetOrCreateCompositedCompiler_NoVariablesAllShare`
- `TestGetOrCreateCompositedCompiler_DifferentVariablesDoNotShare`
- `TestGetOrCreateCompositedCompiler_OrderInsensitiveVariables`
- `TestGetOrCreateCompositedCompiler_DifferentOptsDoNotShare`

**Public API additions**:

- `GetOrCreateCompositedCompiler(...)`
- `GetCompositedCompilerCacheStats() CompositedCompilerCacheStats`
- `SetCompositedCompilerCacheSizeForTests(size int) func()`

## Patch 3 — Expression normalization utility (Approach C)

**Files added**

- `staging/src/k8s.io/apiserver/pkg/admission/plugin/cel/normalize.go`
- `staging/src/k8s.io/apiserver/pkg/admission/plugin/cel/normalize_test.go`

**Files modified**: none.

**Mechanism.** Adds `cel.NormalizeExpression(string) string`, a
deterministic, idempotent canonicalizer that:

- strips leading and trailing whitespace,
- normalizes CRLF / standalone CR to LF,
- strips trailing whitespace from each internal line,
- preserves all internal whitespace (including inside string literals)
  — critical so cel-go error spans aren't perturbed.

The intent is to be wired into the cache keys of Patches 1 and 2 (or any
future caches) to lift cache hit ratios when policy authors write the
same expression with different incidental whitespace. Standalone, the
utility is a no-op on existing behaviour. Includes 10 case-driven tests
plus an idempotency test plus 2 benchmarks (canonical fast path and
needs-normalization slow path).

**Public API addition**: `NormalizeExpression(string) string`.

## Combined effect

If applied together, the per-policy memory cost drops from
`F + E_p × C` (per the analysis in the design doc — `F` = per-policy
fixed overhead, `E_p` = expressions per policy, `C` = bytes per
compiled program) to roughly `F_small + (U_unique × C) / N` where
`F_small` is just the validator wrapper struct and `U_unique` is the
number of structurally distinct expressions in the cluster.

Reproducing the issue reporter's worst case (1000 policies × 100
bindings × 50 expressions, ~7 GiB observed) is expected to drop by an
order of magnitude or more once shared expressions deduplicate.

## Risks

- All three patches are **opt-out friendly** by setting the cache size
  to 0 via the `SetXxxCacheSizeForTests` helpers; the behavior reverts
  to today exactly.
- `cel.Program` is documented goroutine-safe by cel-go, so sharing
  across policies is correctness-safe.
- The cache key fingerprints incorporate every input that affects the
  compiled output (expression text, env identity, all options, return
  types, in-scope variables) — see the design doc for the full
  justification.
- No public API changes to the admission plugin framework. The new
  exported helpers live entirely in
  `staging/src/k8s.io/apiserver/pkg/admission/plugin/cel`.
