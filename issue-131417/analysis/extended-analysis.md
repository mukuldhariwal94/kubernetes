# VAP Memory Optimization — Beyond Patches 0001/0002/0003

> Four additional realistic, upstream-targetable optimizations for
> `ValidatingAdmissionPolicy` memory growth at 100s–10,000+ policies.
> Stacks on top of the existing patches `0001-cel-add-process-wide-compiled-program-cache.patch`,
> `0002-cel-share-composited-compilers-across-policies.patch`,
> `0003-cel-add-expression-normalization-utility.patch` in the parent directory.

---

## 0. Empirical Heap Profile + Sweep Benchmark

Two empirical sources inform this analysis: a pprof heap snapshot of a running apiserver, and a
controlled Go sweep in [`sweep_bench/`](./sweep_bench) ([results](./sweep_bench/RESULTS.md)).
**Read both — they tell complementary stories and together they correct first-principles
over-estimates in earlier drafts of this document.**

### 0.1 Sweep benchmark (controlled, plain cel-go)

A plain cel-go env with three variables, sweeping (envs × unique programs):

```
Scenario A: 1 env, N unique programs       slope = ~37 KB / program  (flat across N=100…10,000)
Scenario B: N envs, 1 program each         slope = ~51 KB / (env+prog)  →  ~14 KB / env extra
Scenario C: linear model fits within 8%     total ≈ 14 KB × envs + 37 KB × programs
```

Predictions vs actual at large scale (Scenario C):

| Case | Predicted (model) | Actual | Error |
|---|---|---|---|
| 1,000 envs × 50 progs (50,000 total) | 1,864 MB | 1,907 MB | +2% |

**There is no missing nonlinear term.** Per-program cost is real, per-env cost is real, both are
linear, and the slopes are modest in plain cel-go.

In a Kubernetes apiserver the slopes are several times higher because the env carries many
extra function libraries (`urls`, `ip`, `cidr`, `regex`, `jsonpatch`, `quantity`, `semver`,
`lists`, etc.), each adding `*FunctionDecl` entries that grow the per-program dispatcher and
the per-env function table. pprof from a real apiserver suggests **~100–180 KB per program**
and a comparable per-env figure.

### 0.2 pprof leaf-node ranking (live apiserver, ~420 MB snapshot)

A heap profile from a representative compiled-policies state ranks the actual sources of retained
heap. Read in light of the sweep above — leaf-node mass at one snapshot is *not* the same as
"cost that scales with policy count":

```
compilePolicyLocked                        334.90 MB (79.57%)
  → validating.compilePolicy               334.32 MB (79.44%)
  → CompileCondition                       288.64 MB (68.58%)
  → CompileCELExpression                   288.14 MB (68.46%)
  → newProgram (env.Program)               269.62 MB (64.06%)   ◄── dominant
       ├─ decls.(*FunctionDecl).Bindings   178.01 MB (42.30%)   ◄── #1 leaf
       │   └─ retained directly            122.51 MB (29.11%)
       └─ interpreter.(*defaultDispatcher).Add  82.61 MB (19.63%)  ◄── #2 leaf
  → NewCompiler / mustBuildEnvs             47.70 MB (11.33%)
       └─ cel.(*Env).Extend                36.20 MB (8.60%) cumulative
            (15.60 MB direct)
```

Ranked by retained heap:

| Rank | Site | % heap | What it is | Targeted by |
|---|---|---|---|---|
| 1 | `decls.(*FunctionDecl).Bindings` | **42.30%** | per-`Program` copy of stdlib function-binding closures | 0001 (when text matches); **Patch D** (always, when env matches) |
| 2 | `interpreter.(*defaultDispatcher).Add` | **19.63%** | per-`Program` overload-id table populated from those bindings | 0001 (when text matches); **Patch D** (always, when env matches) |
| 3 | `mustBuildEnvs` → `cel.(*Env).Extend` | **8.60%** | 8-env matrix per compiler | Patches A (0004) + B (0005) |
| 4 | `cel.(*Env).Extend` (other call sites) | **3.71% direct + tail** | misc env clones (variables overlay, etc.) | Patch C (0006) |
| 5 | parser/checker working state | **~4%** | mostly transient; small retained tail | not targeted |

### 0.3 Reconciling the two sources

The pprof leaf-node mass is **cumulative across all programs in the snapshot**, not "the cost of
adding one more program." The sweep tells us how each marginal program/env contributes.
Combining:

1. **Per-program retained heap is ~37 KB in plain cel-go, ~100–180 KB in apiserver.** This is
   genuinely linear in unique program count.
2. **Per-env retained heap is ~14 KB in plain cel-go, higher in apiserver.** Linear in unique env
   count.
