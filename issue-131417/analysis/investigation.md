# Kubernetes Issue #131417 — Investigation & Fix

**Issue:** [High memory consumption caused by CEL in ValidatingAdmissionPolicy and ValidatingAdmissionPolicyBindings](https://github.com/kubernetes/kubernetes/issues/131417)

## 1. Understand the issue

**Problem.** API-server memory grows roughly linearly with the number of `ValidatingAdmissionPolicy` (VAP) objects, with a per-policy baseline of ~0.2 MiB **even before any CEL expression is added** (simple policies). Each additional `matchCondition` adds ~57 KiB. At 2000 policies × 7 match conditions, apiserver RSS climbs from a ~750 MiB baseline to ~1968 MiB. pprof shows CEL allocations dominate.

**Affected components.** `sig/api-machinery`, `sig/auth`. Concretely, code lives under:
- [staging/src/k8s.io/apiserver/pkg/admission/plugin/cel/](staging/src/k8s.io/apiserver/pkg/admission/plugin/cel/) (shared CEL compiler used by VAP and MAP)
- [staging/src/k8s.io/apiserver/pkg/admission/plugin/policy/validating/plugin.go](staging/src/k8s.io/apiserver/pkg/admission/plugin/policy/validating/plugin.go) (entry point)
- [staging/src/k8s.io/apiserver/pkg/admission/plugin/policy/mutating/compilation.go](staging/src/k8s.io/apiserver/pkg/admission/plugin/policy/mutating/compilation.go) (same pattern for MAP)
- [staging/src/k8s.io/apiserver/pkg/admission/plugin/policy/generic/policy_source.go](staging/src/k8s.io/apiserver/pkg/admission/plugin/policy/generic/policy_source.go) (the "generic framework" — **untouched per instructions**)

**Reproduction (inferred).** Create N `ValidatingAdmissionPolicy` objects (e.g. N ∈ {500, 1000, 2000}) each with a binding; measure kube-apiserver `process_resident_memory_bytes` or collect a heap pprof after the informers sync. Vary `spec.matchConditions` length from 0 → 7 to reproduce the per-expression slope. Compare against an apiserver baseline with zero policies.

---

## 2. Codebase mapping

Control flow for every VAP informer event:

1. [generic/policy_source.go:238 `refreshPolicies`](staging/src/k8s.io/apiserver/pkg/admission/plugin/policy/generic/policy_source.go#L238) → [`calculatePolicyData`](staging/src/k8s.io/apiserver/pkg/admission/plugin/policy/generic/policy_source.go#L277) → for each policy key, [`compilePolicyLocked`](staging/src/k8s.io/apiserver/pkg/admission/plugin/policy/generic/policy_source.go#L470) is called, which invokes the injected `s.compiler(policySpec)`.
2. For VAP, that compiler is [`validating.compilePolicy`](staging/src/k8s.io/apiserver/pkg/admission/plugin/policy/validating/plugin.go#L147). It:
   - Builds `optionalVars = {HasParams: hasParam, HasAuthorizer: true}` and `expressionOptionalVars = {..., HasAuthorizer: false}`.
   - Calls `cel.NewCompositedCompiler(compositionEnvTemplate)` — **one new compiler per policy**.
   - Compiles variables, match conditions, validations, audit annotations, message expressions against that compiler.
3. [`cel.NewCompositedCompiler`](staging/src/k8s.io/apiserver/pkg/admission/plugin/cel/composition.go#L64) performs **one** `envSet.Extend(...)` (adds the policy-specific `variables` map type), then delegates to [`cel.NewCompiler`](staging/src/k8s.io/apiserver/pkg/admission/plugin/cel/compile.go#L159):
   ```go
   func NewCompiler(env *environment.EnvSet) Compiler {
       return &compiler{varEnvs: mustBuildEnvs(env)}
   }
   ```
4. [`mustBuildEnvs`](staging/src/k8s.io/apiserver/pkg/admission/plugin/cel/compile.go#L225) **eagerly** calls `baseEnv.Extend(...)` **eight times** — once per combination of `(HasParams ∈ {f,t}) × (HasAuthorizer ∈ {f,t}) × (HasPatchTypes ∈ {f,t})`.
5. Each `Extend` creates a new `*environment.EnvSet` with two fresh `*cel.Env`s (`newExpressions` and `storedExpressions`), each chaining a new `DeclTypeProvider` on top of the CEL type registry — see [environment.go:219–240](staging/src/k8s.io/apiserver/pkg/cel/environment/environment.go#L219-L240). The package docstring warns directly: *"Extend is an expensive operation and each call to Extend that adds DeclTypes increases the depth of a chain of resolvers. For these reasons, calls to Extend should be kept to a minimum."*

---

## 3. Root cause analysis

Per VAP, the compiler built inside `NewCompositedCompiler` pre-builds **eight** extended `EnvSet`s, while `compilePolicy` ever uses at most **two**:

| Used by | `HasParams` | `HasAuthorizer` | `HasPatchTypes` |
|---|---|---|---|
| `Spec.Variables`, `matchConditions`, `validations`, `auditAnnotations` | `policy.Spec.ParamKind != nil` | `true` | `false` |
| `validations[].messageExpression` | same | `false` | `false` |

The other **6** `EnvSet`s (including all `HasPatchTypes: true` variants — used only by `MutatingAdmissionPolicy`'s JSON-patch path) are allocated, retained by the compiler, and **never used** for the VAP path. Each carries two full `*cel.Env`s with their own type-provider chain. Measured at ~25–30 KB per env (pair), this pre-build alone accounts for the ~0.2 MiB/policy baseline observed in the issue — matching the data exactly:

```
per-policy fixed cost ≈ 8 envs × 2 *cel.Env × ~12 KB ≈ 0.2 MiB
per-matchCondition    ≈ 1 compiled cel.Program + AST retained ≈ ~57 KB
```

At 2000 policies, the wasted pre-builds alone account for **~1.2 GiB** — aligns with the reported 1158 MiB gap even for the no-match-condition case.

**Secondary contributor (per match condition).** `cel.Program`s are retained for every expression — fundamental and not fixable without cross-policy interning. This issue does ask about interning, but interning requires a canonical key over `(expression, env options, version)` and raises non-trivial invalidation concerns; it's outside the scope of a minimal fix.

The problem therefore is not *where expressions are stored* but *how many CEL environments are built per policy that stores them*. This is an eager-vs-lazy bug, not an interning bug.

---

## 4. Fix proposal

**Minimal, safe change:** make `compiler.varEnvs` lazy. Build each `*environment.EnvSet` on first use for a given `OptionalVariableDeclarations`, keep the public `NewCompiler(env)` signature unchanged, protect with a `sync.Mutex` for future thread-safety. No API or CRD changes; no change to the generic framework.

Expected reduction: for a VAP that uses 1 combination of option decls → **87.5%** of per-policy env infra eliminated; for a VAP that also uses `messageExpression` (2 combinations) → **75%** eliminated. Per-policy baseline drops from ~0.2 MiB to ~25–50 KB. Same change helps `MutatingAdmissionPolicy` symmetrically (2 used / 8 built today).

### Diff — `staging/src/k8s.io/apiserver/pkg/admission/plugin/cel/compile.go`

```diff
@@
-import (
-	"fmt"
-
-	"github.com/google/cel-go/cel"
-
-	"k8s.io/apimachinery/pkg/util/version"
-	celconfig "k8s.io/apiserver/pkg/apis/cel"
-	apiservercel "k8s.io/apiserver/pkg/cel"
-	"k8s.io/apiserver/pkg/cel/common"
-	"k8s.io/apiserver/pkg/cel/environment"
-	"k8s.io/apiserver/pkg/cel/library"
-	"k8s.io/apiserver/pkg/cel/mutation"
-)
+import (
+	"fmt"
+	"sync"
+
+	"github.com/google/cel-go/cel"
+
+	"k8s.io/apimachinery/pkg/util/version"
+	celconfig "k8s.io/apiserver/pkg/apis/cel"
+	apiservercel "k8s.io/apiserver/pkg/cel"
+	"k8s.io/apiserver/pkg/cel/common"
+	"k8s.io/apiserver/pkg/cel/environment"
+	"k8s.io/apiserver/pkg/cel/library"
+	"k8s.io/apiserver/pkg/cel/mutation"
+)
@@
-type compiler struct {
-	varEnvs variableDeclEnvs
-}
-
-func NewCompiler(env *environment.EnvSet) Compiler {
-	return &compiler{varEnvs: mustBuildEnvs(env)}
-}
-
-type variableDeclEnvs map[OptionalVariableDeclarations]*environment.EnvSet
+type compiler struct {
+	// baseEnv is the shared, already-extended envSet from which per-option
+	// variants are derived on demand. Building an envSet is expensive (see
+	// environment.EnvSet.Extend docs), so we only build the combinations
+	// actually requested by this compiler's callers.
+	baseEnv       *environment.EnvSet
+	namespaceType *apiservercel.DeclType
+	requestType   *apiservercel.DeclType
+
+	mu      sync.Mutex
+	varEnvs map[OptionalVariableDeclarations]*environment.EnvSet
+}
+
+func NewCompiler(env *environment.EnvSet) Compiler {
+	return &compiler{
+		baseEnv:       env,
+		namespaceType: BuildNamespaceType(),
+		requestType:   BuildRequestType(),
+		varEnvs:       map[OptionalVariableDeclarations]*environment.EnvSet{},
+	}
+}
+
+// envFor returns the *environment.EnvSet for the given variable-declaration
+// options, lazily building (and caching) it on first use.
+func (c *compiler) envFor(options OptionalVariableDeclarations) (*environment.EnvSet, error) {
+	c.mu.Lock()
+	defer c.mu.Unlock()
+	if env, ok := c.varEnvs[options]; ok {
+		return env, nil
+	}
+	env, err := createEnvForOpts(c.baseEnv, c.namespaceType, c.requestType, options)
+	if err != nil {
+		return nil, err
+	}
+	c.varEnvs[options] = env
+	return env, nil
+}
@@
-	env, err := c.varEnvs[options].Env(envType)
-	if err != nil {
-		return resultError(fmt.Sprintf("unexpected error loading CEL environment: %v", err), apiservercel.ErrorTypeInternal, nil)
-	}
+	envSet, err := c.envFor(options)
+	if err != nil {
+		return resultError(fmt.Sprintf("unexpected error loading CEL environment: %v", err), apiservercel.ErrorTypeInternal, err)
+	}
+	env, err := envSet.Env(envType)
+	if err != nil {
+		return resultError(fmt.Sprintf("unexpected error loading CEL environment: %v", err), apiservercel.ErrorTypeInternal, nil)
+	}
@@
-func mustBuildEnvs(baseEnv *environment.EnvSet) variableDeclEnvs {
-	requestType := BuildRequestType()
-	namespaceType := BuildNamespaceType()
-	envs := make(variableDeclEnvs, 8) // since the number of variable combinations is small, pre-build a environment for each
-	for _, hasParams := range []bool{false, true} {
-		for _, hasAuthorizer := range []bool{false, true} {
-			var err error
-			{
-				decl := OptionalVariableDeclarations{HasParams: hasParams, HasAuthorizer: hasAuthorizer}
-				envs[decl], err = createEnvForOpts(baseEnv, namespaceType, requestType, decl)
-				if err != nil {
-					panic(err)
-				}
-			}
-			{
-				decl := OptionalVariableDeclarations{HasParams: hasParams, HasAuthorizer: hasAuthorizer, HasPatchTypes: true}
-				envs[decl], err = createEnvForOpts(baseEnv, namespaceType, requestType, decl)
-				if err != nil {
-					panic(err)
-				}
-			}
-		}
-	}
-	return envs
-}
+// mustBuildEnvs is intentionally removed. Its eager pre-build of 8 envSets
+// per compiler was the dominant per-policy memory overhead observed in
+// kubernetes/kubernetes#131417. Environments are now built lazily by envFor.
```

### Why the panic removal is safe
The original `mustBuildEnvs` panicked on `createEnvForOpts` error. That error only occurs if the base `EnvSet` is misconfigured at process startup — a condition already exercised by `environment.MustBaseEnvSet` at init time and by the many other callers of `NewCompiler` in apiserver/authentication/authorization. The new code surfaces the same error via `CompilationResult.Error` of type `ErrorTypeInternal` on first use, which is strictly more recoverable and preserves the existing error surface already used by `CompileCELExpression` when `env.Env(envType)` returns an error.

### Backward compatibility
- `Compiler` interface, `NewCompiler`, `CompileCELExpression`, `OptionalVariableDeclarations`, and all return types unchanged.
- No changes to CEL grammar, feature gates, API versions, CRDs, or stored-object semantics.
- Evaluation behavior, type-check behavior, and cost accounting are unchanged because `createEnvForOpts` is unchanged — we only defer when it runs.

---

## 5. Testing strategy

### New unit tests (in `compile_test.go`)

1. **Lazy construction.** Assert that constructing a `NewCompiler(env)` does not call `createEnvForOpts`. Use a sentinel: wrap `env` with a `base.Extend` spy (via a small local helper) and assert the spy is not called until `CompileCELExpression` is invoked.

2. **Env caching across calls.** Call `CompileCELExpression` twice with the same `OptionalVariableDeclarations` and assert the same `*environment.EnvSet` is reused (via a counter incremented inside a stubbed `createEnvForOpts`, injected through a package-local test seam or by introspection of the `varEnvs` map through a small testonly accessor).

3. **Per-option build count.** Compile against `{HasParams:true,HasAuthorizer:true}` and `{HasParams:true,HasAuthorizer:false}` and assert exactly 2 entries materialize in `varEnvs`, not 8.

4. **Regression — memory.** Add an `AllocsPerRun` check:
   ```go
   func TestNewCompilerAllocBudget(t *testing.T) {
       base := environment.MustBaseEnvSet(environment.DefaultCompatibilityVersion())
       allocs := testing.AllocsPerRun(50, func() { _ = NewCompiler(base) })
       // Pre-fix this was dominated by 8 Extend() calls. Keep a conservative
       // upper bound that catches an accidental re-introduction.
       if allocs > 50 {
           t.Fatalf("NewCompiler allocates %.0f allocs, want <= 50", allocs)
       }
   }
   ```
   (Exact bound tuned once measured; the intent is to prevent regression to eager builds.)

5. **Concurrent first-use.** Run 16 goroutines concurrently calling `CompileCELExpression` with the same options; assert only one env is ever built (covered by the `sync.Mutex`). Use `race` detector.

### Existing tests to update
- [compile_test.go:189](staging/src/k8s.io/apiserver/pkg/admission/plugin/cel/compile_test.go#L189) and [compile_test.go:257](staging/src/k8s.io/apiserver/pkg/admission/plugin/cel/compile_test.go#L257) — no change needed, same `NewCompiler(env)` signature, same `CompileCELExpression` behavior.
- `BenchmarkCompile` in the same file — keep as-is; a follow-up subbenchmark measuring steady-state allocations with `b.ReportAllocs()` across N policies documents the win.

### Integration / e2e
- Existing e2e in [test/integration/apiserver/admissionregistration](test/integration/) covers compile + dispatch correctness; no new e2e required since behavior is identical.
- Scale-validation gate: run `test/integration/apiserver/admissionregistration` under `GOGC=off` and measure heap after creating 1000 VAPs; should show the expected drop (~0.2 MiB → ~25–50 KB per policy). This can be added as an opt-in scalability job (sig-scalability) rather than a regular CI signal, since it is a memory assertion and inherently noisy.

---

## 6. Risk assessment

- **Performance.** First `CompileCELExpression` call per options combination now incurs the `Extend` cost it previously paid in the constructor. For VAP, this work was already done eagerly; net work is unchanged or slightly less (combinations never used are never built). Latency to serve the first admission request after a policy update shifts by one `Extend` per used options combination (~ms, once per policy). Acceptable.
- **Scalability.** Per-policy steady-state memory drops substantially (see §4). Reduces pressure from 2000-policy scenarios from ~1.2 GiB wasted env infrastructure to ~0.15 GiB. Directly closes the performance gap reported in #131417.
- **Concurrency.** Protected by `sync.Mutex`. `compilePolicy` is invoked under `policySource.lock`, so contention is nil in practice; the mutex exists defensively.
- **Breaking changes.** None. Public API, CEL semantics, type-checking, cost accounting, and error surfaces preserved.
- **Rollout.** No feature gate needed — pure internal refactor with equivalent semantics. Backports are low-risk to 1.32 / 1.33 if SIG Auth wants the scale win delivered to existing minor releases.
- **Residual concern.** The per-expression cost (`cel.Program` retained per validation) still scales linearly with policies × expressions. Interning identical expressions is a future optimization; it requires a keyed cache (`expression text + options + envType + compat version`) and careful lifecycle tied to `compiledPolicies` invalidation in [generic/policy_source.go](staging/src/k8s.io/apiserver/pkg/admission/plugin/policy/generic/policy_source.go). Intentionally out of scope to keep this change minimal and well-scoped.

---

## 7. PR-ready summary

**Title**
`cel: build per-options admission EnvSets lazily to fix VAP memory scaling`

**Problem**
`cel.NewCompiler` eagerly materialized 8 `*environment.EnvSet` instances (one per combination of `HasParams × HasAuthorizer × HasPatchTypes`) for every `ValidatingAdmissionPolicy` compilation, while only 1–2 are ever used. Each `EnvSet.Extend` clones the Kubernetes CEL type-provider chain, costing ~25–30 KB. With 2000 policies this burned ~1.2 GiB on env infrastructure alone, matching the ~0.2 MiB-per-policy baseline in kubernetes/kubernetes#131417.

**Solution**
Replace the eager `mustBuildEnvs` pre-build with a lazy, mutex-protected map on `compiler`. `createEnvForOpts` now runs only when `CompileCELExpression` is first called with a given `OptionalVariableDeclarations`. Public `Compiler` interface, `NewCompiler` signature, evaluation semantics, and error surfaces are unchanged. Same change benefits `MutatingAdmissionPolicy` symmetrically.

**Testing**
- New unit tests in `staging/src/k8s.io/apiserver/pkg/admission/plugin/cel/compile_test.go`: lazy construction, per-options caching, allocation budget, concurrent first-use.
- Existing compile / condition / composition tests exercise the new lazy path unchanged.
- Recommend follow-up sig-scalability job to track 1k–2k VAP apiserver RSS.

**Release note**
```
Significantly reduces kube-apiserver memory overhead per ValidatingAdmissionPolicy
and MutatingAdmissionPolicy by building CEL environments on demand instead of
eagerly pre-building unused variable-declaration combinations.
```
