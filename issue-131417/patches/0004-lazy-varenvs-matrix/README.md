# 0004 — Lazy `varEnvs` matrix (Patch A)

**Status:** ready (rebuilt as a real diff; `git apply` verified).
**Side:** k8s (`staging/src/k8s.io/apiserver/pkg/admission/plugin/cel`).
**Headline impact:** bounded — caps at ~75 % of the `mustBuildEnvs` slice
of heap (~8 % of total compilation-subsystem heap).

**Prerequisite:** patch **0001** (process-wide compiled-program cache). This
patch edits the `compiler` struct, `newCachedCompiler`, and `compileFresh`
that 0001 introduces, plus the `"strings"` import 0001 adds. Apply 0001 first.

**Mutually exclusive with patch 0010** ("lazify per-compiler varEnvs"): both
rewrite the same `varEnvs` region of `compile.go`. 0004 keeps the per-compiler
`BuildNamespaceType`/`BuildRequestType` calls; 0010 additionally hoists those
into process-wide singletons. Pick one, not both.

## Problem

`mustBuildEnvs` builds 8 `*environment.EnvSet` entries eagerly per
`compiler` (= 16 cel-go `cel.Env.Extend` clones — one for `NewExpressions`,
one for `StoredExpressions`, across the Cartesian
`HasParams × HasAuthorizer × HasPatchTypes`).

VAP only ever uses **2 of the 8** combos in production
(`{HasParams, HasAuthorizer:true}` for validations/audits and
`{HasParams, HasAuthorizer:false}` for messageExpressions). MAP uses 4.
The rest is dead weight.

## What it does

Replace the eager `varEnvs map[OptionalVariableDeclarations]*environment.EnvSet`
with a lazy memoized table: a `*sync.Map` of materialized `EnvSet`s plus a
`*sync.Map` of `*sync.Once` guards (both held by pointer on the `compiler`
struct so the value-receiver `compiler` methods share one table). The new
`compiler.envFor(opts)` builds an entry on first request via `createEnvForOpts`
and reuses it thereafter; `compileFresh` calls `envFor` instead of indexing the
old map. `mustBuildEnvs` is removed.

`createEnvForOpts` is pure with respect to its inputs, so memoizing it
cannot change observed behaviour.

## Expected savings

| Before | After |
|---|---|
| 8 × 2 = 16 cel-go envs per compiler | 2 × 2 = 4 (VAP) or 4 × 2 = 8 (MAP) |
| ~50 KB per compiler `F` | ~12 KB per compiler |

At 10 000 unique-shape policies the addressable heap is roughly
**300–500 MB saved** — useful, but not transformative on its own.

## Tradeoffs

- **CPU:** amortized identical. First admission against a given `(opts)`
  combo pays the build cost once.
- **Latency:** tiny one-time hit (~1–5 ms per combo) on first match.
- **Complexity:** small. `sync.Map` + `sync.Once` per key.

## Risks

The current eager build `panic`s on a misconfigured base env at compiler
construction time. With lazy construction the same error surfaces on first
use instead — but `envFor` returns it and `compileFresh` turns it into a
normal `CompilationResult` internal error rather than a panic, so this is
strictly less disruptive. `envFor` also clears the `once` entry on a failed
build so a later call can retry, matching the previous "error at every
compile attempt" behaviour.

## Apply

```
git apply patches/0001-program-cache/0001-cel-add-process-wide-compiled-program-cache.patch
git apply patches/0004-lazy-varenvs-matrix/0004-cel-lazy-varenvs-matrix.patch
```

## See also

Detailed rationale in [`../../analysis/extended-analysis.md`](../../analysis/extended-analysis.md), section 3.1.