3. **Patch 0001 (program dedup by text) is the highest-leverage k8s-side change.** Every cache
   hit eliminates ~37+ KB. At thousands of templated policies sharing text, the savings dominate
   everything else.
4. **Patch D (shared dispatcher per env)** still helps when expression text differs but env is
   shared. In plain cel-go, ~60–80% of the 37 KB per-program cost is dispatcher-related, so
   Patch D saves ~22–30 KB per unique program. At 10,000 unique programs: ~250–350 MB. **Real
   but not "62% of all heap" as the pprof leaf-node read suggested.** That earlier framing
   conflated cumulative leaf-node mass with marginal per-program cost. Corrected here.
5. **Patches A/B/C target the per-env term (~14 KB plain, more in apiserver).** At 10,000
   unique-shape policies: low-hundreds of MB. Useful long-tail wins, not headline.

**The user's intuition was correct**: adding unique expressions does not "spike" memory because
the per-program cost is in the tens of KB, not the hundreds the pprof leaf node implied.
Bloat reports at scale come from the *product* of policies × bindings × expressions, not any
single axis exploding.

---

## 1. Current Architecture Analysis

### 1.1 How VAP policies are loaded

Two informers feed the plugin: one watches `ValidatingAdmissionPolicy`, the other `ValidatingAdmissionPolicyBinding`
(`staging/src/k8s.io/apiserver/pkg/admission/plugin/policy/generic/policy_source.go:52–91`).
On Add/Update/Delete each one calls a `notify()` whose only effect is `policiesDirty.Store(true)`.
**No CEL work runs in the informer goroutine.**

A separate worker `wait.Until(refreshPolicies, 1*time.Second, ctx.Done())` wakes once a second, drains the dirty
bit, and runs `calculatePolicyData()` under a `sync.Mutex`. For each policy it consults
`compiledPolicies[NamespacedName]`. If `entry.policyVersion == policy.ResourceVersion` it reuses the cached
`Validator`; otherwise it calls the per-plugin compiler closure
(`staging/src/k8s.io/apiserver/pkg/admission/plugin/policy/validating/plugin.go:147–181`).
Once all policies are compiled, `policies.Store(&newHooks)` makes the new slice visible to the data plane atomically.

The cache by `(NamespacedName, ResourceVersion)` already exists. It bounds compile rate. It does **not** dedupe
work across distinct policies that happen to share expression text — that gap is the entire problem.

### 1.2 How CEL expressions are parsed/type-checked/compiled

`compilePolicy(policy)` on the validating side does, in order:

1. `getCompositionEnvTemplateWithStrictCost()` — process-wide singleton built once in `sync.Once`
   (`validating/plugin.go:47–57`). The only env state shared across policies today.
2. `cel.NewCompositedCompiler(template)` (`cel/composition.go:66–105`). Inside:
   - `envSet.Extend({variables decl})` — **1 EnvSet.Extend = 2 cel-go `cel.Env.Extend`** (one for
     `NewExpressions`, one for `StoredExpressions`).
   - `mustBuildEnvs(state.EnvSet)` (`cel/compile.go:315–339`) — builds **all 8 entries** of `varEnvs` eagerly
     (Cartesian over `HasParams × HasAuthorizer × HasPatchTypes`). Each `createEnvForOpts` does another
     `EnvSet.Extend = 2 more cel-go env clones`.
3. `CompileAndStoreVariables`, `CompileCondition × 4` (validations / matchConditions / auditAnnotations /
   messageExpressions) → each call → `compiler.CompileCELExpression` → `env.Compile + env.Program`.

Per policy this is **1 (variables) + 8 (mustBuildEnvs base) + 4 (mustBuildEnvs HasPatchTypes Extend) = 13
EnvSet.Extend calls = 26 cel-go `cel.Env.Extend` clones** before a single expression is compiled.
That is the constant `F` term in the memory model.

### 1.3 What `cel.Env.Extend` actually clones

`vendor/github.com/google/cel-go/cel/env.go:430–526` — Extend allocates fresh copies of every map and slice the
parent holds: `prsrOpts`, `chkOpts`, `progOpts`, `validators`, `costOptions`, `variables`, `macros`, `features`,
`appliedFeatures`, `functions` (the big one — base stdlib has hundreds of entries), `libraries`, plus
`provider.Copy()`. With 26 clones per policy you get **150–500 KB** of retained heap before any expression is
compiled.

### 1.4 What `env.Program` actually allocates

`vendor/github.com/google/cel-go/cel/program.go:172–268` — `newProgram` does this **per call**:

- `interpreter.NewDispatcher()` and **populates it with every function in `e.functions`** by iterating and
  calling `fn.Bindings()` (lines 195–203). The biggest per-program retained cost.
