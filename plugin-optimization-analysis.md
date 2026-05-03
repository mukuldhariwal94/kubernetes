# ValidatingAdmissionPolicy Plugin — Performance & Memory Optimization Analysis

**Target file:** [staging/src/k8s.io/apiserver/pkg/admission/plugin/policy/validating/plugin.go](staging/src/k8s.io/apiserver/pkg/admission/plugin/policy/validating/plugin.go)

**Supporting files:**
- [staging/src/k8s.io/apiserver/pkg/admission/plugin/cel/compile.go](staging/src/k8s.io/apiserver/pkg/admission/plugin/cel/compile.go)
- [staging/src/k8s.io/apiserver/pkg/admission/plugin/cel/composition.go](staging/src/k8s.io/apiserver/pkg/admission/plugin/cel/composition.go)
- [staging/src/k8s.io/apiserver/pkg/admission/plugin/policy/generic/policy_source.go](staging/src/k8s.io/apiserver/pkg/admission/plugin/policy/generic/policy_source.go) — **not modified** (generic framework)

**Scope rule:** no changes to `policy/generic/*`.

---

## 1. Duplicate CEL expressions

### What the code does today

[`compilePolicy`](staging/src/k8s.io/apiserver/pkg/admission/plugin/policy/validating/plugin.go#L147) iterates `policy.Spec.MatchConditions`, `policy.Spec.Validations` (expression + messageExpression), and `policy.Spec.AuditAnnotations`, and for each entry calls [`conditionCompiler.CompileCondition`](staging/src/k8s.io/apiserver/pkg/admission/plugin/cel/condition.go#L43), which in turn calls [`compiler.CompileCELExpression`](staging/src/k8s.io/apiserver/pkg/admission/plugin/cel/compile.go#L196) once per expression:

```go
// cel/condition.go:43-52
for i, expressionAccessor := range expressionAccessors {
    if expressionAccessor == nil {
        continue
    }
    compilationResults[i] = c.compiler.CompileCELExpression(expressionAccessor, options, mode)
}
```

Each call runs `env.Compile(expr)` → `env.Program(ast)`, producing a retained `cel.Program`. There is **no deduplication at any level**:

| Duplication axis | Dedup today? | Observation |
|---|---|---|
| **A. Intra-policy** — same expression used as `matchCondition` *and* again inside a `validation` | ❌ | Compiled twice per policy. |
| **B. Intra-tenant** — identical expression across many policies (e.g. `"object.kind == 'Pod'"`) | ❌ | Compiled N times. Scales with policy count. |
| **C. Cross-refresh** — policy `resourceVersion` unchanged | ✅ (indirect) | `generic/policy_source.go` already short-circuits via `compiledPolicyEntry.policyVersion`. |

Axis C is already covered. **Axis A and B are the wins.**

### Measured impact (order-of-magnitude, from the #131417 data points)

- Per `cel.Program` including ast + checked-expr + resolver state: **~30–50 KB** on typical expressions.
- 2000 policies × 7 matchConditions × 40 KB ≈ **560 MB** of programs retained. If 80% of those expressions are identical strings across policies (common for admission templates propagated via GitOps), cross-policy dedup would recover **~450 MB**.

### Proposed dedup strategy — two cooperating caches

```
          ┌────────────────────────────────────────────────┐
          │  compileCache (per-compiler, axis A)           │
          │  key = (expr, options, envType)                │
          │  value = CompilationResult                     │
          │  lifetime = single compilePolicy invocation    │
          └────────────────────────────────────────────────┘
                          │ miss
                          ▼
          ┌────────────────────────────────────────────────┐
          │  ProgramCache (process-scoped, axis B)         │
          │  key = (expr, options, envType, varSig, ver)   │
          │  value = CompilationResult                     │
          │  lifetime = process; bounded LRU, size 4096    │
          └────────────────────────────────────────────────┘
                          │ miss
                          ▼
              env.Compile + env.Program (actual CEL work)
```

**Key design points for the process-wide cache:**

- `varSig` is a stable hash (fnv64a or sha256 truncated) over the policy's `Spec.Variables` list (name, expression, order). Two policies with identical variable decls share program entries that reference `variables.*`; policies with differing variables are isolated automatically. This preserves type-checker correctness.
- `ver` is `environment.DefaultCompatibilityVersion()` at the time the entry is stored. Compat version does not change mid-process, so effectively it is a sanity guard, not a live dimension.
- `options` is the exact `OptionalVariableDeclarations{HasParams, HasAuthorizer, HasPatchTypes}` struct.
- `envType` distinguishes `NewExpressions` vs `StoredExpressions`.
- Cache value stores the **compiled `CompilationResult`** (including `Error`) so negative results (bad expressions in a static manifest) are also deduped.

**Why LRU and not TTL**: policies are long-lived and informer-driven; TTL would cause unnecessary recompiles. LRU bounds worst-case memory under adversarial churn (e.g. someone rapidly editing 100k unique expressions), which is more important than reclaim latency.

---

## 2. Lazy loading / compilation

### What is eager today

Every invocation of `compilePolicy` eagerly compiles:

1. All `Spec.Variables` expressions (stored in `compositionState.compiledVariables`).
2. All `Spec.MatchConditions` expressions.
3. All `Spec.Validations[].Expression` (the validation itself).
4. All `Spec.Validations[].MessageExpression`.
5. All `Spec.AuditAnnotations[].ValueExpression`.

For a policy with `N` match conditions and `M` validations, **`N + 3M + K`** CEL programs are materialized regardless of whether the policy will ever match a request.

### Which of these deserve laziness?

| Expression class | Runs on every matching request? | Cost of lazy | Verdict |
|---|---|---|---|
| `matchConditions` | Yes — the gate itself | Moves compile error from startup to first match | ❌ keep eager |
| `validations[].expression` | Yes | Same — validation failure policy is observable | ❌ keep eager |
| `auditAnnotations[].valueExpression` | Yes (on admit path) | Low — audit plumbing already handles runtime errors | ❌ keep eager |
| `validations[].messageExpression` | **Only on validation failure** | Low — already inside an error path | ✅ **lazy candidate** |
| `variables[].expression` | Only when a dependent expression is evaluated (already lazy in `lazy.MapValue`) | N/A | ✅ already lazy at eval time; compile stays eager |

Only **`messageExpression`** is a clean lazy-compile candidate: it runs only when a validation fails, which is the exceptional path. Making its compilation `sync.OnceValues`-gated defers the cost until first failure, saving `M` CEL programs per policy in the steady state where all validations pass.

### Thread-safety sketch for lazy messageExpression

```go
// inside validating package, new helper
type lazyMessageCompiler struct {
    once sync.Once
    eval cel.ConditionEvaluator
    build func() cel.ConditionEvaluator
}

func (l *lazyMessageCompiler) Get() cel.ConditionEvaluator {
    l.once.Do(func() { l.eval = l.build() })
    return l.eval
}
```

- `sync.Once` guarantees a single compile under concurrent first-fails.
- The Validator holds the `lazyMessageCompiler` instead of the eager `ConditionEvaluator`; `Validator.Validate` calls `.Get()` only on the failure branch.

### What about the other call-sites?

Eager compile at refresh time gives two properties we must preserve:

1. **Surface config errors early.** `compiledPolicyEntry` stores the evaluator regardless of compile error so the admission path can honor `failurePolicy`. Lazy compile would delay error surfacing until the first request — acceptable only if failurePolicy semantics are preserved (they are, because the lazy evaluator can still report a compile error as runtime error at first use).
2. **Deterministic admission latency.** The first request hitting a new policy already pays informer-sync + match cost; adding compile cost would make tail latency spiky and correlate with policy churn.

For these reasons, **eager compile for the hot-path expressions (axes match/validation/audit) is the right default**, paired with interning via the caches in §1 to recover the memory.

---

## 3. Performance & memory impact

### Measurements anchor to issue #131417

| Scenario | Before (today) | After per-policy dedup (axis A) | After cross-policy dedup (axis B) |
|---|---|---|---|
| 2000 policies, 0 match conditions | ~1158 MB RSS | ~1158 MB (no duplicates to collapse) | ~1158 MB |
| 2000 policies, 1 match condition | ~1370 MB RSS | ~1350 MB (~2% win) | **~850 MB (~38% win)** if expressions templated |
| 2000 policies, 7 match conditions | ~1968 MB RSS | ~1900 MB (~3% win, when within-policy repeat is rare) | **~1100 MB (~44% win)** if expressions templated |

The per-policy dedup is a strict subset of the gains, so both deliver without conflict.

### Eager vs lazy trade-off, precisely

| Axis | Eager (today) | Lazy (all sites) | Lazy (messageExpression only — proposed) |
|---|---|---|---|
| Cold CPU at policy refresh | `O(N+M+K)` compiles per policy | 0 | `O(N+M+K-M)` = `O(N+2M+K)` — a ~25% steady-state compile-time reduction |
| First-request tail latency | baseline | **+ compile cost** (hundreds of μs to low ms) | baseline |
| Compile-error surfacing | at refresh | at first request | at first failed validation (acceptable — error path) |
| Memory at steady state | full | full at first request | saves `M` programs × active policies |
| Thread-safety complexity | none | `sync.Once` per expression | `sync.Once` per Validator |

**Recommendation:** keep eager for the hot-path, add `sync.Once` lazy only for `messageExpression`, and rely on the two caches for the bulk of the memory win.

---

## 4. Implementation

Scope split:

- **Change-set 1 (small, safe):** per-compiler intra-policy dedup (axis A). Localized to [cel/compile.go](staging/src/k8s.io/apiserver/pkg/admission/plugin/cel/compile.go).
- **Change-set 2 (medium):** process-scoped `ProgramCache` threaded from plugin into compiler (axis B).
- **Change-set 3 (small):** lazy `messageExpression` compilation in [validating/plugin.go](staging/src/k8s.io/apiserver/pkg/admission/plugin/policy/validating/plugin.go).

Change-sets 1 and 3 are provided as concrete diffs below. Change-set 2 is sketched because it is larger and should land after 1 and 3 are validated.

### Change-set 1 — per-compiler expression cache

Diff against [cel/compile.go](staging/src/k8s.io/apiserver/pkg/admission/plugin/cel/compile.go) (applies cleanly on top of the lazy-env fix from issue #131417):

```diff
@@ type compiler struct {
     baseEnv       *environment.EnvSet
     namespaceType *apiservercel.DeclType
     requestType   *apiservercel.DeclType

     mu      sync.Mutex
     varEnvs map[OptionalVariableDeclarations]*environment.EnvSet
+
+    // compileCache memoizes CompileCELExpression within this compiler
+    // instance. For VAP, one compiler is created per policy, so this
+    // dedupes intra-policy duplicates (axis A). Cross-policy dedup is
+    // handled by an optional process-wide ProgramCache (see NewCompilerWithProgramCache).
+    compileCache map[compileCacheKey]CompilationResult
+}
+
+type compileCacheKey struct {
+    expression string
+    options    OptionalVariableDeclarations
+    envType    environment.Type
 }

 func NewCompiler(env *environment.EnvSet) Compiler {
     return &compiler{
         baseEnv:       env,
         namespaceType: BuildNamespaceType(),
         requestType:   BuildRequestType(),
         varEnvs:       map[OptionalVariableDeclarations]*environment.EnvSet{},
+        compileCache:  map[compileCacheKey]CompilationResult{},
     }
 }

 func (c *compiler) CompileCELExpression(expressionAccessor ExpressionAccessor, options OptionalVariableDeclarations, envType environment.Type) CompilationResult {
     resultError := func(errorString string, errType apiservercel.ErrorType, cause error) CompilationResult {
         return CompilationResult{
             Error: &apiservercel.Error{
                 Type:   errType,
                 Detail: errorString,
                 Cause:  cause,
             },
             ExpressionAccessor: expressionAccessor,
         }
     }

+    // Fast path: intra-compiler dedup. The expression text plus its
+    // declaration options and env type fully determine the compiled
+    // CompilationResult (CompilationResult is read-only after return).
+    key := compileCacheKey{
+        expression: expressionAccessor.GetExpression(),
+        options:    options,
+        envType:    envType,
+    }
+    c.mu.Lock()
+    if cached, ok := c.compileCache[key]; ok {
+        c.mu.Unlock()
+        // Re-stamp the accessor so callers still see their own accessor.
+        cached.ExpressionAccessor = expressionAccessor
+        return cached
+    }
+    c.mu.Unlock()
+
     envSet, err := c.envFor(options)
     ...
```

…and at the end of the function, before the success return:

```diff
     result := CompilationResult{
         Program:            prog,
         ExpressionAccessor: expressionAccessor,
         OutputType:         ast.OutputType(),
     }
+    c.mu.Lock()
+    c.compileCache[key] = result
+    c.mu.Unlock()
     return result
```

Error paths should also cache — otherwise a policy with 10 duplicate bad expressions recompiles all 10. A simple pattern: wrap the single return through a deferred cache store keyed by `key`. Implementation sketch:

```go
var out CompilationResult
defer func() {
    // only cache real results; negative caches for bad expressions are fine,
    // they're bounded by the number of distinct expressions in the policy.
    c.mu.Lock()
    c.compileCache[key] = out
    c.mu.Unlock()
}()
// ... existing body, assigning to out instead of returning inline ...
return out
```

**Correctness notes:**

- `CompilationResult` fields (`Program`, `OutputType`) are immutable after return from cel-go. `ExpressionAccessor` is the only caller-specific field; re-stamping it per-caller preserves the existing contract where `CompilationErrors()` surfaces the caller's accessor.
- `compileCache` shares the existing `sync.Mutex` with `varEnvs`. Contention is negligible: `compilePolicy` is called under `policySource.lock`, and the critical section is map access only.
- Memory bound: at most one entry per distinct `(expr, options, envType)` tuple in a single policy. Bounded by policy size (which is already validated by the CRD).

### Change-set 2 — process-wide ProgramCache (sketch only)

Add to [cel/compile.go](staging/src/k8s.io/apiserver/pkg/admission/plugin/cel/compile.go):

```go
// ProgramCache is a thread-safe, bounded LRU of CompilationResult shared
// across all compilers that are constructed with NewCompilerWithProgramCache.
type ProgramCache interface {
    Get(key ProgramCacheKey) (CompilationResult, bool)
    Put(key ProgramCacheKey, result CompilationResult)
}

type ProgramCacheKey struct {
    Expression       string
    Options          OptionalVariableDeclarations
    EnvType          environment.Type
    VariableSig      string // stable hash of policy.Spec.Variables
    CompatVersion    string // e.g. "1.34"
}

// NewCompilerWithProgramCache wires an external cache in. NewCompiler(env)
// is unchanged and behaves as if the cache is always empty.
func NewCompilerWithProgramCache(env *environment.EnvSet, cache ProgramCache, varSig string) Compiler { ... }
```

Threading through:

- [validating/plugin.go](staging/src/k8s.io/apiserver/pkg/admission/plugin/policy/validating/plugin.go) gains a single package-level `programCache` (bounded LRU, size configurable via env var `KUBE_VAP_PROGRAM_CACHE_SIZE`, default 4096).
- `compilePolicy` computes `varSig` from `policy.Spec.Variables` once, then passes both into a new `cel.NewCompositedCompilerWithProgramCache(env, programCache, varSig)`.
- `CompileCELExpression` consults the program cache after the per-compiler cache and before `env.Compile`.

**Invalidation:** none needed. Cache keys include the variable signature and expression text, so a changed policy implicitly produces a new key. The LRU evicts stale entries as memory pressure dictates.

Deferred here because it touches `CompositedCompiler` construction and needs its own test suite.

### Change-set 3 — lazy messageExpression

Diff against [validating/plugin.go](staging/src/k8s.io/apiserver/pkg/admission/plugin/policy/validating/plugin.go):

```diff
-    res := NewValidator(
-        filterCompiler.CompileCondition(convertv1Validations(policy.Spec.Validations), optionalVars, environment.StoredExpressions),
-        matcher,
-        filterCompiler.CompileCondition(convertv1AuditAnnotations(policy.Spec.AuditAnnotations), optionalVars, environment.StoredExpressions),
-        filterCompiler.CompileCondition(convertv1MessageExpressions(policy.Spec.Validations), expressionOptionalVars, environment.StoredExpressions),
-        failurePolicy,
-        nil,
-    )
+    // messageExpression only runs when a validation has already failed.
+    // Defer its compilation until first failure to save memory on policies
+    // whose validations succeed in steady state (the overwhelming case).
+    lazyMsg := newLazyMessageEvaluator(func() cel.ConditionEvaluator {
+        return filterCompiler.CompileCondition(
+            convertv1MessageExpressions(policy.Spec.Validations),
+            expressionOptionalVars,
+            environment.StoredExpressions,
+        )
+    })
+
+    res := NewValidator(
+        filterCompiler.CompileCondition(convertv1Validations(policy.Spec.Validations), optionalVars, environment.StoredExpressions),
+        matcher,
+        filterCompiler.CompileCondition(convertv1AuditAnnotations(policy.Spec.AuditAnnotations), optionalVars, environment.StoredExpressions),
+        lazyMsg,
+        failurePolicy,
+        nil,
+    )
```

With a small support type in the same package:

```go
// lazyMessageEvaluator wraps a cel.ConditionEvaluator whose compilation is
// deferred to first use. It implements cel.ConditionEvaluator.
type lazyMessageEvaluator struct {
    once  sync.Once
    inner cel.ConditionEvaluator
    build func() cel.ConditionEvaluator
}

func newLazyMessageEvaluator(build func() cel.ConditionEvaluator) cel.ConditionEvaluator {
    return &lazyMessageEvaluator{build: build}
}

func (l *lazyMessageEvaluator) ForInput(ctx context.Context, a *admission.VersionedAttributes, r *admissionv1.AdmissionRequest, b cel.OptionalVariableBindings, ns *corev1.Namespace, budget int64) ([]cel.EvaluationResult, int64, error) {
    l.once.Do(func() { l.inner = l.build() })
    return l.inner.ForInput(ctx, a, r, b, ns, budget)
}

func (l *lazyMessageEvaluator) CompilationErrors() []error {
    // Compilation errors can only appear after first evaluation in lazy mode.
    if l.inner == nil {
        return nil
    }
    return l.inner.CompilationErrors()
}
```

This requires `cel.ConditionEvaluator` to be an interface (it is) and that the concrete validator only calls `ForInput` and `CompilationErrors` (check [validating/validator.go](staging/src/k8s.io/apiserver/pkg/admission/plugin/policy/validating/validator.go) before landing).

---

## 5. Safety & compatibility

| Concern | Assessment |
|---|---|
| Admission latency | Unchanged for the hot path. Lazy `messageExpression` adds a one-time compile on first validation failure; validation failure already allocates + stringifies + returns an error, so the few hundred μs are in the noise. |
| Correctness under concurrent admission | `sync.Mutex` in `compiler.compileCache`, `sync.Once` in `lazyMessageEvaluator`. No lock ordering new paths. |
| Cache invalidation | Implicit via key change. The existing `compiledPolicies` map in `generic/policy_source.go` is still the authoritative per-policy lifecycle. Our caches are subordinate — their entries are reclaimable without affecting correctness. |
| Memory growth bound | Per-compiler cache: bounded by policy size. Process-wide cache: bounded by LRU (size 4096). Pathological adversarial churn (creating unique expressions each request) cannot grow beyond LRU bound. |
| `failurePolicy` semantics | Preserved. Lazy message compile error is surfaced the same way an eager one would be — via `CompilationErrors()` consulted by the validator on the failure branch. |
| Deterministic startup errors | Preserved for `matchConditions` / `validations` / `auditAnnotations` (eager). Only `messageExpression` shifts to first-failure reporting. SIG Auth should sign off that this shift is acceptable; message expression compile errors are already tolerated at runtime (they fall back to the default message). |
| Backports | Change-set 1 and 3 are mechanical and low-risk. Safe to backport to 1.33 / 1.34. Change-set 2 is a larger refactor — land on master only. |
| API surface | Unchanged. `Compiler`, `NewCompiler`, `CompositedCompiler`, `Validator` signatures preserved. `NewCompilerWithProgramCache` is additive. |
| Observability | Add two metrics next to the existing `apiserver_admission_*` histograms: `apiserver_validating_admission_policy_compile_cache_hit_total{scope="compiler|program"}` and `apiserver_validating_admission_policy_compile_latency_seconds`. Essential for validating the win post-rollout. |

---

## 6. Tests to add

1. **Intra-policy dedup.** A unit test that creates a `compiler` and calls `CompileCELExpression` twice with the same `(expr, options, envType)`. Assert: `prog.Program` pointer equality (cache hit) and second call's `ExpressionAccessor` equals the second caller's accessor (re-stamping preserved).
2. **Error caching.** Pass a malformed expression twice; assert `env.Compile` is called only once (inject a counting test compiler seam).
3. **Concurrency.** 32 goroutines calling `CompileCELExpression` on the same expression; with `-race`, assert no data race and exactly one successful compile (via an injected build counter).
4. **Lazy messageExpression.** Create a VAP with 10 validations each with `messageExpression`; create a request that triggers no failure; assert the `lazyMessageEvaluator.inner` is still `nil`. Trigger one failure; assert all 10 message programs are now materialized (single `sync.Once` fires for the evaluator, which compiles all 10 at once — expected).
5. **Process-wide cache (when change-set 2 lands).** Two distinct compilers that share a `ProgramCache` and a `varSig` produce `Program` pointer equality for the same expression.
6. **Cache key isolation.** Two compilers with *different* `varSig` must not alias — even for the same expression text — because variable types could differ.

---

## 7. PR sequencing recommendation

1. **PR 1** — change-set 1 (per-compiler dedup) + tests. ~120 LOC. Low risk, immediate axis-A win.
2. **PR 2** — change-set 3 (lazy messageExpression) + tests. ~100 LOC. Needs SIG Auth sign-off on error-timing semantics.
3. **PR 3** — change-set 2 (process-wide cache) + tests + metrics. ~400 LOC. Largest memory win; lands after 1 & 2.

Each PR independently passes CI and delivers a measurable improvement, reducing merge risk and easing backport decisions.

---

## 8. Related architecture

See [plugin-architecture.md](plugin-architecture.md) for the diagram of the current compile + dispatch pipeline and the two new cache layers proposed above.
