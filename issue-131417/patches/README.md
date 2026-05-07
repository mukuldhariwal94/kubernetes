# Patches — kubernetes/kubernetes#131417

Each patch lives in its own directory with a `README.md` describing
problem, mechanism, savings, risks, and test status. The patches are
**additive and largely independent** — pick any subset.

Apply order suggestion (highest leverage first):

1. **0009** (cel-go vendor) — collapses ~62 % of compilation heap.
2. **0001** (k8s) — dedupes `cel.Program` by expression text.
3. **0008** (k8s) — caps the new program cache at 1500 (~300–400 MB).
4. **0005 / 0004 / 0002** (k8s) — collapse the per-policy `F` env-build
   term in declining order of leverage.
5. **0006** (k8s) — zero-variable fast path (steady-state RSS small,
   admission-path GC churn meaningful).
6. **0003** (k8s) — wire up expression normalization in the cache key.

## Index

| # | Folder | Side | Headline | Status |
|---|---|---|---|---|
| 0001 | [0001-program-cache/](0001-program-cache/) | k8s | Process-wide compiled-program cache | drafted |
| 0002 | [0002-shared-compilers/](0002-shared-compilers/) | k8s | Process-wide `*CompositedCompiler` reuse | drafted |
| 0003 | [0003-expression-normalization/](0003-expression-normalization/) | k8s | Expression normalization utility (utility only) | drafted |
| 0004 | [0004-lazy-varenvs-matrix/](0004-lazy-varenvs-matrix/) | k8s | Lazy 8-entry `varEnvs` matrix | drafted |
| 0005 | [0005-share-varenvs-matrix/](0005-share-varenvs-matrix/) | k8s | Share `varEnvs` matrix per env template | drafted |
| 0006 | [0006-skip-variables-extend/](0006-skip-variables-extend/) | k8s | Skip `variables` Extend for zero-variable policies | drafted |
| 0008 | [0008-shrink-cache-cap/](0008-shrink-cache-cap/) | k8s | Cap process-wide compile-cache LRU at 1500 | applied locally |
| 0009 | [0009-celgo-share-dispatcher/](0009-celgo-share-dispatcher/) | **cel-go** | Share `interpreter.Dispatcher` per `*cel.Env` | applied locally (vendor) |
| 0010 | [0010-lazy-varenvs/](0010-lazy-varenvs/) | k8s | Lazify per-compiler `varEnvs` + share request/namespace DeclTypes; ~14× cut in per-policy compile-time allocation | prototype, includes benchmark |
| 0011 | [0011-presize-policy-source-result/](0011-presize-policy-source-result/) | k8s | Pre-size `calculatePolicyData` result slice to unique-policy count; ~790 KiB / refresh saved at 100×100 binding ratio | prototype, includes benchmark |
| 0012 | [0012-drop-checkedexpr-discard-and-singleton-returntypes/](0012-drop-checkedexpr-discard-and-singleton-returntypes/) | k8s | Drop dead `AstToCheckedExpr` discard in `CompileCELExpression` + singleton `ReturnTypes()` slices; ~17 KiB / 252 allocs saved per complex compile | prototype, includes benchmark |

There is no 0007 — the slot was reserved for the cel-go vendor change
during planning and the actual change shipped as 0009.

## Patch dependency graph

```
0009 ──┐
       ├── independent of all k8s-side patches
       │   (lives in cel-go; consumed via vendor bump)
       │
0001 ──┴── independent
0008 ──── pairs with 0001 (caps the cache 0001 introduces)
0002 ──── independent of 0001/0009
0004 ──── precedes 0005 (lazy matrix is the sane default before sharing it)
0005 ──── builds on 0004
0006 ──── independent
0003 ──── utility; wire into 0001/0002 cache key after both land
```

## Empirical context

The headline numbers are anchored to the heap snapshot summarized in
[`../analysis/master-analysis.md`](../analysis/master-analysis.md) and the
sweep benchmark in [`../benchmarks/sweep/RESULTS.md`](../benchmarks/sweep/RESULTS.md).
Detailed per-patch reasoning, trade-offs, and risks live in
[`../analysis/extended-analysis.md`](../analysis/extended-analysis.md)
sections 3.1–3.4.
