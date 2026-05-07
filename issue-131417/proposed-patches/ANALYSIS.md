# VAP Memory Optimization — Beyond Patches 0001/0002/0003

> Three additional realistic, upstream-targetable optimizations for
> `ValidatingAdmissionPolicy` memory growth at 100s–10,000+ policies.
> Stacks on top of the existing patches `0001-cel-add-process-wide-compiled-program-cache.patch`,
> `0002-cel-share-composited-compilers-across-policies.patch`,
> `0003-cel-add-expression-normalization-utility.patch` in the parent directory.

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

#### Expected savings

| Before | After |
|---|---|
| 8 × 2 = 16 cel-go envs per compiler | 2 × 2 = 4 (VAP) or 4 × 2 = 8 (MAP) |
| ~150–500 KB per compiler `F` | ~40–125 KB per compiler |

For 10,000 unique-shape policies (where 0002 cannot help), savings on the order of **1–4 GiB**.

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

#### Expected savings

| Workload | Before | After Patch B |
|---|---|---|
| 1,000 policies, 100 unique variable signatures | 1,000 × 26 = 26,000 cel-go envs | 16 + 100 + 1,000 = ~1,116 |
| 10,000 policies, 100 unique signatures | 260,000 envs | ~10,116 |

Pure env-clone savings at 10,000 policies: roughly **2–6 GiB** on top of 0001+0002. Critically, this is the term
that 0002 cannot collapse.

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

#### Expected savings

- Zero-variable policies: skip 2 env clones (~10–40 KB) + reduce 8-env matrix parent chain depth by one. Total
  per-policy: **~20–80 KB** saved.
- Plus per-request: ~1 lazy.MapValue allocation + closure per validation expression saved. At 10,000 policies
  and 1,000 admission RPS, meaningful GC pressure relief.

If 50% of policies are zero-variable in a 10,000-policy cluster: **200 MiB–800 MiB** retained heap saved.

#### Tradeoffs

- **CPU**: marginal improvement.
- **Latency**: per-request slightly faster (skip the lazy map rebuild for zero-var policies).
- **Complexity**: low. One sentinel value in `compositionState`. Two callsite branches.

#### Risks

- The empty-singleton `lazy.MapValue` must be returned by-value or as immutable. Returning a shared mutable map
  across requests would be a correctness bug. Easy to enforce with a wrapper type.

#### Upstream-realistic? **Yes.** Low-risk, high-clarity, narrow blast radius.

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

| Criterion | Target |
|---|---|
| RSS reduction at 1000 policies, templated workload | ≥ 60% (combined 0001+0002+A+B+C) |
| RSS reduction at 1000 policies, distinct-variables workload | ≥ 40% (where Patch B carries the win) |
| RSS overhead at 10000 policies, all-unique adversarial | ≤ 1.5× the 1000-policy figure |
| Admission p99 latency change | within ±2% on micro-bench, ≤ +1 ms on e2e |
| Compile latency per policy | ≤ +10% cold, ≥ −20% warm |
| Allocations/op in `Plugin.Validate` | non-regressing (Patch C should *reduce*) |
| GC cycles/sec under sustained admission load | ≥ 15% reduction at 1000 policies |
| Test suite | 100% pass on existing VAP/MAP integration suites |
| Code complexity | ≤ ~600 lines across the three patches; no public API additions outside `pkg/admission/plugin/cel` |
| Backwards compatibility | zero CRD changes, zero spec validation changes |
| Maintainability | single ownership boundary; cel-go vendor untouched |

A reviewer should be able to read the patch diff, run `go test ./staging/src/k8s.io/apiserver/pkg/admission/...`,
see bench numbers from `benchstat`, and confirm in under an hour that the change is safe.

---

## Closing Thought

The three patches above (Lazy `varEnvs`, Hoisted `mustBuildEnvs`, Skip-`variables`-Extend) target distinct
points in the per-policy fixed cost (`F`) that 0001/0002/0003 don't fully reach. They compose:

- **0001** dedupes the per-expression `C` term.
- **0002** dedupes the per-policy `F` when variable shapes match.
- **A + B + C** drive the `F` term toward its theoretical floor — a single shared 8-env matrix per template,
  minimum work per unique variable shape, and zero cost for zero-variable policies.

Together they make linear scaling in `N` an `O(unique_shapes)` problem in practice — the only way 10,000-policy
clusters become viable on a single apiserver.
