# 0005 — Share `varEnvs` matrix across policies (Patch B)

**Status:** drafted.
**Side:** k8s (`staging/src/k8s.io/apiserver/pkg/admission/plugin/cel`).
**Headline impact:** bounded — caps at ~11 % of total heap; closes the
residual `F` term that 0002 cannot collapse.

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

Build the 8-env matrix from the **base template** (without `variables`),
share it process-wide, and apply the per-policy `variables` declaration
as a thin Extend on top.

```
Pre-patch:  9 × N env extends
Post-patch: 8 + 1×K + 1×N    where K = unique variable signatures
```

The set of declarations in the resulting env is identical in both
flows: in either ordering you end up with `(template) ∪ (opts decls)
∪ (variables decl) ∪ (declTypes)`. cel-go's `Extend` treats added
options as additive and order-independent for declarations of disjoint
names.

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

- **Pointer-keyed cache** assumes templates are stable. Production caller
  uses a `sync.Once`-guarded singleton.
- **Order-of-Extend semantics:** tests must confirm equivalent
  type-checked output and identical eval results.

## Apply

```
git apply patches/0005-share-varenvs-matrix/0005-cel-share-varenvs-matrix-across-policies.patch
```

## See also

Detailed rationale in [`../../analysis/extended-analysis.md`](../../analysis/extended-analysis.md), section 3.2.
