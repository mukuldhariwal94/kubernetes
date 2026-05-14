# Patches — kubernetes/kubernetes#131417

Each patch lives in its own directory with a `README.md` describing
problem, mechanism, savings, risks, and test status. Each `.patch` is a real
`git apply`-able diff; some have a hard prerequisite on **0001** (they edit code
0001 introduces) — those are called out below and in each patch's README.

> **Heads-up on overlapping patches.**
> - 0004, 0005 and 0010 all reshape the same `varEnvs` region of `compile.go`.
>   **0010 is the alternative to 0004 + 0005.** Apply **either** `{0004, 0005}`
>   **or** `{0010}` — never a mix.
> - 0002 and 0006 both rewrite the `compilePolicy` functions in
>   `policy/validating/plugin.go` and `policy/mutating/compilation.go`, so they
>   don't apply on top of each other without a hand-merge of that region. Pick
>   one (or merge that hunk manually).

Apply order suggestion (highest leverage first; `git apply` each in turn):

1. **0009** (cel-go vendor) — collapses ~62 % of compilation heap. Independent.
2. **0001** (k8s) — dedupes `cel.Program` by expression text. Independent;
   prerequisite of 0004, 0005, 0006, 0008, 0010.
3. **0008** (k8s) — caps the new program cache at 1500 (~300–400 MB). Needs 0001.
4. Pick one of:
   - **0004** then **0005** (k8s) — lazy `varEnvs` matrix, then share it
     process-wide. Both need 0001; 0005 needs 0004.
   - **0010** (k8s) — single-patch lazy `varEnvs` + shared DeclType singletons.
     Needs 0001.
5. **0006** (k8s) — zero-variable fast path (steady-state RSS small,
   admission-path GC churn meaningful). Needs 0001; composes with 0004/0005/0010.
6. **0002** (k8s) — process-wide `*CompositedCompiler` reuse. Independent of 0001
   — but **cannot stack with 0006** (both edit `compilePolicy`); choose one or
   hand-merge that hunk.
7. **0003** (k8s) — expression normalization utility (independent; wire it into
   the 0001/0002 cache keys in a follow-up).
8. **0011**, **0012** (k8s) — independent micro-optimizations with benchmarks.

## Index

| # | Folder | Side | Headline | Base it applies on |
|---|---|---|---|---|
| 0001 | [0001-program-cache/](0001-program-cache/) | k8s | Process-wide compiled-program cache | master |
| 0002 | [0002-shared-compilers/](0002-shared-compilers/) | k8s | Process-wide `*CompositedCompiler` reuse | master |
| 0003 | [0003-expression-normalization/](0003-expression-normalization/) | k8s | Expression normalization utility (utility only) | master |
| 0004 | [0004-lazy-varenvs-matrix/](0004-lazy-varenvs-matrix/) | k8s | Lazy per-compiler `varEnvs` matrix | master + **0001** |
| 0005 | [0005-share-varenvs-matrix/](0005-share-varenvs-matrix/) | k8s | Share the `varEnvs` matrix per env template (process-wide pool) | master + **0001 + 0004** |
| 0006 | [0006-skip-variables-extend/](0006-skip-variables-extend/) | k8s | Skip `variables` Extend for zero-variable policies | master + **0001** |
| 0008 | [0008-shrink-cache-cap/](0008-shrink-cache-cap/) | k8s | Cap process-wide compile-cache LRU at 1500 | master + **0001** |
| 0009 | [0009-celgo-share-dispatcher/](0009-celgo-share-dispatcher/) | **cel-go** | Share `interpreter.Dispatcher` per `*cel.Env` | master (vendor) |
| 0010 | [0010-lazy-varenvs/](0010-lazy-varenvs/) | k8s | Lazify per-compiler `varEnvs` + share request/namespace DeclTypes (single-patch alternative to 0004+0005); ~14× cut in per-policy compile-time allocation | master + **0001** |
| 0011 | [0011-presize-policy-source-result/](0011-presize-policy-source-result/) | k8s | Pre-size `calculatePolicyData` result slice to unique-policy count; ~790 KiB / refresh saved at 100×100 binding ratio | master |
| 0012 | [0012-drop-checkedexpr-discard-and-singleton-returntypes/](0012-drop-checkedexpr-discard-and-singleton-returntypes/) | k8s | Drop dead `AstToCheckedExpr` discard in `CompileCELExpression` + singleton `ReturnTypes()` slices; ~17 KiB / 252 allocs saved per complex compile | master |

There is no 0007 — the slot was reserved for the cel-go vendor change
during planning and the actual change shipped as 0009.

## Patch dependency graph

```
0009 ──── independent (cel-go vendor bump)
0001 ──── independent
  ├── 0008 ──── needs 0001 (tweaks the cache constant 0001 introduces)
  ├── 0006 ──── needs 0001 (NewSimpleCompiler calls newCachedCompiler)
  │              └── conflicts with 0002 (both rewrite compilePolicy)
  └── varEnvs lazification — choose ONE branch, not both:
        0004 ──── needs 0001 ──┐
        0005 ──── needs 0004 ──┘  (lazy matrix, then share it process-wide)
        0010 ──── needs 0001       (single-patch alternative to {0004,0005})
0002 ──── independent of 0001/0009 (applies on bare master)
  │              └── conflicts with 0006 (both rewrite compilePolicy)
0003 ──── independent utility; wire into 0001/0002 cache keys in a follow-up
0011 ──── independent
0012 ──── independent
```

Every `.patch` here has been verified to `git apply` cleanly on the base shown
in the table above (and the rebuilt 0004/0005/0006 additionally pass
`go build` / `go vet` / `go test` of the touched packages).

## Empirical context

The headline numbers are anchored to the heap snapshot summarized in
[`../analysis/master-analysis.md`](../analysis/master-analysis.md) and the
sweep benchmark in [`../benchmarks/sweep/RESULTS.md`](../benchmarks/sweep/RESULTS.md).
Detailed per-patch reasoning, trade-offs, and risks live in
[`../analysis/extended-analysis.md`](../analysis/extended-analysis.md)
sections 3.1–3.4.
