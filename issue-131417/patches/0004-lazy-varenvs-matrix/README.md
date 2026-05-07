# 0004 — Lazy `varEnvs` matrix (Patch A)

**Status:** drafted.
**Side:** k8s (`staging/src/k8s.io/apiserver/pkg/admission/plugin/cel`).
**Headline impact:** bounded — caps at ~75 % of the `mustBuildEnvs` slice
of heap (~8 % of total compilation-subsystem heap).

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

Convert `varEnvs` from an eager
`map[OptionalVariableDeclarations]*environment.EnvSet` to a lazy memoized
`sync.Map` + `sync.Once` per combo. Build only what's actually requested
at `CompileCELExpression` time.

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

The current eager build surfaces a misconfiguration error at compiler
construction time. With lazy, it would surface at first use. Mitigation:
keep `NewCompiler` validating its inputs cheaply at construction.

## Apply

```
git apply patches/0004-lazy-varenvs-matrix/0004-cel-lazy-varenvs-matrix.patch
```

## See also

Detailed rationale in [`../../analysis/extended-analysis.md`](../../analysis/extended-analysis.md), section 3.1.