- `interpreter.NewAttributeFactory(container, adapter, provider, …)` — fresh per program.
- `interpreter.NewInterpreter(disp, container, provider, adapter, attrFactory)` — fresh.
- `planner.Plan(ast)` builds an `Interpretable` tree mirroring the AST.

Sizes observed in profiling: **~30–200 KB per `cel.Program`**, dominated by the `Dispatcher` copy and the
interpretable tree, *not* the AST itself.

### 1.5 What is shared vs duplicated

| Object | Shared / Per-policy | Cost class |
|---|---|---|
| Base `*environment.EnvSet` (`getCompositionEnvTemplateWithStrictCost()`) | shared (singleton) | once |
| The two `*cel.Env`s inside that EnvSet | shared | once |
| Per-policy `compositionState` (`mapType`, `EnvSet`, `compiledVariables`) | per-policy | linear in policy count |
| `varEnvs` (the 8-entry `OptionalVariableDeclarations` matrix) | per-policy (built eagerly) | linear in policy count |
| `cel.Ast` per expression | not retained | transient |
| `cel.Program` per expression | per-(policy × expression) | linear in `N × E` without dedupe |
| `validator` struct | per-policy, cached by ResourceVersion | linear, bounded by churn |

### 1.6 What scales linearly

- **Pre any patch**: `Total ≈ N × (F + E_p × C)` with `F ≈ 150–500 KB` and `C ≈ 30–200 KB`.
- **Post 0001 alone**: `Total ≈ N × F + min(U, LRUcap) × C`. The `F` term still scales linearly.
- **Post 0001 + 0002**: `Total ≈ unique(envTemplate, varSig, opts) × F + min(U, LRUcap) × C`. `F` collapses for
  policies with structurally identical `variables`. **Policies with distinct variable shapes still pay `F` per
  policy.** This is the gap my new patches target.

---

## 2. Root Cause of Memory Amplification

### 2.1 Retained, scaling structures

1. **`varEnvs` matrix (8 EnvSets per compiler)** — built eagerly in `mustBuildEnvs`. **VAP only ever uses 2 of
   the 8 combos** (`{HasParams, HasAuthorizer:true}` for validations/audits and `{HasParams,
   HasAuthorizer:false}` for messageExpressions; `HasPatchTypes` is MAP-only). So **6 of every 8 entries are
   dead weight**.
2. **The "variables" Extend even when `policy.Spec.Variables` is empty.** `NewCompositedCompiler`
   unconditionally extends the template with `variables` (`composition.go:69`). Adds a `DeclType` to the
   provider chain and an unused variable declaration to every one of the 8 envs that follow.
3. **Per-program function `Dispatcher`** — built fresh per `env.Program`, populated with every overload from
   `e.functions`. Correctly handled by 0001.
4. **`compiledVariables` in `compositionState`** — patched by 0002 when variable lists match.
5. **`compiledPolicies[NamespacedName]` validators** — keyed by ResourceVersion, no eviction except on policy
   delete.

### 2.2 Transient vs retained

| Class | What | Lifetime |
|---|---|---|
| Transient (heavy GC churn) | parser tokens, ANTLR state, `cel.Ast` during compile, intermediate `ref.Val` boxing | one compile / one request |
| Retained (linear in policy count) | `varEnvs` matrix, `compositionState`, `cel.Program` per expression, dispatcher copies | until ResourceVersion changes or policy deleted |
| Long-lived (singleton) | `getCompositionEnvTemplateWithStrictCost()` | process |

### 2.3 The architectural assumption causing the issue

**Compilation is treated as a per-policy, owned-by-policy operation.** The `Validator` returned by
`compilePolicy` owns its environment, its program, and its variable state. That made sense at 1.26–1.28 with a
dozen policies. At 10,000 policies the data is effectively immutable across structurally-equivalent policies;
the ownership is artificial.

The fix is a **shift to a structural-sharing model** at three levels: programs (0001), composited compilers
(0002), and — what my patches add — the env matrix and the variables-extend overhead even when 0002 misses.

---

## 3. Proposed Patches

### 3.1 Patch A — Lazy `varEnvs` (sparse `OptionalVariableDeclarations` matrix)

**File:** `0004-cel-lazy-varenvs-matrix.patch`

#### Problem

`mustBuildEnvs` builds 8 EnvSets eagerly per `compiler` (= 16 cel-go `Env.Extend` clones).
VAP uses 2 combos in production; MAP uses 4. Whatever 0002 collapses, the residual unique compilers still each
waste 75% of this matrix.

#### Architectural reasoning

