# Proposed Patches — Beyond 0001/0002/0003

Three additional optimizations for `ValidatingAdmissionPolicy` memory growth at
100s–10,000+ policies. Stack on top of the patches in the parent directory.

| File | What it does | Targets |
|---|---|---|
| [`ANALYSIS.md`](./ANALYSIS.md) | Full architectural analysis, root cause, benchmarking plan, success criteria | — |
| [`0004-cel-lazy-varenvs-matrix.patch`](./0004-cel-lazy-varenvs-matrix.patch) | Build the 8-entry `OptionalVariableDeclarations` matrix lazily per compiler | per-compiler `F` cost |
| [`0005-cel-share-varenvs-matrix-across-policies.patch`](./0005-cel-share-varenvs-matrix-across-policies.patch) | Hoist `mustBuildEnvs` into a process-wide pool keyed by env-template pointer | residual `F` cost when 0002 misses |
| [`0006-cel-skip-variables-extend-for-zero-variable-policies.patch`](./0006-cel-skip-variables-extend-for-zero-variable-policies.patch) | Skip the `variables` Extend when `policy.Spec.Variables` is empty | zero-variable policies + per-request GC churn |

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

- **0001** dedupes the per-expression `C` term (compiled `cel.Program`).
- **0002** dedupes the per-policy `F` term when variable shapes match.
- **0004** makes the `F` term sparse instead of fully-materialized — saves
  ~75% on the 0002-miss path even per-compiler.
- **0005** turns the `F` term into a process-wide shared resource, so the
  residual scaling is `O(unique_variable_signatures)` instead of `O(N)`.
- **0006** drops `F` to ~zero for zero-variable policies and removes per-request
  lazy-map allocations on their hot path.

Together they target a memory model where compilation cost scales with
*structural diversity* rather than *policy count*, which is the only way
10,000-policy clusters become viable on a single apiserver.
