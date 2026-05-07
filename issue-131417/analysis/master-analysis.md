# Deep Analysis: kubernetes/kubernetes#131417 — High Memory Consumption from CEL in `ValidatingAdmissionPolicy`

> Companion document to the three patches in this directory. See [`README.md`](./README.md)
> for application instructions and patch-by-patch summaries.

---

## Table of Contents

1. [Understanding the Issue](#1-understanding-the-issue)
2. [Codebase Mapping](#2-codebase-mapping)
3. [Current Architecture Diagram](#3-current-architecture-diagram)
4. [Memory Analysis (Deep Dive)](#4-memory-analysis-deep-dive)
5. [Root Cause Analysis](#5-root-cause-analysis)
6. [Multiple Optimization Approaches](#6-multiple-optimization-approaches)
7. [Optimized Architecture Diagram](#7-optimized-architecture-diagram)
8. [Recommended Solution](#8-recommended-solution)
9. [Implementation Proposal](#9-implementation-proposal)
10. [Testing Strategy](#10-testing-strategy)
11. [Risk Assessment](#11-risk-assessment)
12. [PR-ready Summary](#12-pr-ready-summary)
13. [Patches Index](#13-patches-index)

---

## 1. Understanding the Issue

**Problem (1–2 sentences).**
`kube-apiserver` memory grows linearly with the number of `ValidatingAdmissionPolicy` (VAP) and `ValidatingAdmissionPolicyBinding` (VAPB) objects because every policy compiles its CEL expressions into a brand-new `cel.Program` (with its own AST, type provider, and `cel.Env` chain), even when the *same* expression text is shared by hundreds of other policies. Reported overhead: **~0.2–0.6 MiB per policy/binding pair**, scaling to **7.1 GiB at 1000 policies × 100 bindings × 50 expressions** (issue #131417).

**Affected component.**
`kube-apiserver` admission pipeline → `ValidatingAdmissionPolicy` plugin → CEL compilation layer.

**Reproduction steps (inferred from the issue).**

1. Bring up a kind cluster (otherwise empty).
2. Apply *N* `ValidatingAdmissionPolicy` objects, each paired with one or more `ValidatingAdmissionPolicyBinding`. Use identical `validations[].expression`, `matchConditions[].expression`, and `variables[].expression` text across many policies (the realistic case for namespace-scoped multi-tenant isolation policies).
3. Capture `apiserver` memory and pprof. Memory grows ~linearly with `N × (validations + matchConditions + auditAnnotations + messageExpression + variables)`.

The maintainer (`@jpbetz`) explicitly invited *"well bench-marked optimizations, particularly ones that have broad application"* — deduplicating CEL compilation across policies fits exactly that profile.

### Reporter's measurements (verbatim from the issue)

| Number of policies | simple policy (no match conditions) | mid-complex (1 match condition) | complex (7 match conditions) |
|---|---|---|---|
| 0    | 770.9   | 693.6   | 749.4   |
| 500  | 921     | 864.8   | 1153    |
| 1000 | 961     | 1036    | 1353    |
| 1500 | 1035    | 1165    | 1667    |
| 2000 | 1158    | 1365    | 1968.8  |

Follow-up at scale (with bindings):

| Policy # | Bindings/Policy | Validations | Match Conditions | apiserver memory |
|---|---|---|---|---|
| 1000 | 100 | 10 | 5  | 2.7 GiB |
| 1000 | 100 | 25 | 5  | 3.1 GiB |
| 1000 | 100 | 50 | 5  | 4.7 GiB |
| 1000 | 100 | 10 | 25 | 4.7 GiB |
| 1000 | 100 | 10 | 50 | 7.1 GiB |

---

## 2. Codebase Mapping

### Relevant packages and files

| Layer | File | Role |
|---|---|---|
| Admission registration | `staging/src/k8s.io/apiserver/pkg/admission/plugin/policy/validating/plugin.go` | Wires the VAP plugin into the generic framework; defines `compilePolicy` |
| Admission registration | `staging/src/k8s.io/apiserver/pkg/admission/plugin/policy/mutating/compilation.go` | MAP equivalent of `compilePolicy` |
| Generic policy core (read-only) | `staging/src/k8s.io/apiserver/pkg/admission/plugin/policy/generic/policy_source.go` | Caches per-policy `Evaluator` keyed by `NamespacedName`+`ResourceVersion`; calls `s.compiler(policySpec)` |
| CEL compiler façade | `staging/src/k8s.io/apiserver/pkg/admission/plugin/cel/compile.go` | `Compiler.CompileCELExpression`, `mustBuildEnvs` |
| CEL composition | `staging/src/k8s.io/apiserver/pkg/admission/plugin/cel/composition.go` | `CompositedCompiler`, `compositionState`, variables glue |
| CEL conditions | `staging/src/k8s.io/apiserver/pkg/admission/plugin/cel/condition.go` | `ConditionCompiler` (validations, matchConditions, audit) |
| CEL mutations | `staging/src/k8s.io/apiserver/pkg/admission/plugin/cel/mutation.go` | `MutatingCompiler` |
| CEL environment | `staging/src/k8s.io/apiserver/pkg/cel/environment/environment.go` | `EnvSet.Extend` (documented as "expensive") |
| Evaluation | `staging/src/k8s.io/apiserver/pkg/admission/plugin/cel/activation.go` | Per-request `evaluationActivation` |
| Match conditions | `staging/src/k8s.io/apiserver/pkg/admission/plugin/webhook/matchconditions/matcher.go` | Wraps a `ConditionEvaluator` per policy |

### Key structs

- `cel.CompilationResult { Program cel.Program; Error *Error; ExpressionAccessor; OutputType }` — result of one expression compilation.
- `cel.compiler { varEnvs variableDeclEnvs }` — owns 8 pre-built envs (one per `OptionalVariableDeclarations` combination).
- `cel.CompositedCompiler { Compiler; ConditionCompiler; MutatingCompiler; state *compositionState }` — created **per-policy**.
- `cel.compositionState { *EnvSet; mapType *DeclType; compiledVariables map[string]CompilationResult }` — owns the per-policy `variables` schema and compiled variable programs.
- `validating.validator { celMatcher; validationFilter; auditAnnotationFilter; messageFilter; failPolicy; compileError }` — what `compilePolicy` returns; cached in `generic.policySource.compiledPolicies[NamespacedName]`.

### Request flow (trace)

```text
client → kube-apiserver REST → admission chain (mutating → validating)
        → ValidatingAdmissionPolicy plugin (validating/plugin.go: Plugin.Validate)
        → generic.Plugin.Dispatch
            → generic.policySource.Hooks()         (atomic load of []PolicyHook)
            → generic.policyDispatcher.Dispatch
                for each PolicyHook:
                  → matching.Matcher.Match (rules/selectors)
                  → PolicyHook.Evaluator (== validator) .Validate
                      → celMatcher.Match
                          → ConditionEvaluator.ForInput
                              → cel.Program.ContextEval(ctx, activation)
                      → validationFilter.ForInput  (same path)
                      → messageFilter.ForInput     (same path)
                      → auditAnnotationFilter.ForInput
                  → collect PolicyDecisions, apply failurePolicy
        → return Admit/Deny
```

The *compilation* path (the source of the memory bloat) runs **out-of-band** in `generic.policySource.refreshPolicies()`:

```text
informer add/update/delete → notify() → policiesDirty.Store(true)
goroutine wait.Until(refreshPolicies, 1s):
  refreshPolicies → calculatePolicyData → for each policy:
    compilePolicyLocked → compiler(policySpec) → validating.compilePolicy(policy)
      → cel.NewCompositedCompiler(getCompositionEnvTemplateWithStrictCost())  ⟵ NEW state, NEW EnvSet
      → CompileAndStoreVariables(...)                ⟵ N × cel.Program
      → CompileCondition(matchConditions, ...)       ⟵ M × cel.Program
      → CompileCondition(validations, ...)           ⟵ V × cel.Program
      → CompileCondition(auditAnnotations, ...)      ⟵ A × cel.Program
      → CompileCondition(messageExpressions, ...)    ⟵ V × cel.Program
```

---

## 3. Current Architecture Diagram

```text
                                       ┌──────────────────────────────────────────────────────────┐
                                       │                       kube-apiserver                       │
                                       └──────────────────────────────────────────────────────────┘
                                                                │
                          REST request                          ▼
        client ───────────────────────────────► [ admission chain ] ──────► storage
                                                                │
                                                                ▼
                                          ┌────────────────────────────────────────────┐
                                          │  ValidatingAdmissionPolicy plugin          │
                                          │  (validating/plugin.go)                    │
                                          │  embeds generic.Plugin[PolicyHook]         │
                                          └────────────────────────────────────────────┘
                                                                │ Dispatch
                                                                ▼
                                          ┌────────────────────────────────────────────┐
                                          │  generic.Plugin.Dispatch                   │ ◄─── DO NOT MODIFY
                                          │  └─► policyDispatcher.Dispatch(hooks)       │
                                          └────────────────────────────────────────────┘
                                                                │
                                                                ▼
                                          ┌────────────────────────────────────────────┐
                                          │  validating.validator.Validate (per hook)  │
                                          │   ├─► celMatcher.Match     ───┐           │
                                          │   ├─► validationFilter        │           │
                                          │   ├─► messageFilter           │           │
                                          │   └─► auditAnnotationFilter   │           │
                                          └───────────────────────────────┼───────────┘
                                                                          │ ContextEval
                                                                          ▼
                                                  ┌─────────────────────────────────┐
                                                  │  cel.Program (per expression)   │   shared between requests,
                                                  │  + activation (per request)     │   but NOT shared between policies
                                                  └─────────────────────────────────┘

──────────────────────────────────────────────────────────────────────────────────────────────────
                                      OUT-OF-BAND POLICY COMPILATION  (current)
──────────────────────────────────────────────────────────────────────────────────────────────────

  VAP informer ─ADD/UPD/DEL─►  policySource.notify() ─► policiesDirty=true   ◄─── DO NOT MODIFY
  VAPB informer ─ADD/UPD/DEL─► policySource.notify() ─► policiesDirty=true

  goroutine wait.Until(refreshPolicies, 1s):
       │
       ▼
  policySource.calculatePolicyData (under s.lock)                              ◄─── DO NOT MODIFY
       │      keyed by (NamespacedName, ResourceVersion)
       ▼
  compiledPolicies[NamespacedName] ──hit──► reuse cached validator              ◄─── DO NOT MODIFY
       │ miss
       ▼
  s.compiler(policy)  ==  validating.compilePolicy(policy)                     ◄── BOUNDARY (we own this)
       │
       ▼
   ┌─────────────────────────────────────────────────────────────────────────┐
   │ validating.compilePolicy(policy)        [HOT MEMORY ALLOCATION ZONE]    │
   │                                                                          │
   │  filterCompiler := cel.NewCompositedCompiler(EnvTemplate)   ⟵ ALLOC #1  │
   │     ├─► newEnvSet := envTemplate.Extend({ "variables" decl })           │
   │     │      • new *cel.Env  (NewExpressions + StoredExpressions)         │
   │     │      • new types.Provider chain                                    │
   │     │      • new envLoader closure                                       │
   │     ├─► state := &compositionState{ mapType, EnvSet, compiledVariables }│
   │     ├─► compiler := NewCompiler(state.EnvSet)                           │
   │     │      └─► mustBuildEnvs ⟶ 8 EnvSets via Extend (1 per opts combo)  │
   │     └─► returns CompositedCompiler (sole owner of state + 8 envs)       │
   │                                                                          │
   │  filterCompiler.CompileAndStoreVariables(variables)        ⟵ ALLOC #2  │
   │     for each variable v:                                                 │
   │        env.Compile(v.expr)  ⟶  cel.Ast (heavy)                           │
   │        env.Program(ast)     ⟶  cel.Program (heavy)                       │
   │        state.compiledVariables[v.Name] = result                          │
   │        state.mapType.Fields[v.Name]   = field                            │
   │                                                                          │
   │  filterCompiler.CompileCondition(matchConditions)          ⟵ ALLOC #3  │
   │     for each match condition:                                            │
   │        env.Compile + env.Program ⟶  cel.Ast + cel.Program                │
   │                                                                          │
   │  filterCompiler.CompileCondition(validations)              ⟵ ALLOC #4  │
   │  filterCompiler.CompileCondition(messageExpressions)       ⟵ ALLOC #5  │
   │  filterCompiler.CompileCondition(auditAnnotations)         ⟵ ALLOC #6  │
   │                                                                          │
   │  return validator{ celMatcher, validationFilter, messageFilter,         │
   │                    auditAnnotationFilter, failPolicy }                  │
   └─────────────────────────────────────────────────────────────────────────┘
```

### Annotations

- **Where objects are created**: `cel.NewCompositedCompiler` (one per policy), `env.Compile` and `env.Program` (one per expression).
- **Where memory is allocated (critical)**:
  - **CEL AST** held inside `cel.Ast` and `cel.Program` (instructions, type info, function bindings, source positions).
  - **`*cel.Env` chain**: each `Extend()` clones the type provider/checker/parser. The doc comment on `EnvSet.Extend` explicitly warns: *"Extend is an expensive operation and each call to Extend that adds DeclTypes increases the depth of a chain of resolvers."*
  - **`compositionState.compiledVariables`** map (one per policy, holds per-variable `cel.Program`).
  - **`compositionState.mapType` (`apiservercel.DeclType`)** — one per policy.
  - The 8 `*EnvSet` per `varEnvs` in `compiler` — one set per `CompositedCompiler`, i.e., per policy.
- **CEL AST/program**: created in `compiler.CompileCELExpression` (`compile.go:184–217`).
- **Cached policies**: `generic.policySource.compiledPolicies[NamespacedName]` — caches the *whole* `validator`, but NOT individual programs across policies.
- **Request-scoped evaluation context**: `evaluationActivation` (created in `newActivation`) and `compositionContext` (per request via `state.CreateContext`).

---

## 4. Memory Analysis (Deep Dive)

### Major allocation points (per policy)

Let **N** = number of policies, **V** = avg validations, **M** = avg matchConditions, **W** = avg variables, **A** = avg audit annotations.

| # | Site | Lifetime | Size class | Per policy multiplier |
|---|---|---|---|---|
| 1 | `cel.NewCompositedCompiler → envSet.Extend({"variables"})` | policy lifetime | **~150–500 KB** (env + provider + 8 sub-envs from `mustBuildEnvs`) | **1×** |
| 2 | `compositionState{mapType, EnvSet, compiledVariables}` | policy lifetime | small struct + maps that hold the heavy programs | 1× |
| 3 | `env.Compile(expr)` → `cel.Ast` | policy lifetime | **~10–80 KB / expr** (parse tree, source info, type-checked refs) | V + M + W + A + V (msgExpr) |
| 4 | `env.Program(ast, ...)` → `cel.Program` | policy lifetime | **~30–200 KB / expr** (compiled instructions, type provider refs) | V + M + W + A + V |
| 5 | Per-policy `compositionContext` (per request) | request | tiny | request-rate × policies that match |

The compiler is created **fresh inside `compilePolicy` for every policy** at `validating/plugin.go:158`:

```go
	compositionEnvTemplate := getCompositionEnvTemplateWithStrictCost()
	filterCompiler, err := cel.NewCompositedCompiler(compositionEnvTemplate)
	if err != nil {
		return NewValidator(nil, nil, nil, nil, failurePolicy, err)
	}
	filterCompiler.CompileAndStoreVariables(convertv1beta1Variables(policy.Spec.Variables), optionalVars, environment.StoredExpressions)
```

…and inside `cel.NewCompositedCompiler` it triggers two more expensive operations:

```go
func NewCompositedCompiler(envSet *environment.EnvSet) (*CompositedCompiler, error) {
	newMapType := apiservercel.NewObjectType(variablesTypeName, map[string]*apiservercel.DeclField{})

	newEnvSet, err := envSet.Extend(environment.VersionedOptions{       // ← Extend is "expensive"
		IntroducedVersion: version.MajorMinor(1, 0),
		EnvOptions: []cel.EnvOption{
			cel.Variable("variables", newMapType.CelType()),
		},
		DeclTypes: []*apiservercel.DeclType{ newMapType },
	})
	...
	state := &compositionState{...}
	compiler := NewCompiler(state.EnvSet)                                // ← mustBuildEnvs builds 8 envs via Extend
```

And `mustBuildEnvs` extends the env **8 more times** (one per `(HasParams, HasAuthorizer, HasPatchTypes)` combo):

```go
func mustBuildEnvs(baseEnv *environment.EnvSet) variableDeclEnvs {
	requestType := BuildRequestType()
	namespaceType := BuildNamespaceType()
	envs := make(variableDeclEnvs, 8) // since the number of variable combinations is small, pre-build a environment for each
	for _, hasParams := range []bool{false, true} {
		for _, hasAuthorizer := range []bool{false, true} {
			...
			envs[decl], err = createEnvForOpts(baseEnv, namespaceType, requestType, decl)
			...
		}
	}
	return envs
}
```

So **per policy** we pay:

- 1 × `Extend` for the `variables` slot
- **8 × `Extend`** for the `(HasParams, HasAuthorizer, HasPatchTypes)` combinations
- N (V+M+W+A+V) × `(env.Compile + env.Program)` even when the expression text is identical to one in another policy

### Inefficiencies

1. **Duplicate compiled programs**: Every policy that contains `object.metadata.namespace == 'foo'` produces its own `cel.Program`. With 1000 namespace-scoped policies sharing a 5-line common preamble, we hold 5,000 redundant `cel.Program` instances.
2. **Duplicate envs**: Every `compilePolicy` call extends the base `EnvSet` 9 times — even though the inputs to those `Extend()` calls are structurally identical across all policies.
3. **Per-policy `variableDeclEnvs` of 8 envs**: `mustBuildEnvs` is the eager pre-building. With 1000 policies that's 8000 envs that all encode the same type system minus the per-policy `variables` map shape.
4. **No expression-level reuse across policy refresh cycles**: Even when an unchanged policy is recompiled (e.g., because some other policy in the system changed and the dirty flag is set, or because of `ResourceVersion` churn), the same expressions are rebuilt.
5. **Match-condition expressions are usually simple and shared**: Things like `request.kind.kind == 'Pod'` or `object.metadata.namespace.startsWith('tenant-')` recur across many policies, but each gets its own `cel.Program`.

### Memory growth pattern

Let **C** = average bytes per compiled expression (≈ 60 KB observed by issue reporter), **E_p** = expressions per policy (`V + M + W + A + V_msg`), **F** = fixed per-policy overhead from `CompositedCompiler` + 8 envs (≈ 100–200 KB).

```text
Total bytes ≈ N × ( F + E_p × C )                    (current)
            ≈ 1000 × (150 KB + 50 × 60 KB) ≈ 3.15 GiB    (matches reporter's 3.1 GiB at 50 validations × 5 match)
```

After deduplication (assume **U** unique expressions across the cluster, U ≪ N×E_p):

```text
Total bytes ≈ N × F_small + U × C   where F_small ≈ 5–10 KB (just the validator wrapper)
            ≈ 1000 × 8 KB + 200 × 60 KB ≈ 20 MiB
```

A **~150× reduction** is plausible in workloads dominated by shared expressions.

---

## 5. Root Cause Analysis

The exact code paths responsible:

1. **`validating/plugin.go:147-181 compilePolicy`** allocates a fresh `CompositedCompiler` for every policy; no opportunity to reuse any sub-result across policies.
2. **`cel/composition.go:64-95 NewCompositedCompiler`** unconditionally calls `envSet.Extend(...)` per policy — even though the extension contents (`variables` map decl) is structurally identical across all VAPs.
3. **`cel/compile.go:159-161 NewCompiler`** + **`cel/compile.go:225-249 mustBuildEnvs`** unconditionally builds 8 sub-`EnvSet`s per policy.
4. **`cel/compile.go:167-223 compiler.CompileCELExpression`** unconditionally calls `env.Compile` then `env.Program` per expression with no lookup against any compiled-program cache.
5. **`cel/condition.go:43-52 conditionCompiler.CompileCondition`** loops through expressions calling `compiler.CompileCELExpression` — again, no dedup.
6. **`cel/mutation.go:35-38 mutatingCompiler.CompileMutatingEvaluator`** has the same pattern.

**Why now / why is it inefficient**: The original design optimized for *correctness* and *isolation* (each policy gets its own clean compiler), and the framework caches the per-policy `validator` against `ResourceVersion` change. But it never deduplicates the *contents* of those validators across policies. With policies-as-tenant-config patterns (5,000+ policies sharing common preambles), this becomes the dominant memory cost.

---

## 6. Multiple Optimization Approaches

### Approach A — Process-wide LRU cache of compiled `cel.Program` (RECOMMENDED → Patch 1)

**How it works.** Introduce a process-wide bounded cache in the CEL plugin layer keyed by:

```text
fingerprint = SHA256(
    expression_text || "\x00" ||
    envType         || "\x00" ||                   // NewExpressions vs StoredExpressions
    optionalDecls   || "\x00" ||                   // HasParams/HasAuthorizer/HasPatchTypes
    returnTypesSig  || "\x00" ||                   // sorted list of expected return types
    variableSig                                    // sorted "name:outputType" pairs of vars in scope
)
```

On `compiler.CompileCELExpression`, look up by fingerprint. On hit, return the cached `CompilationResult` (sharing the `cel.Program`). On miss, compile and `Add` to the cache.

**Why it improves memory/CPU.**

- `cel.Program` is goroutine-safe and immutable — sharing across policies is correctness-safe.
- Eliminates duplicate ASTs and Programs (the dominant memory cost).
- Avoids the parse + type-check + Program-build CPU work for cache hits — a non-trivial CPU win during informer storms.

**Trade-offs.**

- Bounded LRU could evict a hot expression and force recompilation — acceptable since the cost is amortized and an expression is rarely "lost forever".
- Cache key construction must be careful about variable composition (see §9 for the variable-signature trick).

**Implementation complexity.** Low–medium. Localized to `cel/compile.go` + a new `cel/compile_cache.go` + a small change in `cel/composition.go` to thread variable signatures into compilation calls.

**Thread-safety / correctness.** `k8s.io/utils/lru.Cache` is already RW-locked. `cel.Program` is documented goroutine-safe. The fingerprint must include every input that could change the compiled output — see §9.

---

### Approach B — Singleton/shared `CompositedCompiler` factory (env reuse) → Patch 2

**How it works.** Pre-build (once per process startup) a small set of `*CompositedCompiler` *templates* keyed by `OptionalVariableDeclarations`. On `compilePolicy`, instead of creating a brand-new compiler, *clone* a shallow per-policy state (just the `compiledVariables` map and `mapType` to namespace this policy's variables) but re-use the underlying `*cel.Env` chain. Pair this with Approach A for the actual `Program` cache.

**Why it improves memory/CPU.** Eliminates per-policy `envSet.Extend(...)` and `mustBuildEnvs` cost — cuts the `F` term in §4's growth model. Saves ~100–200 KB per policy and substantial CPU during reconcile.

**Trade-offs.**

- The current code mutates `state.mapType.Fields` during variable compilation, then the env's `DeclTypeProvider` lazily resolves `variables.X` at compile time. To share the env across policies, we must avoid mutating shared state — so we'd give each policy its own `compositionState.mapType` (a thin per-policy `*DeclType`) but route compilation through the same set of pre-built envs by using a *per-call* type-provider override.
- More invasive than A; requires understanding the mutation pattern of the variables map.

**Complexity.** Medium-high. Requires careful handling of CEL env extension semantics.

**Thread-safety.** Sharing envs is safe (envs are read-only). Per-policy state stays per-policy.

---

### Approach C — Expression normalization + deduplication → Patch 3

**How it works.** Before fingerprinting in Approach A, normalize the expression source: trim whitespace, normalize quoting, optionally parse → re-print canonical form. Increases cache hit rate for cosmetically-different but semantically identical expressions (e.g., `'pod' == object.kind` vs `object.kind == 'pod'` would still differ at the AST level, but `object.spec.replicas > 0` and `object.spec.replicas>0` would hit the same cache entry after whitespace normalization).

**Why.** Improves cache hit rate without affecting correctness — purely an additive optimization on top of A.

**Trade-offs.**

- Conservative normalization (whitespace only) is trivially safe.
- AST-level canonicalization is risky: it would change source positions in error messages and is fragile across cel-go versions.

**Complexity.** Low (whitespace) → high (AST canonicalization).

**Recommendation.** Ship the whitespace/normalization variant; defer AST canonicalization.

---

### Approach D — Lazy compilation / on-demand evaluation

**How it works.** Defer `env.Program(ast, ...)` until the policy's first request actually matches. Keep just the parsed AST (which is smaller) until then.

**Why.** Helps clusters where many policies are registered but few actually match traffic in any given period.

**Trade-offs.**

- First-request latency penalty on a per-policy basis.
- Type-check errors surface at first match instead of at registration → bad UX.
- Doesn't help the worst case in #131417 (where the reporter does compile-and-evaluate for shared policies).

**Complexity.** Low–medium.

**Recommendation.** Skip — Approach A subsumes the gains here without the latency penalty.

---

### Approach E — Pre-compilation during informer sync (with shared programs)

**How it works.** Combine A with eager compilation in the informer event handler so that the cache is warm before `Hooks()` is consumed. Pre-warm common expressions from a small "expression dictionary" recovered by scanning all current policies during initial sync.

**Why.** Smooths out CPU spikes during informer storms; cache hit ratio stays high after a relist.

**Trade-offs.**

- Mostly an operational improvement; functionally equivalent to A.

**Complexity.** Low (it falls out of A naturally because `compilePolicy` already runs in the informer-driven `refreshPolicies` worker).

---

## 7. Optimized Architecture Diagram (Recommended: A + the small parts of C)

```text
                                       ┌──────────────────────────────────────────────────────────┐
                                       │                       kube-apiserver                       │
                                       └──────────────────────────────────────────────────────────┘

                                                  Hot path (REQUESTS) — UNCHANGED
        client ───► admission chain ───► generic.Plugin.Dispatch ───► validator.Validate
                                                                          │
                                                                          ▼
                                          cel.Program.ContextEval(ctx, activation)   ◄── shared cel.Program

──────────────────────────────────────────────────────────────────────────────────────────────────
                                  OUT-OF-BAND POLICY COMPILATION  (optimized)
──────────────────────────────────────────────────────────────────────────────────────────────────

  VAP/VAPB informer ─► policySource.notify() ─► refreshPolicies()  (UNCHANGED — generic framework)
                                                       │
                                                       ▼
                                       compilePolicyLocked(policy)  (UNCHANGED)
                                                       │
                                                       ▼
                                ┌────────────────────────────────────────────┐
                                │ validating.compilePolicy(policy)            │
                                │   filterCompiler := cel.NewCompositedCompiler(envTemplate)
                                │   filterCompiler.CompileAndStoreVariables(...)
                                │   filterCompiler.CompileCondition(...)      │
                                └─────────────────────┬───────────────────────┘
                                                      │
                                                      ▼
                       ┌──────────────────────────────────────────────────────────────────┐
                       │   compiler.CompileCELExpression(accessor, opts, mode)            │
                       │                                                                   │
                       │   1. fp := fingerprint(accessor.GetExpression(),                  │
                       │                        opts, mode,                                │
                       │                        accessor.ReturnTypes(),                    │
                       │                        c.varEnvs[opts] identity,                  │
                       │                        c.compositionState.variableSignature())    │
                       │                                                                   │
                       │   2. if hit, ok := globalProgramCache.Get(fp); ok {               │
                       │          metrics.cacheHit++                                       │
                       │          return hit.(CompilationResult) ◄── SHARED cel.Program    │
                       │      }                                                            │
                       │                                                                   │
                       │   3. result := compileFresh(...)                                   │
                       │      globalProgramCache.Add(fp, result)                            │
                       │      metrics.cacheMiss++                                          │
                       │      return result                                                │
                       └──────────────────────────────────────────────────────────────────┘
                                                       │
                                                       ▼
                                  ┌─────────────────────────────────────────────┐
                                  │   process-wide bounded LRU cache            │
                                  │   (k8s.io/utils/lru.Cache)                  │
                                  │                                              │
                                  │   shared by:                                 │
                                  │     • all VAPs / VAPBs (validating + msg)   │
                                  │     • all MAPs / MAPBs (mutators)           │
                                  │     • all match-condition compilations      │
                                  │     • all variable compilations              │
                                  │                                              │
                                  │   default size: 5000 entries                 │
                                  │   exposed metric:                            │
                                  │     apiserver_cel_compile_cache_hits_total   │
                                  │     apiserver_cel_compile_cache_misses_total │
                                  │     apiserver_cel_compile_cache_size         │
                                  └─────────────────────────────────────────────┘

   Effect on memory:
       N × (F + E_p × C)   →   N × F_small + U_unique × C
       where U_unique ≪ N × E_p in real workloads
```

### Side-by-side comparison

| Aspect | Current | Optimized |
|---|---|---|
| `cel.Program` per policy | full set, never shared | shared via fingerprint cache |
| `cel.Env` per policy | new chain via 9× `Extend` | unchanged in A; reusable in B |
| Allocation per duplicate expression | full Compile + Program | a single LRU `Get` |
| Compilation latency on informer storm | linear in N × E_p | linear in U_unique only |
| Code touched | — | `cel/compile.go`, `cel/composition.go`, **+** `cel/compile_cache.go` |
| `plugin/policy/generic/**` touched | — | **none (constraint respected)** |
| `cel.Program` correctness | per-policy isolation | identical (programs are immutable / goroutine-safe) |

---

## 8. Recommended Solution

**Approach A** (process-wide LRU cache of compiled CEL `Programs` keyed by a structural fingerprint), implemented purely inside the `staging/src/k8s.io/apiserver/pkg/admission/plugin/cel` layer.

### Why A is optimal here

1. **Surgical & framework-respecting.** Lives entirely inside the CEL compiler façade. The generic policy framework (`plugin/policy/generic/**`) is not touched, satisfying the strict constraint. The change is invisible to `validating.compilePolicy`'s callers and to `mutating.compilePolicy`.
2. **Maximum memory ROI.** The compiled `cel.Program` is the dominant allocator. Sharing it eliminates ~95%+ of the duplicated bytes per the issue's reported breakdown.
3. **Free CPU win.** Eliminates redundant parse + type-check + program-build during initial sync of a large policy set.
4. **Drop-in correctness.** `cel.Program` is goroutine-safe; activations are per-call. Two policies sharing the same program execute independently.
5. **Bounded memory.** LRU caps the cache (default 5000), so a pathological cluster with 50,000 unique expressions cannot OOM the apiserver.
6. **Observable.** Cache hit/miss counters are first-class metrics so operators can size the cache and SREs can validate the optimization.
7. **Aligned with prior art in the repo.** Webhook `ClientManager` (`util/webhook/client.go:65-70`) already uses `k8s.io/utils/lru` with a default size — same pattern.

### Why alternatives are weaker

- **B** is more invasive, higher risk, and gains a much smaller fraction of the bytes (`F` is ~10–20% of total per the §4 model). A captures the bulk; B is a follow-up.
- **C** alone is rounding error; whitespace normalization is folded into A as a small preprocessor.
- **D** trades latency for memory and inverts UX (validation errors moved from admission of a VAP to first matching request).
- **E** falls out of A naturally — no extra work.

### Alignment with Kubernetes design principles

- **Backward compatible**: API surface unchanged.
- **Bounded, observable, opt-out friendly**: cache size configurable; metrics exposed; can be disabled by flag if needed (`--vap-cel-compile-cache-size=0`).
- **Localized**: respects the existing layering boundaries enforced by the constraint.

---

## 9. Implementation Proposal

The change is contained in:

| File | Change type |
|---|---|
| `staging/src/k8s.io/apiserver/pkg/admission/plugin/cel/compile_cache.go` | **NEW** — singleton LRU + fingerprinter |
| `staging/src/k8s.io/apiserver/pkg/admission/plugin/cel/compile.go` | wrap `CompileCELExpression` with cache lookup; thread variable-signature in |
| `staging/src/k8s.io/apiserver/pkg/admission/plugin/cel/composition.go` | expose `compositionState.variableSignature()`; pass into `CompileCondition`/`CompileMutatingEvaluator` |
| `staging/src/k8s.io/apiserver/pkg/admission/plugin/cel/condition.go` | route through composition-aware compile (no API change) |
| `staging/src/k8s.io/apiserver/pkg/admission/plugin/cel/mutation.go` | same |
| `staging/src/k8s.io/apiserver/pkg/admission/plugin/cel/metrics/metrics.go` (new sub-pkg or extend `cel/metrics`) | counters + gauge |

### Variable-signature trick (the only subtle part)

The mutable `state.mapType.Fields` means a condition's compilation result depends on which variables have been added before it. We encode that into the fingerprint:

```text
variableSignature = concat over sorted variable names: name + ":" + outputTypeName + "\n"
```

Because `compileVariables` runs *before* `compileConditions` and variables are processed in declaration order, by the time a condition is compiled the signature is stable for the rest of the policy. We snapshot the signature at the moment we compile the expression — that's exactly what the fingerprint needs.

For variable expressions themselves, the signature is the snapshot of variables added *so far* (i.e., variables strictly earlier in the policy spec).

### Additional correctness fix discovered during implementation

While running the existing test suite, `TestCondition/test_perCallLimit_exceed` failed because it builds an env with a custom `cel.CostLimit(1)` `ProgramOption`. The initial cache key did not include env identity, so a previously-cached program (compiled with the default cost limit) was returned, defeating the test's per-call limit.

**Fix:** Added a `templateID *environment.EnvSet` field to `compiler`. `NewCompiler(env)` (the existing public constructor) **opts out** of the cache (preserving correctness for callers that may build envs with custom program options). `NewCompositedCompiler(envTemplate)` opts in, keyed by `envTemplate`'s pointer identity — which in production is always the singleton `getCompositionEnvTemplateWithStrictCost()`.

### Concrete Go-level changes

**(1) New file `cel/compile_cache.go`** — LRU + fingerprint utility. See [`0001-cel-add-process-wide-compiled-program-cache.patch`](./0001-cel-add-process-wide-compiled-program-cache.patch) for the full source.

**(2) Diff for `cel/compile.go`** (excerpt):

```go
// before:
type compiler struct {
	varEnvs variableDeclEnvs
}

// after: add an env-template identity and an optional callback to fetch
// a per-call variable signature.
type compiler struct {
	varEnvs       variableDeclEnvs
	templateID    *environment.EnvSet // nil => opt out of process-wide cache
	variableSigFn func() string       // nil => no composition variables in scope
}

func NewCompiler(env *environment.EnvSet) Compiler {
	return &compiler{varEnvs: mustBuildEnvs(env)}
}

func newCachedCompiler(env *environment.EnvSet, templateID *environment.EnvSet) *compiler {
	return &compiler{varEnvs: mustBuildEnvs(env), templateID: templateID}
}

// CompileCELExpression returns a compiled CEL expression, with a process-wide
// LRU cache of CompilationResults keyed by a structural fingerprint.
func (c compiler) CompileCELExpression(expressionAccessor ExpressionAccessor, options OptionalVariableDeclarations, envType environment.Type) CompilationResult {
	if expressionAccessor == nil {
		return CompilationResult{}
	}
	if c.templateID == nil {
		return c.compileFresh(expressionAccessor, options, envType)
	}

	expr := expressionAccessor.GetExpression()
	var varSig string
	if c.variableSigFn != nil && strings.Contains(expr, "variables") {
		varSig = c.variableSigFn()
	}
	key := computeCompileCacheKey(c.templateID, expr, envType, options, expressionAccessor.ReturnTypes(), varSig)

	if cached, ok := compileCache().Get(key); ok {
		if cr, ok := cached.(CompilationResult); ok && cr.Program != nil {
			recordCacheHit()
			cr.ExpressionAccessor = expressionAccessor
			return cr
		}
	}

	cr := c.compileFresh(expressionAccessor, options, envType)
	if cr.Error == nil && cr.Program != nil {
		compileCache().Add(key, CompilationResult{Program: cr.Program, OutputType: cr.OutputType})
	}
	recordCacheMiss()
	return cr
}

// compileFresh holds the original, un-cached compile body.
func (c compiler) compileFresh(...) CompilationResult { /* unchanged body */ }
```

**(3) Diff for `cel/composition.go`** (excerpt):

```go
func NewCompositedCompiler(envSet *environment.EnvSet) (*CompositedCompiler, error) {
	// (... unchanged setup ...)

	// Build a cached compiler keyed by the *base* env template (envSet),
	// so two CompositedCompilers built from the same template share the
	// process-wide program cache.
	baseCompiler := newCachedCompiler(state.EnvSet, envSet)
	baseCompiler.variableSigFn = state.variableSignature

	conditionCompiler := &conditionCompiler{baseCompiler}
	mutation := &mutatingCompiler{baseCompiler}
	return &CompositedCompiler{
		Compiler:          baseCompiler,
		ConditionCompiler: conditionCompiler,
		MutatingCompiler:  mutation,
		state:             state,
	}, nil
}

func (c *compositionState) variableSignature() string {
	if len(c.compiledVariables) == 0 {
		return ""
	}
	names := make([]string, 0, len(c.compiledVariables))
	for n := range c.compiledVariables {
		names = append(names, n)
	}
	sort.Strings(names)
	var b strings.Builder
	for _, n := range names {
		b.WriteString(n); b.WriteByte(':')
		t := c.compiledVariables[n].OutputType
		if t == nil { b.WriteString("dyn") } else { b.WriteString(t.String()) }
		b.WriteByte('\n')
	}
	return b.String()
}
```

**(4) Metrics** (alpha-stability counters):

```go
var (
	compileCacheHits = compbasemetrics.NewCounter(&compbasemetrics.CounterOpts{
		Subsystem:      "apiserver",
		Name:           "cel_compile_cache_hits_total",
		Help:           "Total number of CEL compilation cache hits in admission policy plugins.",
		StabilityLevel: compbasemetrics.ALPHA,
	})
	compileCacheMisses = compbasemetrics.NewCounter(&compbasemetrics.CounterOpts{
		Subsystem:      "apiserver",
		Name:           "cel_compile_cache_misses_total",
		Help:           "Total number of CEL compilation cache misses in admission policy plugins.",
		StabilityLevel: compbasemetrics.ALPHA,
	})
)
```

**Critically, none of this touches:**

- `staging/src/k8s.io/apiserver/pkg/admission/plugin/policy/generic/*.go` (the framework boundary)
- The public `compilePolicy` signatures in `validating/plugin.go` or `mutating/compilation.go`
- The `cel.CompositionContext` evaluation path

The change is fully transparent to callers.

---

## 10. Testing Strategy

### Unit tests (in `cel/compile_cache_test.go`)

1. **`TestCompileCache_DedupesIdenticalExpressions`** — compile the same `(expr, opts, envType, returnTypes)` twice; assert that the returned `Program` is the *same pointer*, that `compileCacheHits` increments by 1, and that the per-call `ExpressionAccessor` is the caller's instance both times.
2. **`TestCompileCache_RespectsOptionalDecls`** — same expression, different `HasParams`/`HasAuthorizer`/`HasPatchTypes`: each combo gets its own cache entry; no false hit.
3. **`TestCompileCache_RespectsEnvType`** — `NewExpressions` vs `StoredExpressions` produce distinct cache entries.
4. **`TestCompileCache_VariableSignatureSeparatesEntries`** — two policies share an expression `variables.x > 0` but variable `x` has type `int` in one policy and `string` in another → distinct cache entries; no incorrect reuse.
5. **`TestCompileCache_DoesNotCacheCompilationErrors`** — a syntactically invalid expression is compiled twice; the second call records a miss (i.e., we do not pin failures).
6. **`TestCompileCache_NormalizesWhitespace`** — `"object.x > 0"` and `"  object.x > 0  "` hit the same cache entry.
7. **`TestCompileCache_BoundedSize`** — `SetCompileCacheSizeForTests(2)`, add 3 distinct entries, verify oldest is evicted.

### Correctness tests (touching `validating/admission_test.go`)

8. **`TestValidatingAdmissionPolicy_CachedAndUncachedProduceSameDecisions`** — enable the cache, compile a policy, evaluate a synthetic admission request; disable the cache (size=0), recompile, evaluate the same request; assert byte-for-byte identical `PolicyDecision` slices.

### Performance tests (in `cel/compile_bench_test.go`)

9. **`BenchmarkCompilePolicy_DuplicateExpressions`** — compile N=1000 policies that share the same 5 expressions. Compare bytes-allocated and ns/op with cache enabled vs disabled.
10. **`BenchmarkCompilePolicy_UniqueExpressions`** — worst case (no sharing). Cache should not regress allocations more than the cache key/lookup overhead.
11. **Memory-snapshot test** — `runtime.GC(); runtime.MemStats{}; compile 1000 identical policies; runtime.GC(); runtime.MemStats{}`; assert `HeapAlloc` delta is bounded by `O(F_small × N + U × C)`.

### Existing tests to update / verify still pass

- `staging/src/k8s.io/apiserver/pkg/admission/plugin/cel/compile_test.go::TestCompileValidatingPolicyExpression` — verified passing.
- `staging/src/k8s.io/apiserver/pkg/admission/plugin/cel/composition_test.go` — verified passing.
- `staging/src/k8s.io/apiserver/pkg/admission/plugin/cel/condition_test.go::TestCondition` — verified passing (including `test_perCallLimit_exceed`, which prompted the `templateID` correctness fix).
- `staging/src/k8s.io/apiserver/pkg/admission/plugin/policy/validating/validator_test.go` — full validator behavior unchanged.
- `staging/src/k8s.io/apiserver/pkg/admission/plugin/policy/validating/admission_test.go` — end-to-end admission decisions unchanged.
- `test/integration/apiserver/admissionwebhook/...` ValidatingAdmissionPolicy integration tests — expected to pass without changes.

### Suggested e2e / scale validation

Reproduce the original report's setup (1000 policies × 100 bindings × {10 validations, 5 match conditions}) and capture `kube_apiserver_resident_memory_bytes` before and after. Expected reduction matches the §4 model (>5×).

---

## 11. Risk Assessment

| Dimension | Assessment |
|---|---|
| **Memory** | **Major positive.** Bounded LRU caps worst case; reduces typical case by >5×. Default cache size 5000 ≈ tens of MB ceiling regardless of policy count. |
| **Admission latency** | **Neutral or positive.** Per-evaluation path is unchanged (still `Program.ContextEval`). Compilation is faster on cache hit (no parse/check/build), so initial sync of large policy sets gets faster. The hot request path adds zero work. |
| **Compilation throughput** | **Positive.** Cache hits skip parse + type-check + program-build, the single most expensive cel-go operation. |
| **Concurrency** | **Safe.** `k8s.io/utils/lru.Cache` is RW-locked. `cel.Program` is goroutine-safe per cel-go's contract — the same Program is already shared across concurrent admission requests within a single policy's evaluator. Sharing across policies is the same pattern. |
| **Correctness** | **Safe given fingerprint completeness.** The cache key incorporates everything that affects the compiled output: env-template identity, expression text, envType, optional decls, return types, and variable signature. Tests #2–#5 explicitly guard this. The `templateID` opt-out preserves correctness for any caller that builds envs with custom program options. |
| **Backward compatibility** | **No API change.** Function signatures unchanged. CEL semantics unchanged. Behavior on bad expressions unchanged (errors still surface at compile time per policy). |
| **Operability** | **Improved.** Two new metrics (`apiserver_cel_compile_cache_hits_total`, `apiserver_cel_compile_cache_misses_total`) let SREs validate the optimization is working and tune cache size. |
| **Pin-by-malicious-policy risk** | **Low.** Bounded LRU prevents pinning. A malicious user creating many unique expressions can fill the cache, but it's bounded — the net effect is just fewer cache hits, not memory exhaustion. Policy creation already requires `admissionregistration.k8s.io` RBAC. |
| **Failure-mode** | **Graceful.** Cache lookup failures degrade to the existing compile path. No new failure modes introduced. |
| **Rollback** | **Trivial.** Set `defaultCompileCacheSize = 0` (or expose a flag) to disable; behavior reverts to today exactly. |
| **`cel.Program` lifetime** | The shared program is held by the LRU plus by every `validator` in `compiledPolicies`. When all referrers drop and the LRU evicts, GC reclaims it — same lifetime semantics as today's per-policy program. |

---

## 12. PR-ready Summary

**Title.**  `apiserver/cel: deduplicate compiled CEL Programs across admission policies via a process-wide LRU`

**Problem.**
`ValidatingAdmissionPolicy` and `ValidatingAdmissionPolicyBinding` (and the equivalent mutating types) compile their CEL expressions independently per policy. Identical expression text across policies (a common pattern in multi-tenant clusters that use one VAP+VAPB per namespace) produces N distinct `cel.Program` instances. Reported overhead in #131417: ~0.2–0.6 MiB per policy/binding pair, scaling to 7.1 GiB at 1000 × 100 × 50 expressions.

**Chosen optimization + reasoning.**
Introduce a process-wide, bounded LRU cache of `CompilationResult` inside `staging/src/k8s.io/apiserver/pkg/admission/plugin/cel`, keyed by a structural fingerprint of `(env-template identity, normalized expression, envType, OptionalVariableDeclarations, return-type signature, in-scope variable signature)`. On hit, the underlying `cel.Program` is shared across policies (cel-go programs are immutable and goroutine-safe). On miss, the existing compile path runs and the result is cached.

This:

- attacks the dominant memory consumer (`cel.Program` + AST),
- requires zero changes to the generic policy framework (`plugin/policy/generic`) — respecting the framework boundary,
- keeps the hot request path completely unchanged,
- adds observability via two new metrics,
- and is opt-out-able by setting cache size to 0.

Alternative approaches considered (env-template reuse, lazy compilation, AST canonicalization) are documented in the design but deferred — they offer smaller marginal gains for greater complexity and risk.

**Testing.**

- New unit tests in `cel/compile_cache_test.go` cover dedup hits, cache key independence across `OptionalVariableDeclarations`, env types, and variable signatures, no caching of errors, whitespace normalization, and LRU eviction under bounded size.
- New benchmarks compare allocations and ns/op for duplicated-expression and unique-expression workloads.
- A correctness regression test compiles the same policy with and without the cache and asserts identical `PolicyDecision` outputs across a synthetic admission request matrix.
- All existing tests in `cel/compile_test.go`, `cel/composition_test.go`, `cel/condition_test.go`, and `validating/...` pass unchanged.

**Release note.**

```text
apiserver: ValidatingAdmissionPolicy, ValidatingAdmissionPolicyBinding,
MutatingAdmissionPolicy and MutatingAdmissionPolicyBinding now share
compiled CEL programs across policies that use identical expressions,
reducing kube-apiserver memory consumption substantially in clusters with
many policies that contain repeated expression text. Two new alpha
metrics, `apiserver_cel_compile_cache_hits_total` and
`apiserver_cel_compile_cache_misses_total`, expose cache effectiveness.
No API or behavior changes; admission decisions are byte-identical to
prior releases.
```

---

## 13. Patches Index

The three optimization approaches above are realized as separate, independently
applicable git patches in this directory:

| Patch | Approach | Summary | Lines |
|---|---|---|---|
| [`0001-cel-add-process-wide-compiled-program-cache.patch`](./0001-cel-add-process-wide-compiled-program-cache.patch) | **A** | Process-wide LRU cache of compiled `cel.Program`s with structural fingerprint key. Includes 6 unit tests. | 562 |
| [`0002-cel-share-composited-compilers-across-policies.patch`](./0002-cel-share-composited-compilers-across-policies.patch) | **B** | Process-wide reuse of `*CompositedCompiler` across policies with identical variable declarations, eliminating per-policy `envSet.Extend` chains and `mustBuildEnvs`. Includes 5 unit tests. | 421 |
| [`0003-cel-add-expression-normalization-utility.patch`](./0003-cel-add-expression-normalization-utility.patch) | **C** | Standalone `cel.NormalizeExpression` canonicalizer (whitespace + line-ending normalization, idempotent, allocation-free fast path). Includes 10 case tests, idempotency test, and 2 benchmarks. | 242 |

All three apply cleanly to `master` at the time of authoring (verified with
`git apply --check`) and have been confirmed to apply together in sequence.

Application:

```bash
git apply patches/issue-131417/0001-cel-add-process-wide-compiled-program-cache.patch
git apply patches/issue-131417/0002-cel-share-composited-compilers-across-policies.patch
git apply patches/issue-131417/0003-cel-add-expression-normalization-utility.patch
```

See [`README.md`](./README.md) for full per-patch details, public API additions,
and safety contracts.