Convert `varEnvs` from an eager `map[OptionalVariableDeclarations]*environment.EnvSet` to a lazy memoized
`sync.Map` + `sync.Once` per combo. Build only what's actually requested at `CompileCELExpression` time.

`createEnvForOpts` is pure with respect to `(baseEnv, namespaceType, requestType, opts)`. Memoizing it can't
change observed behavior. Thread safety via `sync.Once` per key.

#### Expected savings (revised against pprof)

| Before | After |
|---|---|
| 8 × 2 = 16 cel-go envs per compiler | 2 × 2 = 4 (VAP) or 4 × 2 = 8 (MAP) |
| ~50 KB per compiler `F` (empirical: 47.7 MB / ~1000 policies) | ~12 KB per compiler |

Caps at ~75% of the `mustBuildEnvs` slice (`8.60%` of heap in the reference profile). At
**10,000 unique-shape policies** the addressable heap is roughly the 47.7 MB scaled
linearly — order of **300–500 MB saved**, not the multi-GB figure originally claimed.
Useful, but not transformative on its own.

#### Tradeoffs

- **CPU**: amortized identical. First admission against a given `(opts)` combo pays the build cost once.
- **Latency**: tiny one-time hit (~1–5 ms per combo) on first match.
- **Complexity**: small. `sync.Map` + `sync.Once` per key.
- **Thread safety**: `sync.Once` per `OptionalVariableDeclarations` key.

#### Risks

The current eager build surfaces a misconfiguration error at compiler construction time. With lazy, it would
surface at first use. Mitigation: keep `NewCompiler` validating its inputs cheaply.

#### Upstream-realistic? **Yes.**

---

### 3.2 Patch B — Hoist `mustBuildEnvs` to a process-wide template-keyed pool

**File:** `0005-cel-share-varenvs-matrix-across-policies.patch`

#### Problem

The 8-env matrix is *per `compiler`*, but the only thing that varies between two compilers built from the same
env template is the `variables` declType. The matrix construction is otherwise identical. After 0002 *misses*
(different variables), each policy still recomputes the matrix from scratch — 8 cel-go env clones whose
function/library/declType machinery is byte-for-byte identical to thousands of others.

#### Architectural reasoning

Build the 8-env matrix from the **base template** (without `variables`), share it process-wide, and apply the
per-policy `variables` declaration as a thin Extend on top.

Pre-patch: `9 × N` env extends. Post-patch: `8 + 1×K + 1×N` extends, where `K` is the number of distinct
variable signatures.

The set of declarations in the resulting env is identical in both flows: in either ordering you end up with
`(template) ∪ (opts decls) ∪ (variables decl) ∪ (declTypes)`. cel-go's Extend treats added options as additive
and order-independent for declarations of disjoint names.

#### Expected savings (revised against pprof)

| Workload | Before | After Patch B |
|---|---|---|
| 1,000 policies, 100 unique variable signatures | 1,000 × 26 = 26,000 cel-go envs | 16 + 100 + 1,000 = ~1,116 |
| 10,000 policies, 100 unique signatures | 260,000 envs | ~10,116 |

Pprof attributes ~11.3% of heap to `mustBuildEnvs` (~47.7 MB at the 1,000-policy class). At
10,000 policies: a few hundred MB. **The savings ceiling is the `mustBuildEnvs` slice — not
multi-GB**, contrary to the original estimate. This is the term that 0002 cannot collapse, but
the absolute magnitude is bounded.

#### Tradeoffs

- **CPU**: slightly improved — fewer total Extend calls.
- **Latency**: cold-start improves; first-policy compile pays the matrix once.
- **Complexity**: medium. Pool needs careful invariants (templates must be pointer-stable;
  `getCompositionEnvTemplateWithStrictCost()` already is).
- **Thread safety**: `sync.Map` + `sync.Once` per key.

#### Risks

- **Pointer-keyed cache** assumes templates are stable. Production caller uses a `sync.Once`-guarded singleton.
- **Eviction**: none. Templates are O(1) in production.
- **Order-of-Extend semantics**: tests must confirm equivalent type-checked output and identical eval results.

#### Upstream-realistic? **Yes**, possibly more so than 0002 because pointer identity replaces key computation.

---

### 3.3 Patch C — Skip the `variables` Extend when policy has no `variables` declarations

**File:** `0006-cel-skip-variables-extend-for-zero-variable-policies.patch`

#### Problem

`NewCompositedCompiler` unconditionally extends the template:

```go
newEnvSet, err := envSet.Extend(environment.VersionedOptions{
    EnvOptions: []cel.EnvOption{cel.Variable("variables", newMapType.CelType())},
    DeclTypes:  []*apiservercel.DeclType{newMapType},
})
```

