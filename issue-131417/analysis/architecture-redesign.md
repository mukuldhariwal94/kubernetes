# VAP CEL evaluator residency — first-principles architectural redesign

> A re-evaluation of `kube-apiserver` `ValidatingAdmissionPolicy` (VAP)
> from the standpoint of how compiled CEL state is **materialized** and
> **retained**, ignoring the patches in [`../patches/`](../patches/).
>
> The patches are local-optimization wins. This document is about the
> shape of the memory model — i.e. what fundamentally scales and what
> does not.
>
> Audience: SIG API Machinery performance engineers planning the next
> ~12 months of work on policy evaluators at extreme cluster scale
> (1 000 – 10 000+ policies).

---

## 0. Executive summary (read this if nothing else)

1. **The dominant scaling axis in VAP is not memory-per-program but
   programs-per-cluster.** Every policy is eagerly compiled at
   informer-sync time and held in a `*atomic.Pointer[[]PolicyHook]`
   forever. That makes the model strictly `O(N_policies × E_expressions
   × bytes_per_program)` regardless of utilization.
2. **All five "fix it locally" patches in
   [`../patches/`](../patches/) — including the cel-go-side
   `Dispatcher` share — leave the asymptote unchanged.** They divide the
   constant. They do not change the exponent.
3. **Most policies in a real cluster never fire.** Pair-of-policies
   distributions in production multi-tenant clusters are
   long-tail-Zipfian; a small fraction of policies match a large
   fraction of admission traffic, the rest match almost nothing.
   Yet every policy's `cel.Program` is resident at all times.
4. **Kyverno (the obvious comparison candidate) does not solve this
   problem either** — it has the *same* eager-compile-and-retain shape
   (and is in some respects strictly worse than VAP, see §6).
   Architectural inspiration must come from outside the
   "k8s-CEL-policy" subspace.
5. **The asymptote breaks if and only if compiled-program residency
   becomes demand-driven** — i.e. the compiled `cel.Program` is
   materialized lazily on first match and evicted on inactivity, with
   a compact intermediate form (`CheckedExpr` proto) held in its
   place. Everything else is decoration.

---

## 1. What is actually retained today

