# cel-go memory-saving API audit

> Survey of every public cel-go API option that affects the in-memory
> footprint of a compiled `cel.Program` or its `Env` / interpreter state,
> evaluated against the `kube-apiserver` admission-plugin compile path.
>
> Source: [`vendor/github.com/google/cel-go/`](../../../vendor/github.com/google/cel-go/),
> consumers under
> [`staging/src/k8s.io/apiserver/pkg/cel/`](../../../staging/src/k8s.io/apiserver/pkg/cel/) and
> [`staging/src/k8s.io/apiserver/pkg/admission/plugin/cel/`](../../../staging/src/k8s.io/apiserver/pkg/admission/plugin/cel/).
>
> Heap context: pprof profiles for issue-131417 attribute ~62 % of retained
> compilation-subsystem heap to `FunctionDecl.Bindings` +
> `interpreter.defaultDispatcher.Add`, plus a long tail in per-`Env.Extend`
> clones. This audit looks for **additional** levers beyond the patch
> series in [`../patches/`](../patches/).

---

## Category 1 — High-impact unused options (easy wins)

### 1.1 `cel.EvalOption` flags (`cel/options.go:626-656`)

| Flag | Effect | Status in apiserver | Action |
|---|---|---|---|
| `OptTrackState` | Retains per-eval `EvalState` in `Result.Details`. | **NOT used.** | Confirm it is never set anywhere in the admission plugin. |
| `OptExhaustiveEval` | Disables short-circuit; forces state tracking. | **NOT used.** | Same as above. |
| `OptPartialEval` | Switches `AttributeFactory` → `PartialAttributeFactory` (~2–5 KB extra per Program). | **NOT used.** | Confirm validator expressions never need partial eval. |
| `OptTrackCost` | Spawns per-eval `CostTracker` with `overloadTrackers` map + stack. | **USED** via `cel.InterruptCheckFrequency` and runtime cost limits. | Required for cost enforcement; cannot disable without losing safety guarantees. |
| `OptOptimize` | Enables constant folding + (with `OptimizeRegex`) regex pre-compilation at `Program` creation. | **NOT used.** | **Recommendation: enable.** Moves cost from per-eval to per-Program; reduces eval-time allocations 5–15 % for constant-heavy expressions. |

### 1.2 `cel.Globals` (`cel/options.go:430-442`)

Sets `p.defaultVars`, wraps every Eval with a `HierarchicalActivation` (~200–400 B per eval). Apiserver does not pass globals — neutral.

---

## Category 2 — Retention in Env / Program structures

### 2.1 `Env.macros` (`cel/options.go:200-208`, stdlib `library.go:160-200`)

Every `Env.Extend()` copies the full macros slice (~15–20 stdlib macros).
~2–4 KB per env, multiplied by the 8 variant envs that
`mustBuildEnvs` creates per compiler. Total ~16–32 KB per compiler. A
`StdLibSubset()` could prune unused comprehensions
(`all`/`exists`/`map`/`filter`) but the savings are bounded.

### 2.2 `Env.functions` map (`cel/options.go:343-347`, `library.go:160-200`)

The full stdlib function map: 100+ overloads, ~50–80 KB per env after
copy. **This is the same data the per-program dispatcher copy is built
from** — patch [0009](../patches/0009-celgo-share-dispatcher/) shares the
populated dispatcher across programs of an env. The env-side function
map itself remains; an analogous "share `e.functions` between parent and
child unless mutated" change in cel-go would close the long tail but
needs CoW machinery to be safe.

### 2.3 Checker env retention (`cel/env.go` chk fields)

`Env` lazily caches a `checker.Env` (`chk` field, populated on first
`Check()`) holding the validated declarations and scope chain. ~5–10 KB
per env. **Apiserver does not currently set
`cel.EagerlyValidateDeclarations(true)`**
(`cel/options.go:167-177`) on the base env. Enabling it would pre-build
the checker.Env once on the singleton template and reuse the validated
declarations across every `Extend()` (8 per compiler), saving ~1 KB per
extend × 8 envs/compiler × N compilers.

### 2.4 `types.Registry.Copy()` (`common/types/provider.go:140-150`)

`Env.Extend` deep-copies provider/adapter when they implement
`*types.Registry` (the default). 10–50 KB per copy depending on
registered types. Apiserver uses the singleton template for production;
the copy happens once per `mustBuildEnvs` matrix. A
`CustomTypeProvider`/`CustomTypeAdapter` option that shares a read-only
provider would skip the copy entirely — feasible because none of the
admission types are mutated post-registration.