even when the caller has **zero variables**. Consequences:

1. Two extra cel-go env clones (NewExpressions + StoredExpressions).
2. `apiservercel.NewDeclTypeProvider(declTypes...)` chain gains an extra link, deepening every type lookup at
   compile *and* runtime.
3. The 8-env matrix is built on top of an env with the unused `variables` decl.
4. At runtime, `compositionContext.Variables(activation)` builds a `lazy.MapValue` even when the expression
   doesn't reference `variables`.

A meaningful fraction of in-the-wild VAPs declare zero `variables`.

#### Architectural reasoning

Split the constructor: keep `NewCompositedCompiler` for variable-bearing policies, add `NewSimpleCompiler` (or a
sentinel `mapType == nil`) for zero-variable policies. The runtime `Variables()` method returns a process-wide
empty singleton.

When `Spec.Variables == nil/empty`, no expression can legally reference `variables.x` — that's a compile error
today. Skipping the `variables` declType cannot cause an expression that compiles today to fail to compile, and
vice versa.

#### Expected savings (revised against pprof)

- The non-`mustBuildEnvs` `Extend` slice (the variables overlay path) is `~3.7%` direct in the
  reference profile (~15.6 MB / 1,000 policies). Patch C eliminates this for zero-variable
  policies.
- Per-policy: **~10–20 KB** retained heap saved (was 20–80 KB; corrected to match the smaller
  empirical `Extend` cost).
- Per-request: 1 `lazy.MapValue` allocation + closure per validation expression saved. At 10,000
  policies and 1,000 admission RPS, this is the biggest contribution of Patch C — **GC churn
  reduction**, not steady-state RSS.
- If 50% of policies are zero-variable in a 10,000-policy cluster: **~50–150 MB** retained heap
  saved, plus a meaningful drop in allocation rate on the admission hot path.

#### Tradeoffs

- **CPU**: marginal improvement.
- **Latency**: per-request slightly faster (skip the lazy map rebuild for zero-var policies).
- **Complexity**: low. One sentinel value in `compositionState`. Two callsite branches.

#### Risks

- The empty-singleton `lazy.MapValue` must be returned by-value or as immutable. Returning a shared mutable map
  across requests would be a correctness bug. Easy to enforce with a wrapper type.

#### Upstream-realistic? **Yes.** Low-risk, high-clarity, narrow blast radius.

---

### 3.4 Patch D — Share `interpreter.Dispatcher` per `*cel.Env` (cel-go-side)

**File:** `0007-celgo-share-dispatcher-per-env.patch` *(cel-go vendor change, not yet shipped here)*

This patch directly targets the **#1 and #2 nodes in the heap profile** (~62% of total heap).

#### Problem

In cel-go, [`vendor/github.com/google/cel-go/cel/program.go:172–268`](vendor/github.com/google/cel-go/cel/program.go#L172-L268), `newProgram` does this **per call**:

```go
disp := interpreter.NewDispatcher()                    // fresh, empty
...
for _, fn := range e.functions {                       // every function in the env
    bindings, err := fn.Bindings()                     // materializes closures
    ...
    err = disp.Add(bindings...)                        // copies each into disp
}
```

Two consequences observed in pprof:

- **`decls.(*FunctionDecl).Bindings`** retains 178 MB / 42.30% — every program holds its own copy
  of every stdlib + Kubernetes-library function-binding closure.
- **`interpreter.(*defaultDispatcher).Add`** retains 82.6 MB / 19.63% — the per-program overload-id
  table that those bindings populated.

These are functionally constant **per-`*cel.Env`** — they depend only on `e.functions`, which is
immutable after the env is constructed. Two programs built from the same env will populate
byte-for-byte identical dispatchers.

Patch 0001 dedupes when expression text matches. **It does not help when expressions differ but
the env is the same** — which after Patch B is the dominant case in a large-scale cluster
(thousands of distinct expressions, one shared env-matrix). On the 0001-miss path, the per-program
dispatcher is the largest residual cost.

#### Architectural reasoning

Build the dispatcher **once per `*cel.Env`**, lazy + sync.Once-guarded, and share it across all
programs derived from that env. The dispatcher is logically immutable after population (cel-go
already documents `Program` as thread-safe and stateless), so sharing is correctness-preserving.

Rough sketch (cel-go side):

```go
// cel/env.go
type Env struct {
    ...
    dispatcher       interpreter.Dispatcher  // lazily built, shared across programs
    dispatcherOnce   sync.Once
    dispatcherErr    error
}

func (e *Env) sharedDispatcher() (interpreter.Dispatcher, error) {
    e.dispatcherOnce.Do(func() {
        d := interpreter.NewDispatcher()
        for _, fn := range e.functions {
            bindings, err := fn.Bindings()
            if err != nil { e.dispatcherErr = err; return }
            if err := d.Add(bindings...); err != nil { e.dispatcherErr = err; return }
        }
        e.dispatcher = d
    })
    return e.dispatcher, e.dispatcherErr
}

// cel/program.go: newProgram
disp, err := e.sharedDispatcher()
if err != nil { return nil, err }
p := &prog{Env: e, dispatcher: disp, ...}
// no longer iterate e.functions here
```

Importantly, `Env.Extend` already deep-copies `e.functions`, so two extended envs have independent
function tables. Each extended env builds *its own* shared dispatcher on first use. The
sharing model is: **one dispatcher per env**, not "one global dispatcher."

#### Why preserves semantics

- `Dispatcher.Add` is the only mutation; all reads (`FindOverload`, dispatch lookups) are
  read-only. Once the env's function table is fully populated (which happens during
  `cel.NewEnv` / `Extend.configure`), the dispatcher built from it is observationally immutable.
