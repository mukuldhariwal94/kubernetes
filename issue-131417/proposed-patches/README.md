# Proposed Patches — Beyond 0001/0002/0003

Four additional optimizations for `ValidatingAdmissionPolicy` memory growth at
100s–10,000+ policies. Stack on top of the patches in the parent directory.

A representative pprof heap profile was used to re-rank priorities — see
[`ANALYSIS.md`](./ANALYSIS.md) §0. **`cel.Program` (specifically its per-program
`FunctionDecl.Bindings` + `Dispatcher`) is ~62% of total heap** in the
compilation subsystem and is the dominant target.

| File | What it does | Heap target |
|---|---|---|
| [`ANALYSIS.md`](./ANALYSIS.md) | Full architectural analysis, root cause, benchmarking plan, success criteria | — |
| [`0004-cel-lazy-varenvs-matrix.patch`](./0004-cel-lazy-varenvs-matrix.patch) | Build the 8-entry `OptionalVariableDeclarations` matrix lazily per compiler | ~6% of heap (subset of `mustBuildEnvs`) |
| [`0005-cel-share-varenvs-matrix-across-policies.patch`](./0005-cel-share-varenvs-matrix-across-policies.patch) | Hoist `mustBuildEnvs` into a process-wide pool keyed by env-template pointer | ~11% of heap |
| [`0006-cel-skip-variables-extend-for-zero-variable-policies.patch`](./0006-cel-skip-variables-extend-for-zero-variable-policies.patch) | Skip the `variables` Extend when `policy.Spec.Variables` is empty | small RSS + meaningful per-request GC churn |
| **Patch D (cel-go side, 0007)** *(design only — not yet drafted as a diff)* | Share `interpreter.Dispatcher` per `*cel.Env` so all programs derived from one env share function-binding storage | **~62% of heap** — directly attacks the dominant `FunctionDecl.Bindings` + `defaultDispatcher.Add` nodes |

## Order of application

Apply in numeric order, on top of `0001`/`0002`/`0003` from the parent directory:

```sh
cd <kubernetes-repo>
git am ../issue-131417/0001-cel-add-process-wide-compiled-program-cache.patch
git am ../issue-131417/0002-cel-share-composited-compilers-across-policies.patch
git am ../issue-131417/0003-cel-add-expression-normalization-utility.patch
git am ../issue-131417/proposed-patches/0004-cel-lazy-varenvs-matrix.patch
git am ../issue-131417/proposed-patches/0005-cel-share-varenvs-matrix-across-policies.patch
git am ../issue-131417/proposed-patches/0006-cel-skip-variables-extend-for-zero-variable-policies.patch
```

The diffs are illustrative — `git am` will reject if the surrounding context
has drifted. Treat the patches as a design specification; the analysis
document spells out the exact intended semantics for each change.

## How they compose

- **0001** dedupes the per-expression `Program` when expression text matches. **Largest k8s-side
  lever.**
- **0002** dedupes the per-policy `*CompositedCompiler` when variable shapes match.
- **0004 (Patch A)** makes the `mustBuildEnvs` matrix sparse instead of fully-materialized.
- **0005 (Patch B)** turns the matrix into a process-wide shared resource, so residual scaling is
  `O(unique_variable_signatures)` instead of `O(N)`.
- **0006 (Patch C)** drops the `variables` Extend for zero-variable policies and removes
  per-request lazy-map allocations on their hot path.
- **Patch D (cel-go)** shares the `interpreter.Dispatcher` per `*cel.Env`. **Largest cel-go-side
  lever** — directly targets the ~62% of heap that 0001 only catches when expression text
  matches.

## Recommended landing order

1. **0001** in k8s/k8s — the biggest k8s-side win, already drafted in the parent directory.
2. **Patch D** in google/cel-go — the biggest absolute win, attacks the dominant heap nodes.
3. **0002 / 0004 / 0005 / 0006** in k8s/k8s — incremental, close the long tail.
4. **0003** as a polish change to widen 0001/0002's cache hit rate.

Together they target a memory model where compilation cost scales with *structural diversity*
rather than *policy count*, which is the only way 10,000-policy clusters become viable on a
single apiserver.