---

## Category 3 — Interpreter / runtime state

### 3.1 `CostTracker` + `overloadTrackers` (`interpreter/runtimecost.go:257-266`)

Per-eval CostTracker creates an `overloadTrackers` map only if the
caller registers per-overload `FunctionTracker`s. Apiserver does not.
Default `ActualCostEstimator` is sufficient. Confirm the admission cost
options never register per-overload trackers and document.

### 3.2 `InterruptCheckFrequency` (`cel/options.go:670-675`)

Apiserver uses this — `cel.InterruptCheckFrequency(celconfig.CheckFrequency)`
in `compile.go:303`. `prog.go:388-396` already reuses an `activation`
pool, so the per-Eval cost is amortised. No change needed.

### 3.3 `OptimizeRegex` (`cel/options.go:447-452`)

Compiles `matches()` regex constants at `Program` creation rather than
eval time. **Apiserver does not enable this.** Adding
`cel.EvalOptions(cel.OptOptimize) + cel.OptimizeRegex(cel.MatchesRegexOptimization)`
to the production `env.Program(ast, …)` call in
[`compile.go:302-304`](../../../staging/src/k8s.io/apiserver/pkg/admission/plugin/cel/compile.go#L302-L304)
would shift regex compilation off the hot path. Estimated 100–500 B
per eval saved on policies that use `matches()`.

### 3.4 Decorator / Interpretable plan tree (`cel/options.go:405-410`,
`interpreter/decorators.go`)

Plan-tree size dominates the residual per-program cost after 0009 lands
(~5 MB / 1.19 % in the reference profile). No cel-go option reduces
this without changing semantics.

---

## Category 4 — AST / type artifacts

### 4.1 `EagerlyValidateDeclarations(true)` (`cel/options.go:167-177`)

**Highest-leverage flag the apiserver does not set.** On the base env,
this triggers `featureEagerlyValidateDeclarations` which pre-validates
declarations at `NewEnv` and reuses the result for every `Extend()`. With
8 `Extend()` calls per compiler × thousands of compilers, this saves
~1 KB per extend in the warm path. Easy: add to
`environment.MustBaseEnvSet` construction.

### 4.2 `cel.AstToCheckedExpr` (`cel/io.go`)

`Ast` retains the parsed/typed tree. There is no public API to drop the
`Ast` post-`Program`. The interpretable plan tree is what the program
actually executes; the `Ast` is held only for diagnostics and re-emit.
Not currently a hotspot.

---

## Category 5 — Lazy/deferred Program materialization

There is **no public cel-go API** to retain only a `CheckedExpr`
(serialized proto) and lazily materialize a `cel.Program`. This pattern
would let cold expressions sit in proto form (~few KB) and pay
`newProgram` cost only on first eval — useful for VAPs that match no
admission requests for hours. Implementing it requires either a custom
caching layer above cel-go or a cel-go upstream change.

---

## Top recommendations (ranked)

| # | Action | Magnitude | Effort |
|---|---|---|---|
| 1 | Enable `cel.EagerlyValidateDeclarations(true)` on the base env in `apiserver/pkg/cel/environment` | 5–10 % env-build heap, ~8 KB per template | trivial |
| 2 | Add `cel.EvalOptions(cel.OptOptimize)` + `cel.OptimizeRegex(cel.MatchesRegexOptimization)` to the production `env.Program(...)` call | 5–15 % per-eval allocation drop on regex-using policies | trivial |
| 3 | Audit and prune `StdLib` macros / functions never used by VAP/MAP via `StdLibSubset()` | 2–5 % env heap; ~500 B–2 KB per env | low (requires usage audit) |
| 4 | Confirm `OptTrackState` / `OptExhaustiveEval` / `OptPartialEval` are never set anywhere; document the contract in `compile.go` | 10–15 % per-eval if accidentally set | trivial (docs) |
| 5 | Use a read-only `CustomTypeProvider` so `Env.Extend` skips `Registry.Copy()` | ~10–50 KB per extend × matrix size | medium |
| 6 | (Future) cel-go upstream: share `e.functions` (and not just dispatcher) parent→child via CoW | closes the long tail after 0009 | high — cel-go-side |

Items #1 and #2 are low-risk one-line changes that stack cleanly with
the patch series in [`../patches/`](../patches/). Items #3–#5 are
follow-ups for after the headline patches land.