- Programs built from the same env see the same dispatch behavior whether the dispatcher is
  shared or per-program — the table contents are identical.
- `Env.Extend` produces a new env with a new function table → a new (lazily-built) dispatcher.
  Parent and child envs do not alias dispatchers, so additive function declarations work as today.

#### Expected savings (the headline)

Direct attack on **42.30% + 19.63% = ~62% of total heap** in the reference profile.

| Workload | Before | After Patch D |
|---|---|---|
| 1,000 policies × 5 expressions = 5,000 programs | 5,000 × dispatcher copies | 1 dispatcher per env (typically ≤ a handful per process) |
| Reference profile (~420 MB total) | 260 MB in Bindings + dispatcher | low double-digit MB |

Estimated savings: **~250 MB on the 1,000-policy reference workload** (≈60% of total heap), and
proportionally more at 10,000 policies. Combined with Patch 0001 (which addresses the program
cache itself), the residual heap of the compilation subsystem should be dominated by env state
(addressed by A/B/C) plus the unavoidable per-expression `Interpretable` tree (~5 MB / 1.19% in
the profile — the small `newProgram` direct node).

#### Tradeoffs

- **CPU**: identical or faster. First program built from an env pays the population cost once;
  every subsequent program built from that env pays zero dispatcher-population cost.