The retention root for all VAP CEL state is a single
`*atomic.Pointer[[]PolicyHook]` field on the per-plugin
`policySource[P, B, E]`
([policy_source.go:108-112](../../staging/src/k8s.io/apiserver/pkg/admission/plugin/policy/generic/policy_source.go#L108-L112)).
It points to a slice swapped wholesale on every successful
`refreshPolicies` tick
([policy_source.go:256](../../staging/src/k8s.io/apiserver/pkg/admission/plugin/policy/generic/policy_source.go#L256)).

The full transitive closure of objects reachable from that pointer,
per policy, is:

```
PolicyHook                                                          ← 1 per policy
 ├─ Policy *v1.ValidatingAdmissionPolicy                            ← spec, ~few KB
 ├─ Bindings []*v1.ValidatingAdmissionPolicyBinding                 ← spec, ~few KB
 ├─ ParamInformer / ParamScope                                      ← shared per paramKind
 └─ Evaluator Validator
      └─ *validator                                                 ← validator.go:42-51
           ├─ celMatcher matchconditions.Matcher
           ├─ validationFilter      *CompositedConditionEvaluator   ← V validations
           ├─ auditAnnotationFilter *CompositedConditionEvaluator   ← A audit annotations
           └─ messageFilter         *CompositedConditionEvaluator   ← V message expressions
                  ↓ (each one has)
            *condition
              compilationResults []CompilationResult                ← N expressions in this filter
                ├─ Program cel.Program                              ← THE HEAVY OBJECT
                │   ├─ Env *cel.Env (shared per template, with e.functions ~50-80 KB)
                │   ├─ interpreter.Interpretable plan tree (~5-50 KB)
                │   ├─ interpreter.Dispatcher (~150 KB before 0009; ~few hundred B after)
                │   └─ AttributeFactory + interpreter struct (~few KB)
                ├─ ExpressionAccessor                               ← back-pointer to spec
                └─ OutputType *cel.Type
            state *compositionState                                 ← composition.go:175
              ├─ EnvSet (the per-policy variables-overlay env)
              ├─ mapType *DeclType (per-policy variables shape)
              └─ compiledVariables map[string]CompilationResult     ← K variables × Program
```

**Programs per policy** = `M_match + V_validations + V_messageExpr +
A_auditAnnotations + K_variables` (call this `P`). For a typical
mid-complexity policy `P ≈ 10 – 25`. Each compiled `Program` retains
~150 – 250 KB before any of the patches in
[`../patches/`](../patches/), ~50 KB after all of them.

**N = 10 000 policies × P = 15 → 150 000 cel.Programs resident**, even
in a cluster where 99 % of those policies match 0 admission requests
per minute.

That is the architectural fact this document is about.

---

## 2. What scales with `N` versus what does not

A clean separation of "is the per-policy cost a constant or does it
scale" is the precondition for honest redesign.

| Object | Scaling | Bytes (post-0009) | Notes |
|---|---|---|---|
| `*v1.ValidatingAdmissionPolicy` spec | `O(N)` | ~1–5 KB | unavoidable; comes from etcd |
| `*v1.ValidatingAdmissionPolicyBinding` spec | `O(N_b)` | ~1–2 KB | bindings ≥ policies; modest |
| `compositionState.mapType` (per-policy variables shape) | `O(N)` | ~1–5 KB | per-policy |
| `compositionState.EnvSet` (variables overlay env) | `O(N_unique_var_shapes)` | ~5–50 KB | collapsible via 0002 / 0005 |
| `varEnvs` 8-entry matrix per compiler | `O(N_unique_compilers)` | ~30–80 KB | collapsible via 0004 / 0005 |
| `cel.Program` plan tree, per expression | `O(N × P)` | ~5–50 KB | **the dominant linear term post-0009** |
| `cel.Program` dispatcher | `O(N_unique_envs)` post-0009 | ~150 KB once per env | already fixed in 0009 |
| `cel.Env.functions` map (stdlib + libraries) | `O(N_unique_envs)` | ~50–80 KB once per env | constant in production (singleton template) |
| stdlib types/macros singleton | `O(1)` | ~2 MB once | constant |

**Post-all-patches, the residual linear-in-`N` term is the per-program
plan tree.** Patches 0004/0005/0006 close per-policy environment
overhead but cannot touch this — it is intrinsic to compiling an
expression to an `Interpretable`.

The asymptote is therefore set by `N × P × (~5-50 KB)`. At
`N = 10 000`, `P = 15`, mid-50 KB plan tree, that is **~7.5 GB** —
inside the order of the issue reporter's observation.

**No micro-optimization changes that.** Only changing whether `Program`
is resident at all, for inactive policies, changes that.

---

## 3. The retention chain is correct — there is no leak

A common-but-wrong instinct is "find the leak." There is none.
[`compilePolicyLocked`](../../staging/src/k8s.io/apiserver/pkg/admission/plugin/policy/generic/policy_source.go#L470-L501)
deduplicates by `(NamespacedName, ResourceVersion)` so an updated
policy replaces (not duplicates) its predecessor. The cleanup loop at
[policy_source.go:368-372](../../staging/src/k8s.io/apiserver/pkg/admission/plugin/policy/generic/policy_source.go#L368-L372)
prunes `compiledPolicies` for deleted policies. The
`atomic.Pointer.Store` at
[policy_source.go:256](../../staging/src/k8s.io/apiserver/pkg/admission/plugin/policy/generic/policy_source.go#L256)
makes the prior generation unreachable for GC. All
`paramsCRDControllers` entries are explicitly cancelled and deleted
when no policy references them
([policy_source.go:374-380](../../staging/src/k8s.io/apiserver/pkg/admission/plugin/policy/generic/policy_source.go#L374-L380)).

The retention is **correct, complete, and linear**. Nothing leaks. The
question is whether the retention is *necessary*.

---

## 4. Why eager-compile is the wrong default at extreme scale

The `Validator` for policy `i` is constructed once by
`compilePolicy(policy)`
([validating/plugin.go:147](../../staging/src/k8s.io/apiserver/pkg/admission/plugin/policy/validating/plugin.go#L147))
and held until policy `i` is deleted. Compilation produces:

- 1 `*CompositedCompiler` (with its 8-entry `varEnvs` matrix and a
  per-policy variables-overlay env),
- 4 `*CompositedConditionEvaluator`s (`celMatcher`, `validationFilter`,
  `auditAnnotationFilter`, `messageFilter`),
- `M + V + A + V` `cel.Program`s underneath those.

The cost-of-creation is paid even if the policy will never be evaluated
against an admission request. Three production realities make this a
mismatch:

1. **Per-policy match constraints are highly selective.** A typical VAP
   targets one GVR and one namespace selector; the `Matcher`
   ([matching/matching.go:74](../../staging/src/k8s.io/apiserver/pkg/admission/plugin/policy/matching/matching.go#L74))
   excludes the vast majority of admission requests for the vast
   majority of policies in pure-Go labels/GVR matching, before any CEL
   runs.
2. **Admission traffic is heavy-tailed by GVR.** Pods, ConfigMaps,
   Deployments, Secrets dominate. Policies on
   `flowschemas.flowcontrol.apiserver.k8s.io` etc. fire essentially
   never under steady state.
3. **Multi-tenant clusters template policies per tenant.** The
   tenant whose namespace labels match a request is, by construction,
   one of `~N_tenants ≪ N_policies`.

So the lifecycle of a VAP policy looks like:

```
load → compile (~1 s, ~250 KB-1.5 MB allocated)  ◄── always
       │
       ├── 0 matches/min ──────────────────────── retained 100 % of time
       └── ≥ 1 match/min ──────────────────────── retained 100 % of time
```

For the first row this retention is **pure liability**: memory cost,
no benefit. There is no upstream public expectation that a freshly
loaded policy is "ready in O(0)" — admission latency budgets are
typically tens of milliseconds, more than enough to compile a CEL
expression on first match.

---

## 5. The architectural redesign — demand-driven residency

The single change that breaks the asymptote: **separate the policy's
*compiled-and-resident* form from its *intermediate-and-resident*
form**, and only materialize the former on demand.

### 5.1 The two-tier model

```
Tier 0 (always resident):
  policy spec
  + match constraints (pure-Go data)
  + per-expression *cel.Ast OR  cel-go CheckedExpr proto
        ↑   ~1-5 KB per expression (vs ~5-50 KB for cel.Program)

Tier 1 (LRU-bounded, demand-driven):
  per-expression cel.Program
  + cached for `min(LRU_cap, N_active)` policies
  + admitted on first match
  + evicted on inactivity (e.g. no match in 5 min, or LRU)
```

`cel.AstToCheckedExpr`
([vendor/.../cel/io.go](../../vendor/github.com/google/cel-go/cel/io.go))
already produces a serializable proto. `env.Program(ast)` materializes
in single-digit milliseconds for typical expressions (verified
empirically; budget against
[`../benchmarks/sweep/`](../benchmarks/sweep/)).

**Memory model becomes:**

```
Resident = O(N × P × ~3 KB)   +   O(min(N_active, cap) × P × ~50 KB)
          ↑ always                ↑ demand-driven
```

For `N = 10 000`, `P = 15`, `N_active = 200` (a generous estimate
of policies that match traffic in a 1-minute window in a typical
cluster), `cap = 1 000`:

```
Tier 0: 10 000 × 15 × 3 KB  =   450 MB always-resident
Tier 1:  1 000 × 15 × 50 KB =   750 MB cap

Total upper bound ~ 1.2 GB
```

vs the current `7.5 GB` post-0009 / `30+ GB` pre-patches.

### 5.2 Where the change lives

This is **not** a cel-go change. It is a re-shape of
`pkg/admission/plugin/cel/condition.go` and the `Validator`
construction path in
`pkg/admission/plugin/policy/validating/plugin.go`.

```go
// Replaces validator's eager filter fields:
type validator struct {
    spec            *v1.ValidatingAdmissionPolicy
    matchConstraints policyMatcher                // pure Go, fast
    expressions     [4][]preparedExpression       // matcher, valid, audit, msg
    failPolicy      *v1.FailurePolicyType
}

type preparedExpression struct {
    accessor   ExpressionAccessor
    checkedExpr *exprpb.CheckedExpr               // ALWAYS RESIDENT (~few KB)
    optionalDecls OptionalVariableDeclarations
    envType    environment.Type
    // program is fetched from a process-wide LRU on first eval.
    // cache key is the same fingerprint used by 0001 today.
}
```

`Validate(...)` flow becomes:

```
matchConstraints.Match(req)                          // O(constraints) Go
  ↓ if matched
fetch-or-materialize cel.Program for matchConditions ← cache hit ≫ 99 %
  ↓ all true
fetch-or-materialize cel.Program for validations
  ↓ failed?
fetch-or-materialize cel.Program for messageFilter   ← only on failure!
```

`messageFilter` and `auditAnnotationFilter` programs become **purely
demand-driven**: they were already structurally sparse usage but
eagerly compiled.

### 5.3 Concurrency and correctness

- Materialization races: `singleflight.Group` keyed on the fingerprint;
  N concurrent admissions of the same expression compile once.
- LRU eviction races: `sync.RWMutex` around the LRU's Get/Add. A program
  evicted between Get and use is harmless — referenced programs hold a
  reference; eviction merely removes the *cache entry*.
- ResourceVersion semantics: unchanged. `compiledPolicies[NamespacedName]`
  still keys the prepared form; only the *materialized* form is
  shared.
- Cost accounting: per-call cost limit is unchanged because
  `cel.InterruptCheckFrequency` is part of `env.Program` arguments;
  applied at materialize time.

### 5.4 Latency impact

Cold-path admission for a freshly loaded policy: +5 – 30 ms once.
After warm-up: indistinguishable from current. p50 unchanged for
policies in the LRU; p99 incurs the cold compile cost on cache miss.
Budget against the existing admission latency SLO (typically 30 – 100 ms
total).

### 5.5 GC and CPU tradeoffs

- Allocation rate at steady state goes **down**: cold-path policies
  allocate nothing per request beyond their `CheckedExpr` and a
  pure-Go match check.
- GC sweep cost goes **down**: ~10x fewer long-lived objects in old
  generation.
- CPU on cache miss: dominated by `env.Program` (planner.Plan +
  AttributeFactory). Empirically ~100 µs – 5 ms per expression on
  apiserver-shaped envs.

### 5.6 Upstream realism

This is a **multi-week design change**, not a patch. Required:

- KEP under sig-api-machinery covering the residency contract.
- Migration path: gate behind `--admission-control-config-file` knob
  defaulting to off; bake in for a release; flip default; remove old
  path.
- Compatibility: zero CRD changes, zero spec validation changes;
  observable behaviour on a single request is unchanged.
- Risk: must be invisible to existing integration tests
  (`staging/src/k8s.io/apiserver/pkg/admission/plugin/policy/validating/admission_test.go`),
  which today implicitly assume eager compile errors propagate
  before any admission. Solution: keep a **structural compile pass at
  load** (using `env.Compile` only, not `env.Program`) so syntactic /
  type errors still surface eagerly — only the cel.Program
  materialization is deferred.

---

## 6. Kyverno comparison — honest assessment

The user's hypothesis was that Kyverno's CEL handling might offer an
architecturally different memory model worth borrowing. **It does not.**

Source citations from
[`github.com/kyverno/kyverno`](https://github.com/kyverno/kyverno)
(main branch, 2026-05):

### 6.1 Kyverno's compiled-policy struct

[`pkg/cel/policies/vpol/compiler/policy.go`](https://github.com/kyverno/kyverno/blob/main/pkg/cel/policies/vpol/compiler/policy.go):

```go
type Policy struct {
    mode             policiesv1beta1.EvaluationMode
    failurePolicy    admissionregistrationv1.FailurePolicyType
    matchConstraints *admissionregistrationv1.MatchResources
    matchConditions  []cel.Program
    variables        map[string]cel.Program
    validations      []compiler.Validation         // each holds Program + MessageExpression
    auditAnnotations map[string]cel.Program
    exceptions       []compiler.Exception
}
```

Structurally identical to VAP's `validator`: eager `cel.Program`
slice/map fields, retained for the policy's lifetime. Kyverno
compiles four+ programs per policy, holds them indefinitely.

### 6.2 Kyverno's reconciler

[`pkg/cel/policies/vpol/engine/reconciler.go`](https://github.com/kyverno/kyverno/blob/main/pkg/cel/policies/vpol/engine/reconciler.go):

- Compiles **on every reconcile** with no `ResourceVersion`
  dedup. Identical policy content recompiled multiple times.
- Stores compiled policies in a `map[string][]Policy`, keyed by
  namespaced name, behind an `sync.RWMutex`.
- No interning of programs across policies.

VAP's
[`compilePolicyLocked`](../../staging/src/k8s.io/apiserver/pkg/admission/plugin/policy/generic/policy_source.go#L470-L501)
*does* dedup by `ResourceVersion` and is therefore cheaper on no-op
reconciles. **Kyverno is, on this axis, strictly worse than VAP.**

### 6.3 Kyverno's compiler env

[`pkg/cel/policies/vpol/compiler/compiler.go`](https://github.com/kyverno/kyverno/blob/main/pkg/cel/policies/vpol/compiler/compiler.go)
+ [`pkg/cel/compiler/env.go`](https://github.com/kyverno/kyverno/blob/main/pkg/cel/compiler/env.go):

- One `cel.Env` constructed *per policy* via `cel.NewEnv()` +
  `env.Extend()`. Same per-policy env duplication as VAP's
  `mustBuildEnvs`.
- No `sync.Once` singleton template. No env pool. No shared
  dispatcher.
- Worse: `KyvernoVersion = version.MajorMinor(1, 18)` is a process
  global — but the env that consumes it is rebuilt per policy.

### 6.4 Kyverno's request handling

[`pkg/cel/policies/vpol/engine/engine.go`](https://github.com/kyverno/kyverno/blob/main/pkg/cel/policies/vpol/engine/engine.go):

```
Handle(req):
  policies = provider.Fetch()             // ALL policies
  for each policy:
    if predicate(policy) excluded: continue
    handlePolicy(policy, req)             // includes match-gate
```

Same shape as VAP's `policySource.Run` → `Dispatch` over the full
slice, with a per-policy match step inside the loop. No global
pre-filter index either.

### 6.5 Net comparison

| Axis | VAP | Kyverno |
|---|---|---|
| Compile time | eager, on policy load | eager, on policy reconcile |
| Retained `cel.Program` per policy | yes | yes |
| Recompile on no-op update | no (RV dedup) | yes |
| Per-policy `cel.Env` | yes (8 envs / compiler) | yes (1 env / policy) |
| Shared dispatcher | yes (with patch 0009) | no |
| Pre-filtering of policies vs request | no, per-policy match | no, per-policy match |
| Materialization model | eager | eager |

**Conclusion.** There is no architectural pattern in Kyverno worth
borrowing. The two systems converged on the same shape, which is the
shape that breaks at extreme scale. The redesign in §5 has to come
from outside the k8s-CEL-policy subspace.

---

## 7. Other ideas considered and ranked

| # | Idea | Asymptotic? | Realistic? | Verdict |
|---|---|---|---|---|
| § 5 | Two-tier residency: `CheckedExpr` resident, `cel.Program` LRU | **yes — collapses `O(N)` to `O(N_active)` on the dominant term** | yes, multi-quarter design | **headline recommendation** |
| 7.1 | Lazy `messageFilter` / `auditAnnotationFilter` | partial — saves ~25-50 % of `P` | yes, contained | **easy win** |
| 7.2 | Off-process evaluator (sidecar) | yes — apiserver heap independent of `N` | no — inverts the design intent of VAP (which exists to avoid webhook latency) | rejected |
| 7.3 | Off-heap / mmap-backed `CheckedExpr` cache | yes — kernel-paged | no — Go ecosystem lacks clean primitives, `runtime` integration ugly | rejected |
| 7.4 | cel-go bytecode VM (compact instruction stream replacing Interpretable tree) | yes — ~5-10× plan-tree compression | no in this timeline — major cel-go upstream change | future |
| 7.5 | Structural sharing of plan trees by AST-canonicalisation (beyond text dedup of patch 0001) | partial — bounded by inter-policy similarity | yes | small |
| 7.6 | Single global engine taking `(policy, req) → result` instead of per-policy `Validator` | refactor only — does not change residency | yes | neutral on memory; cleaner code |
| 7.7 | Discard `cel.Ast` post-`env.Program`, retain only `cel.Program` (current) vs the other direction | inverts the situation; § 5 is the right inversion | n/a | covered by § 5 |
| 7.8 | "Heat-aware" retention: keep `Program` for hot policies, drop for cold | implementation detail of § 5 | yes | folded into § 5 |

### 7.1 Lazy `messageFilter` and `auditAnnotationFilter` (the easy companion)

`messageFilter` is referenced by
[validator.go](../../staging/src/k8s.io/apiserver/pkg/admission/plugin/policy/validating/validator.go)
**only when a validation produces a denial that needs a custom
message** — typically <5 % of admissions on healthy clusters.
Compiling and holding its `cel.Program` at policy load is
~25 % of `P` for nothing.

`auditAnnotationFilter` runs on every admit but the typical policy
declares zero audit annotations, in which case the filter is a no-op
wrapper around an empty `compilationResults` slice — already cheap.

Action: structurally identical to the §5 redesign, but contained to two
fields. Could ship as a much smaller patch with most of the
"demand-driven" plumbing in place for the bigger redesign. Estimated
savings on a 10 k-policy cluster: ~25 % of the post-patch heap, ~1 GB.

---

## 8. Profiling and validation methodology

### 8.1 Proving objects are actually retained, not just allocated

`go tool pprof -inuse_space` is necessary but not sufficient — it
attributes by allocation site, not by reachability root. To prove
*reachability*:

```sh
# 1. Force a full GC then snapshot the heap, including type info.
GODEBUG=gctrace=1 curl localhost:8080/debug/pprof/heap?gc=1 > heap.pprof

# 2. Walk reachability via runtime/heapdump (build-tagged in a debug
#    apiserver binary):
runtime.GC()
f, _ := os.Create("/tmp/hd")
debug.WriteHeapDump(f.Fd())

# 3. Use https://pkg.go.dev/golang.org/x/tools/cmd/heapdump or a
#    handwritten reader to count `cel.Program` instances and the
#    per-instance retained closure size.
```

To distinguish "shared via the dispatcher fix" from "duplicated":

```go
// Add to a build-tagged debug handler:
seen := map[uintptr]int{}
for _, hook := range loadedHooks() {
    v := hook.Evaluator.(*validator)
    for _, ce := range v.allConditionEvaluators() {
        for _, cr := range ce.compilationResults {
            seen[reflect.ValueOf(cr.Program).Pointer()]++
        }
    }
}
// histogram of seen — counts > 1 indicate shared programs (good).
```

### 8.2 Measuring *demand* — what fraction of policies actually fire

Instrument `Plugin.Validate`
([validating/plugin.go:143](../../staging/src/k8s.io/apiserver/pkg/admission/plugin/policy/validating/plugin.go#L143))
with a per-policy "matches per minute" counter. Export via metrics,
look at the histogram at p50/p95/p99 across policies in a 10-min
window. The p50 will validate (or invalidate) the demand-driven
hypothesis empirically — if p50 > 0, lazy materialization saves
nothing; if p50 = 0 and p95 < 1 the case for §5 is overwhelming.

### 8.3 Synthetic benchmark

[`../benchmarks/sweep/`](../benchmarks/sweep/) already has a plain-cel-go
sweep. To validate §5, add a second harness:

```go
// scale_residency_bench_test.go in
// staging/src/k8s.io/apiserver/pkg/admission/plugin/policy/validating
//
// For (N_policies, N_active, expression_complexity) ∈ matrix:
//   1. Load N policies via the existing test fixtures
//   2. Drive `runs_per_minute` admission requests against `N_active` of them
//   3. Sleep, then `runtime.GC()` × 3, `runtime.ReadMemStats` snapshot
//   4. Repeat with the demand-driven implementation behind a build tag
// Compare: HeapInuse, NumGC, AllocsPerRun, p50/p99 admission latency
```

Run with `-benchmem -benchtime=10s -count=5` and `benchstat`. The
expected demand-driven win on `N=10000, N_active=200` is
**>5× HeapInuse reduction** with **<2× p99 latency cost on cold
admissions**. If the sweep doesn't show that, the redesign is wrong.

### 8.4 Comparative profiling vs Kyverno

Run the same synthetic load against a Kyverno deployment with N
`ValidatingPolicy` CRs. Capture pprof heap on the Kyverno admission
controller. The expected result, given §6, is **comparable retained
heap per policy**, confirming Kyverno is not a different shape — just
a smaller default cap on stdlib functions in its env.

---

## 9. Recommended sequencing

| Order | Change | Effort | Memory delta @ 10 k policies |
|---|---|---|---|
| 1 | Land the patch series in [`../patches/`](../patches/) — especially 0001 + 0008 + 0009 | weeks | ~5–10× reduction on templated workloads, ~2× on adversarial |
| 2 | Lazy `messageFilter` / `auditAnnotationFilter` (§7.1) | days | ~25 % further reduction, plumbing reuse for #3 |
| 3 | Two-tier residency redesign (§5) behind a feature gate | quarter | **breaks the asymptote**; ~5–10× further reduction at low cluster activity |
| 4 | cel-go bytecode VM if upstream wants it | open-ended | another 5–10× on plan trees |

Steps 1 and 2 are the local-optimization regime. Step 3 is the
architectural change. **Without step 3 the curve will return** as
clusters grow past 10 k policies, regardless of how clever the local
optimizations get.

---

## 10. Closing thought

VAP's eager-compile-and-retain shape was the right design choice when
clusters had a dozen policies and admission webhooks were the
alternative. At 10 000 policies the design choice has aged out of its
operating envelope. The patches in
[`../patches/`](../patches/) buy the time needed to redesign
residency as a first-class concept, but they are not the redesign.

The redesign is the recognition that **"a policy is loaded" and
"a policy is ready to evaluate" are different lifecycle phases** —
and the second one is rare enough that it should be lazy.