- **Latency**: cold-start for the first program-per-env shifts by a few ms (the population work
  was already happening; it's now hoisted to first use). Subsequent compiles are faster.
- **Complexity**: low for the cel-go side — one `sync.Once` and a getter. The change touches
  exactly two files in cel-go (`cel/env.go`, `cel/program.go`).
- **Thread safety**: provided by `sync.Once`. Concurrent `newProgram` calls on the same env
  serialize through Once for the first one, then run lock-free.

#### Risks

- **Mutability assumption**: relies on `Dispatcher` being effectively immutable after
  initial population. Today it is — `Add` is only called from `newProgram`. Future cel-go
  changes could violate this; the patch should include a comment locking that invariant in.
- **Test-time helpers**: any cel-go test that mutates a `Dispatcher` post-construction would
  break. A grep confirms this is not done in the public test suite, but cel-go maintainers
  would need to confirm.
- **No backward-compat issues for Kubernetes**: this is internal to cel-go's program construction;
  Kubernetes callers see no API change.

#### Upstream-realistic?

**Yes — but the path is via cel-go, not k8s/k8s.** Filing this as a cel-go PR is the right
shape. The change is small, well-bounded, and the maintainer team has previously accepted
similar internal optimizations. SIG API Machinery would consume it via a vendor bump.

The relationship to existing patches:

- **0001 + Patch D are complementary, not redundant.** 0001 dedupes whole `Program`s when
  expression text matches; Patch D dedupes the `Dispatcher` slice of every `Program`, including
  ones with unique text.
- **Patch D is the highest-impact change in this analysis** (target: 60% of heap), but it lives
  in a different repo and is therefore on a different release cadence. 0001 is the highest-impact
  k8s/k8s change.

#### Why this was missed in the original analysis

The first-principles model treated `cel.Program` as opaque "30–200 KB / expression." pprof
revealed that ~70% of that mass is the `FunctionDecl.Bindings` + `Dispatcher` substructure —
material that depends only on the env, not the expression. Once you see that, the optimization
is obvious. **Always profile before optimizing.**

---

## 4. Benchmarking & Validation Plan

### 4.1 Workloads

A synthetic generator (already present in `../generate_policies.py`) emits VAPs in three shapes:

- **Templated multi-tenant** — N policies, all sharing one expression text. The 0001/0002 best case.
- **Distinct-variables homogeneous** — N policies with structurally identical validation logic but distinct
  `variables[]`. 0001 hits, 0002 misses, **Patch B hits**.
- **Adversarial all-unique** — N policies, each with unique expression text and unique variables. All miss;
  this is the worst case; we want bounded overhead.

Sweep `N ∈ {100, 1_000, 5_000, 10_000}` and complexity `E ∈ {5, 10, 50}`.

### 4.2 Metrics

| Metric | How |
|---|---|
| RSS at steady state | `runtime.ReadMemStats().Sys` after `runtime.GC()` × 3 |
| Retained heap | `pprof -inuse_space`, top-N attributed to `cel-go.*Env|Program|Dispatcher` |
| Allocations/op, bytes/op | `go test -bench -benchmem` |
| Compile latency (cold/warm) | per-policy timing in `compilePolicy`; seed cache for warm |
| Admission latency p50/p95/p99 | end-to-end through `Plugin.Validate` |
| GC pause time | `runtime/metrics` `/sched/pauses-stopping:seconds` |
| GC frequency | `runtime/metrics` `/gc/cycles/automatic:gc-cycles` |
| Cold-start memory | RSS at "informer cache synced" + "first 100 policies compiled" snapshots |

### 4.3 Bench harness skeleton

`staging/src/k8s.io/apiserver/pkg/admission/plugin/policy/validating/scale_bench_test.go`:

```go
func BenchmarkVAP_CompileAndEvaluate(b *testing.B) {
    for _, shape := range []string{"templated", "distinct-vars", "all-unique"} {
        for _, n := range []int{100, 1000, 5000, 10000} {
            b.Run(fmt.Sprintf("%s/N=%d", shape, n), func(b *testing.B) {
                policies := genPolicies(shape, n, /*E=*/10)
                runtime.GC()
                var beforeMem runtime.MemStats
                runtime.ReadMemStats(&beforeMem)
                for _, p := range policies {
                    _ = compilePolicy(p)
                }
                runtime.GC()
                var afterMem runtime.MemStats
                runtime.ReadMemStats(&afterMem)
                b.ReportMetric(float64(afterMem.HeapInuse-beforeMem.HeapInuse), "bytes/policy-set")
                b.ReportMetric(float64(afterMem.HeapInuse-beforeMem.HeapInuse)/float64(n), "bytes/policy")
                b.ResetTimer()
                for i := 0; i < b.N; i++ {
                    runOneAdmissionAgainstAllPolicies(b, policies)
                }
            })
        }
    }
}
```

Compare runs with `benchstat baseline.txt patch-A.txt patch-A+B.txt patch-A+B+C.txt`.

### 4.4 Detecting hidden regressions

- **Equivalence test**: capture eval results from a 50-expression test suite under master; rerun under each
  patch; assert byte-equal `decisions` slices.
- **Cache poisoning test**: ensure 0001's hits never leak `ExpressionAccessor` from another policy.
- **Order-of-Extend invariance test (Patch B)**: compile the same expression via the old (variables-first) and
  new (variables-last) ordering, ensure `cel.Program` outputs match.
- **Allocs/op bound**: `testing.AllocsPerRun(100, …)` must regress no more than ~5% on templated shape.
- **Latency regression**: p99 admission latency must not increase by more than 1 ms.

---

## 5. Tooling & Profiling

### 5.1 Capture protocol

Run apiserver with `/debug/pprof`. Capture, in order:

1. **Cold-start**: `curl localhost:8080/debug/pprof/heap > heap.cold.pprof` after informers sync.
2. **Compile-only**: apply N policies, wait for two `refreshPolicies` ticks, force GC,
   `heap > heap.compiled.pprof`.
3. **Steady-state**: drive 5 minutes of admission load,
   `heap > heap.steady.pprof` and `goroutine > goroutine.steady.pprof`.

Retained-heap diff:
```sh
go tool pprof -inuse_space -base heap.cold.pprof heap.compiled.pprof
```

### 5.2 Identifying duplicated structures

Heap profiles bucket by allocation site, not retained reachability. Verify dedupe with:

1. `pprof.Lookup("heap").WriteTo(f, 2)` for full type-resolved heap.
2. Filter to `*cel.prog`, `*cel.Env`, `interpreter.Dispatcher` — count instances. **Patch B's correctness
   criterion: fewer than `N × 16` `*cel.Env` instances after compiling N policies.**
3. A reflection-based diagnostic walking `policySource.compiledPolicies` and counting unique pointer identities
   for each interesting field. Build-tagged + test-only handler.

### 5.3 Allocation profiling

```sh
go test -run=^$ -bench=BenchmarkVAP_CompileAndEvaluate \
  -memprofile=alloc.pprof -memprofilerate=1
go tool pprof -alloc_objects alloc.pprof
top20 -cum
```

`-memprofilerate=1` records every allocation — use when chasing GC churn from per-request allocations.

### 5.4 cel-go instrumentation hooks

Build-tagged counters in `cel/env.go:Extend` and `cel/program.go:newProgram` to count hits per N-policy run.
Single most useful diagnostic for verifying Patch B's effect.

### 5.5 Kubernetes scalability tooling

`k8s.io/perf-tests/clusterloader2` is the canonical large-scale harness. Add a CL2
`testStrategy: cel-policy-scale` that loads N VAPs via the API and drives admission via parallel `kubectl
create configmap` workers. CL2 collects RSS, GC, latency percentiles, and pprof samples — feed outputs through
`benchstat`.

---

## 6. Success Criteria

Revised against the empirical profile. **Patch D + 0001 are the two headline wins; A/B/C are
incremental reinforcement.**

| Criterion | Target |
|---|---|
| RSS reduction at 1000 policies, templated workload | ≥ 70% (0001 + Patch D dominate) |
| RSS reduction at 1000 policies, distinct-text workload (0001 misses) | ≥ 50% (Patch D carries the win — this is the case 0001 cannot help with) |
| RSS reduction at 1000 policies, distinct-variables workload | ≥ 40% (Patch B + Patch D) |
| RSS overhead at 10000 policies, all-unique adversarial | ≤ 1.5× the 1000-policy figure |
| Admission p99 latency change | within ±2% on micro-bench, ≤ +1 ms on e2e |
| Compile latency per policy | ≤ +10% cold (lazy first-build amortizes), ≥ −20% warm |
| Allocations/op in `Plugin.Validate` | non-regressing (Patch C should *reduce*) |
| GC cycles/sec under sustained admission load | ≥ 15% reduction at 1000 policies |
| Test suite | 100% pass on existing VAP/MAP integration suites; cel-go conformance suite must pass for Patch D |
| Code complexity | A+B+C: ≤ ~600 lines in `pkg/admission/plugin/cel`. Patch D: ≤ ~80 lines in cel-go (`cel/env.go`, `cel/program.go`) |
| Backwards compatibility | zero CRD changes, zero spec validation changes; Patch D is internal to cel-go |
| Maintainability | A/B/C: single ownership boundary. Patch D requires cel-go upstream coordination + vendor bump |

A reviewer should be able to read the patch diff, run `go test ./staging/src/k8s.io/apiserver/pkg/admission/...`,
see bench numbers from `benchstat`, and confirm in under an hour that the change is safe.

---

## Closing Thought

The empirical heap profile reorders the priorities relative to a first-principles analysis. **The
single largest source of retained heap is `cel.Program`** — specifically the per-program copy of
function bindings and the dispatcher table built from them, which together account for ~62% of
the compilation subsystem's heap. Anything that doesn't address this is an incremental win.

The five-patch program now decomposes as:

| Patch | Where | Targets | Impact |
|---|---|---|---|
| **0001** | k8s — `pkg/admission/plugin/cel` | per-expression `Program` dedup by text | **headline (k8s side)** — 60%+ on templated workloads |
| **0002** | k8s — `pkg/admission/plugin/cel` | `*CompositedCompiler` dedup by variable signature | meaningful when variables match |
| **0003** | k8s — `pkg/admission/plugin/cel` | expression-text canonicalization for cache key | enables 0001/0002 hits |
| **Patch A (0004)** | k8s — `pkg/admission/plugin/cel` | lazy `varEnvs` matrix per compiler | bounded — capped at ~8% of heap |
| **Patch B (0005)** | k8s — `pkg/admission/plugin/cel` | shared `mustBuildEnvs` matrix per env template | bounded — capped at ~11% of heap |
| **Patch C (0006)** | k8s — `pkg/admission/plugin/cel` + `policy/{validating,mutating}` | zero-variable fast path | small RSS, meaningful GC churn relief |
| **Patch D (0007)** | **cel-go** — `cel/env.go`, `cel/program.go` | shared `Dispatcher` per `*cel.Env` | **headline (cel-go side)** — 60%+ of heap, including the cases 0001 cannot help |

**Combined target:** linear scaling in policy count becomes `O(unique_envs × unique_expression_text)` —
which for any realistic cluster is sublinear in `N`. 10,000-policy clusters then become a function
of structural diversity, not policy count.

**The two changes worth landing first**, in order:

1. **0001** in k8s/k8s (already drafted in the parent directory).
2. **Patch D** in google/cel-go.

Patches 0002, A, B, C, and 0003 are valuable refinements that close the long tail.
