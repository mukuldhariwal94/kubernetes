# ValidatingAdmissionPolicy in `kube-apiserver` — Architecture Reference

> A complete, code-grounded walkthrough of how `ValidatingAdmissionPolicy` (VAP) is wired into `kube-apiserver` — from API registration, through informer-driven compilation, to per-request CEL evaluation and the final admit/deny decision.
>
> Every claim cites a file:line in the kubernetes source tree (paths relative to repo root). Where sentences make architectural claims, the citation is the authority — read the code, not the prose.

---

## Table of Contents

1. [Where VAP Sits in `kube-apiserver`](#1-where-vap-sits-in-kube-apiserver)
2. [Object Model](#2-object-model)
3. [Layered Component View](#3-layered-component-view)
4. [Plugin Lifecycle](#4-plugin-lifecycle)
5. [Control Plane — Compilation Pipeline](#5-control-plane--compilation-pipeline)
6. [Data Plane — Per-Request Evaluation](#6-data-plane--per-request-evaluation)
7. [CEL Subsystem](#7-cel-subsystem)
8. [Matcher Subsystem](#8-matcher-subsystem)
9. [Param Resolution](#9-param-resolution)
10. [Failure Policy and Validation Actions](#10-failure-policy-and-validation-actions)
11. [Type Checking](#11-type-checking)
12. [Concurrency Model](#12-concurrency-model)
13. [Memory Model + Cache](#13-memory-model--cache)
14. [End-to-End Sequence Diagram](#14-end-to-end-sequence-diagram)
15. [Source File Reference Map](#15-source-file-reference-map)

---

## 1. Where VAP Sits in `kube-apiserver`

VAP is one plugin in the `kube-apiserver` admission chain. Every API write travels through the chain after authentication/authorization and before storage:

```
              ┌───────────────────────────────────────────────────────────────┐
              │                       kube-apiserver                          │
              └───────────────────────────────────────────────────────────────┘
client ─HTTP─►   AuthN   ►   AuthZ   ►   Mutating   ►   Validating   ►  storage
                                            chain          chain
                                              │              │
                                              ▼              ▼
                                          [...other      ┌──────────────────────┐
                                           plugins]      │ ValidatingAdmission  │
                                                         │ Policy plugin        │  ← THIS DOC
                                                         │ + Webhook plugin     │
                                                         │ + ResourceQuota      │
                                                         │ + ...                │
                                                         └──────────────────────┘
```

The validating plugin is registered with the global `admission.Plugins` registry at process startup ([validating/plugin.go:60-64](staging/src/k8s.io/apiserver/pkg/admission/plugin/policy/validating/plugin.go#L60-L64)):

```go
func Register(plugins *admission.Plugins) {
    plugins.Register(PluginName, func(configFile io.Reader) (admission.Interface, error) {
        return NewPlugin(configFile)
    })
}
```

The plugin name `"ValidatingAdmissionPolicy"` ([plugin.go:43-44](staging/src/k8s.io/apiserver/pkg/admission/plugin/policy/validating/plugin.go#L43-L44)) is included by default in the apiserver's enabled-plugins list whenever the corresponding feature gate is on.

Two interfaces are implemented ([plugin.go:76-79](staging/src/k8s.io/apiserver/pkg/admission/plugin/policy/validating/plugin.go#L76-L79)):

```go
var _ admission.Interface           = &Plugin{}
var _ admission.ValidationInterface = &Plugin{}
```

Only `Validate(...)` is meaningful — VAP does not mutate (mutation is its sibling plugin `MutatingAdmissionPolicy`).

---

## 2. Object Model

VAP introduces four user-facing API kinds, all in `admissionregistration.k8s.io/v1`:

| Kind | Purpose | Cluster-scoped? |
|---|---|---|
| `ValidatingAdmissionPolicy` | The policy *definition*: CEL expressions, match constraints, param schema reference | Yes |
| `ValidatingAdmissionPolicyBinding` | Binds a policy to a *scope* (namespace/object selectors) and a concrete param object | Yes |
| `<Custom param kind>` | Optional CRD or built-in resource referenced by the policy's `paramKind` | Either |
| `MatchCondition` | Inline CEL precondition list inside a policy | (not a separate kind) |

Type aliases at the plugin level pin the generic framework to these v1 types ([plugin.go:67-70](staging/src/k8s.io/apiserver/pkg/admission/plugin/policy/validating/plugin.go#L67-L70)):

```go
type Policy           = v1.ValidatingAdmissionPolicy
type PolicyBinding    = v1.ValidatingAdmissionPolicyBinding
type PolicyEvaluator  = Validator
type PolicyHook       = generic.PolicyHook[*Policy, *PolicyBinding, PolicyEvaluator]
```

A `PolicyHook` ([generic/policy_source.go:93-104](staging/src/k8s.io/apiserver/pkg/admission/plugin/policy/generic/policy_source.go#L93-L104)) is the runtime triple `(Policy, []Binding, Evaluator)` — the unit the dispatcher iterates over per request.

Inside a `Policy.Spec` the relevant fields are:

- `MatchConstraints` — resource rules + namespace/object selectors (matched once per request)
- `MatchConditions` — inline CEL preconditions (evaluated per request as a fast-path filter)
- `Validations` — the actual CEL rules whose boolean result decides admit/deny
- `AuditAnnotations` — CEL expressions producing audit annotation values
- `Variables` — named CEL sub-expressions accessible as `variables.<name>` from later expressions
- `ParamKind` — optional reference to a parameter resource type
- `FailurePolicy` — `Fail` (default) or `Ignore` for compile/runtime errors

A `Binding.Spec` adds:

- `PolicyName` — fk to the policy
- `MatchResources` — additional matcher (intersection with policy's `MatchConstraints`)
- `ParamRef` — concrete param to inject as `params` CEL variable
- `ValidationActions` — `Deny`, `Audit`, `Warn` (combinable)

---

## 3. Layered Component View

```
┌─────────────────────────────────────────────────────────────────────────────────────┐
│ admission.Interface (kube-apiserver façade)                                         │
│ ─────────────────────────────────────────────────────────────────────────────────── │
│  validating.Plugin                                                                  │
│    embeds → generic.Plugin[PolicyHook]                  ◄── shared with MAP plugin  │
│                                                                                     │
│      Source[H]      = generic.policySource[*Policy,*Binding,Validator]              │
│      Dispatcher[H]  = generic.policyDispatcher[...] → validating.dispatcher         │
│      Matcher        = matching.Matcher  (resource rules + selectors)                │
└─────────────────────────────────────────────────────────────────────────────────────┘
            │                                                       │
            │ control plane (compile)                               │ data plane (validate)
            ▼                                                       ▼
┌──────────────────────────────────┐               ┌────────────────────────────────────────┐
│ Informers                        │               │ Per-request:                           │
│   - VAP informer                 │               │   1. Plugin.Validate(attrs)            │
│   - VAPB informer                │               │   2. generic.Plugin.Dispatch           │
│   - Param CRD informers (lazy)   │               │   3. policyDispatcher.Dispatch         │
│ ↓ notify on change               │               │   4. validating.dispatcher.Dispatch    │
│                                  │               │   5. validator.Validate per (P,B,Param)│
│ refreshPolicies (1Hz)            │               │   6. CEL eval for matchCond/valid/audit│
│   → compilePolicy(P)             │               │   7. Apply ValidationActions (D/A/W)   │
│   → cache validators             │               │   8. Aggregate decisions → admit/deny  │
└──────────────────────────────────┘               └────────────────────────────────────────┘
            │                                                       │
            └──────────────────► validating.Validator ◄─────────────┘
                              (compiled CEL programs)

                                   │   uses
                                   ▼
┌─────────────────────────────────────────────────────────────────────────────────────┐
│ CEL subsystem (apiserver/pkg/admission/plugin/cel/*)                                │
│  • CompositedCompiler   — per-policy facade (variables + conditions + mutations)    │
│  • Compiler             — turns expression text into cel.Program                    │
│  • compileCache (LRU)   — process-wide, by structural fingerprint                   │
│  • Activation           — runtime variable resolver per evaluation                  │
│  • environment.EnvSet   — base type system (StoredExpressions / NewExpressions)     │
└─────────────────────────────────────────────────────────────────────────────────────┘
                                   │   uses
                                   ▼
              github.com/google/cel-go (Compile, Program, Eval)
```

---

## 4. Plugin Lifecycle

### 4.1 Construction

`NewPlugin()` ([plugin.go:109-141](staging/src/k8s.io/apiserver/pkg/admission/plugin/policy/validating/plugin.go#L109-L141)) builds a `validating.Plugin` whose embedded `generic.Plugin[PolicyHook]` is parameterized with three factory closures:

1. **Source factory** ([plugin.go:121-132](staging/src/k8s.io/apiserver/pkg/admission/plugin/policy/validating/plugin.go#L121-L132)) — produces a `policySource` that knows how to read VAPs and VAPBs from informers and how to compile each.
2. **Dispatcher factory** ([plugin.go:133-138](staging/src/k8s.io/apiserver/pkg/admission/plugin/policy/validating/plugin.go#L133-L138)) — produces the validating dispatcher (vs. mutating).
3. **Compiler closure** — `compilePolicy` ([plugin.go:147-181](staging/src/k8s.io/apiserver/pkg/admission/plugin/policy/validating/plugin.go#L147-L181)), called per-policy by the source.

The generic `Plugin[H]` ([generic/plugin.go:69-110](staging/src/k8s.io/apiserver/pkg/admission/plugin/policy/generic/plugin.go#L69-L110)) holds the source, dispatcher, matcher, and a feature-gate flag.

### 4.2 Initialization

`ValidateInitialization()` ([generic/plugin.go:203](staging/src/k8s.io/apiserver/pkg/admission/plugin/policy/generic/plugin.go#L203)) is called by the apiserver after dependency-injection (SharedInformerFactory, RESTMapper, Authorizer, etc.) has been wired in. It:

1. Builds the namespace/object `matching.Matcher`.
2. Instantiates the source via the factory.
3. Instantiates the dispatcher via the factory.
4. Calls `source.Run(ctx)` and `dispatcher.Start(ctx)` in background goroutines.

After this returns, the plugin is **active**: every admission request reaches `Plugin.Validate(...)`.

### 4.3 Singleton CEL Env Template

A process-wide `sync.Once` ([plugin.go:47-57](staging/src/k8s.io/apiserver/pkg/admission/plugin/policy/validating/plugin.go#L47-L57)) builds the base `environment.EnvSet` exactly once:

```go
func getCompositionEnvTemplateWithStrictCost() *environment.EnvSet {
    lazyCompositionEnvTemplateWithStrictCostInit.Do(func() {
        lazyCompositionEnvTemplateWithStrictCost = environment.MustBaseEnvSet(environment.DefaultCompatibilityVersion())
    })
    return lazyCompositionEnvTemplateWithStrictCost
}
```

This pointer becomes the cache-key identity used by Patch 0001's program cache (see §13). It is the **only** env that is shared across policies today; everything below it is reconstructed per-policy.

---

## 5. Control Plane — Compilation Pipeline

The control plane's job is: *whenever a VAP/VAPB/Param object changes, recompute the set of `PolicyHook`s that the data plane will iterate over.*

### 5.1 Informer Wiring

`policySource` ([generic/policy_source.go:52-91](staging/src/k8s.io/apiserver/pkg/admission/plugin/policy/policy/generic/policy_source.go#L52-L91)) holds:

```go
type policySource[P, B, E] struct {
    policyInformer  generic.Informer[P]            // VAP
    bindingInformer generic.Informer[B]            // VAPB
    policies        atomic.Pointer[[]PolicyHook[P, B, E]]   // active set
    compiledPolicies map[types.NamespacedName]compiledPolicyEntry[E]
    paramsCRDControllers map[schema.GroupVersionKind]*paramInfo
    ...
}
```

Both informers register `notify()` ([generic/policy_source.go:266-267](staging/src/k8s.io/apiserver/pkg/admission/plugin/policy/generic/policy_source.go#L266-L267)) on Add/Update/Delete — `notify()` simply does `policiesDirty.Store(true)`. **No compilation runs in the informer goroutine.**

### 5.2 The Refresh Loop

`Run()` ([generic/policy_source.go:146](staging/src/k8s.io/apiserver/pkg/admission/plugin/policy/generic/policy_source.go#L146)) starts both informers, blocks until cache sync, then `wait.Until(refreshPolicies, 1*time.Second, ctx.Done())`.

`refreshPolicies()` is the only code path that recompiles policies. It checks `policiesDirty`; if set, it calls `calculatePolicyData()`.

### 5.3 `calculatePolicyData` — the heart of compilation

[generic/policy_source.go:277-365](staging/src/k8s.io/apiserver/pkg/admission/plugin/policy/generic/policy_source.go#L277-L365) does, under the source's mutex:

1. List all policies and bindings from the informers.
2. Group bindings by `Spec.PolicyName` → `policyToBindings map[NamespacedName][]*Binding`.
3. For each policy:
   a. Call `compilePolicyLocked(policy)` ([line 470](staging/src/k8s.io/apiserver/pkg/admission/plugin/policy/generic/policy_source.go#L470)).
   b. Resolve the param informer via `ensureParamsForPolicyLocked(policy)` ([line 393](staging/src/k8s.io/apiserver/pkg/admission/plugin/policy/generic/policy_source.go#L393)).
   c. Construct a `PolicyHook` with policy + bindings + evaluator + param informer + any setup error.
4. Atomically swap the new `[]PolicyHook` into `s.policies`.

### 5.4 `compilePolicyLocked` — the per-policy cache

```go
// generic/policy_source.go:470 (paraphrased)
entry, ok := s.compiledPolicies[NamespacedName{policy.Namespace, policy.Name}]
if ok && entry.policyVersion == policy.ResourceVersion {
    return entry.evaluator        // cache hit by ResourceVersion
}
ev := s.compiler(policy)          // → validating.compilePolicy
s.compiledPolicies[NamespacedName] = compiledPolicyEntry{policyVersion, ev}
return ev
```

So a policy's `validator` is **rebuilt only when `ResourceVersion` changes**. An unchanged policy passes through untouched even if some other policy in the cluster triggered a refresh.

### 5.5 `compilePolicy` — the actual CEL work

[validating/plugin.go:147-181](staging/src/k8s.io/apiserver/pkg/admission/plugin/policy/validating/plugin.go#L147-L181):

```go
func compilePolicy(policy *Policy) Validator {
    optionalVars             := cel.OptionalVariableDeclarations{HasParams: hasParam, HasAuthorizer: true}
    expressionOptionalVars   := cel.OptionalVariableDeclarations{HasParams: hasParam, HasAuthorizer: false}
    failurePolicy            := policy.Spec.FailurePolicy
    matchConditions          := policy.Spec.MatchConditions

    compositionEnvTemplate := getCompositionEnvTemplateWithStrictCost()       // singleton
    filterCompiler, _      := cel.NewCompositedCompiler(compositionEnvTemplate)  // ★ per-policy ★
    filterCompiler.CompileAndStoreVariables(convertv1beta1Variables(policy.Spec.Variables), optionalVars, environment.StoredExpressions)

    if len(matchConditions) > 0 {
        matcher = matchconditions.NewMatcher(filterCompiler.CompileCondition(matchExprAccessors, optionalVars, environment.StoredExpressions), failurePolicy, "policy", "validate", policy.Name)
    }
    return NewValidator(
        filterCompiler.CompileCondition(convertv1Validations(policy.Spec.Validations),         optionalVars, environment.StoredExpressions),
        matcher,
        filterCompiler.CompileCondition(convertv1AuditAnnotations(policy.Spec.AuditAnnotations), optionalVars, environment.StoredExpressions),
        filterCompiler.CompileCondition(convertv1MessageExpressions(policy.Spec.Validations),    expressionOptionalVars, environment.StoredExpressions),
        failurePolicy,
        nil,
    )
}
```

This is where all the per-policy memory cost from issue #131417 is paid. Each `cel.NewCompositedCompiler` call triggers ~13 `EnvSet.Extend` calls (= 26 `cel.Env.Extend` clones), and each `CompileCondition` call invokes `env.Compile + env.Program` per expression (~30–200 KB per program).

### 5.6 The `validator` Returned

[validating/validator.go:42-50](staging/src/k8s.io/apiserver/pkg/admission/plugin/policy/validating/validator.go#L42-L50):

```go
type validator struct {
    celMatcher              matchconditions.Matcher    // optional preconditions
    validationFilter        cel.ConditionEvaluator     // the rules
    messageFilter           cel.ConditionEvaluator     // dynamic messages
    auditAnnotationFilter   cel.ConditionEvaluator     // audit metadata
    failPolicy              *v1.FailurePolicyType
    compileError            error                      // surfaced at first request if non-nil
}
```

This struct is what gets stored in `compiledPolicies[NamespacedName]` and what every admission request runs against.

---

## 6. Data Plane — Per-Request Evaluation

### 6.1 Entry: `Plugin.Validate`

[validating/plugin.go:143-145](staging/src/k8s.io/apiserver/pkg/admission/plugin/policy/validating/plugin.go#L143-L145):

```go
func (a *Plugin) Validate(ctx context.Context, attr admission.Attributes, o admission.ObjectInterfaces) error {
    return a.Plugin.Dispatch(ctx, attr, o)        // delegate to generic.Plugin
}
```

### 6.2 `generic.Plugin.Dispatch`

[generic/plugin.go:302-345](staging/src/k8s.io/apiserver/pkg/admission/plugin/policy/generic/plugin.go#L302-L345):

1. Short-circuit: if the feature gate is off → return nil.
2. Exclude resources the apiserver wants invisible (admission registration objects themselves, etc.).
3. Atomically read the current hook list: `hooks := a.source.Hooks()`.
4. Call `a.dispatcher.Dispatch(ctx, attr, o, hooks)`.

`source.Hooks()` ([generic/policy_source.go:225-235](staging/src/k8s.io/apiserver/pkg/admission/plugin/policy/generic/policy_source.go#L225-L235)) is a single `s.policies.Load()` — **lock-free read**, the whole point of the atomic-pointer pattern. The data plane never blocks on the compilation worker.

### 6.3 `policyDispatcher.Dispatch` (Generic Layer)

[generic/policy_dispatcher.go:110-265](staging/src/k8s.io/apiserver/pkg/admission/plugin/policy/generic/policy_dispatcher.go#L110-L265) does the per-policy match-and-resolve work:

```
for each PolicyHook in hooks:
    matches, gvr, gvk := matcher.DefinitionMatches(attrs, hook.Policy)        ◄── policy-level match
    if !matches: continue

    for each binding in hook.Bindings:
        ok := matcher.BindingMatches(attrs, hook.Policy, binding)             ◄── binding-level match
        if !ok: continue

        params := CollectParams(hook.ParamInformer, binding, hook.Policy)     ◄── resolve params
        for each p in params (or [nil] if no paramKind):
            invocation := PolicyInvocation{Policy, Binding, Param: p, Kind, Resource, Evaluator}
            invocations = append(invocations, invocation)

delegate.dispatchInvocations(ctx, attrs, invocations)                          ◄── validating.dispatcher
```

`PolicyInvocation` ([generic/policy_dispatcher.go:43-66](staging/src/k8s.io/apiserver/pkg/admission/plugin/policy/generic/policy_dispatcher.go#L43-L66)) is the unit handed to the validating-specific layer.

### 6.4 `validating.dispatcher.Dispatch`

[validating/dispatcher.go:72-308](staging/src/k8s.io/apiserver/pkg/admission/plugin/policy/validating/dispatcher.go#L72-L308) — for each invocation:

1. Call `hook.Evaluator.Validate(...)` ([line 214](staging/src/k8s.io/apiserver/pkg/admission/plugin/policy/validating/dispatcher.go#L214)) — returns `[]PolicyDecision` + `[]PolicyAuditAnnotation`.
2. For each decision ([line 227-255](staging/src/k8s.io/apiserver/pkg/admission/plugin/policy/validating/dispatcher.go#L227-L255)):
   - If `Action == Admit`: record latency metric, continue.
   - If `Action == Deny`: walk `binding.Spec.ValidationActions`:
     - `Deny` → append to `deniedDecisions`.
     - `Audit` → publish a validation-failure annotation.
     - `Warn` → emit a `Warning:` HTTP header on the response.
3. Publish all collected audit annotations via the `audit.AddAuditAnnotations` API.
4. If `deniedDecisions` is non-empty ([line 289](staging/src/k8s.io/apiserver/pkg/admission/plugin/policy/validating/dispatcher.go#L289)): build a `metav1.StatusError` with the first decision's message + reason, attach all decisions as `causes`, return the error → **request rejected**.
5. Otherwise return `nil` → **request admitted**.

### 6.5 `validator.Validate` — the CEL evaluation core

[validating/validator.go:85-268](staging/src/k8s.io/apiserver/pkg/admission/plugin/policy/validating/validator.go#L85-L268). For one (Policy, Binding, Param) tuple:

```
1. If v.compileError != nil:
       return policyDecisionActionForError(failPolicy)        # one synthetic decision

2. If v.celMatcher != nil (matchConditions present):
       result := v.celMatcher.Match(ctx, versionedAttr, request, runtimeBudget)
       if !result.Matches:
           return []                                          # short-circuit: not applicable

3. Build the "outer" activation:
       a := newActivation(versionedAttr, request, optionalVars, namespace, authorizer, requestResourceAuthorizer, params)

4. validations, _, _ := v.validationFilter.ForInput(...)
       for each evaluation:
           switch:
               err != nil:        Action = policyDecisionActionForError(failPolicy);  Eval = EvalError
               result == false:   Action = Deny;   Eval = EvalDeny;   Message = static message
               result == true:    Action = Admit;  Eval = EvalAdmit

5. messages, _, _ := v.messageFilter.ForInput(...)
       for each (validation, messageEval):
           if messageEval.Result is non-empty string and validation was Deny:
               override decision.Message

6. audits, _, _ := v.auditAnnotationFilter.ForInput(...)
       for each (audit_expr, eval):
           if Result is null:        skip
           if Result is string:      attach as PolicyAuditAnnotation
           if eval error:            Action depends on failPolicy

7. return decisions, auditAnnotations
```

Note: **all** validation expressions in a policy are evaluated even if the first one denies — VAP semantics return *all* failures so they can be reported to the caller and recorded in audit.

---

## 7. CEL Subsystem

The CEL plumbing lives in `staging/src/k8s.io/apiserver/pkg/admission/plugin/cel/*`. It is a multi-layered façade over `github.com/google/cel-go`.

### 7.1 The Type System

`environment.EnvSet` ([environment/environment.go:71-83](staging/src/k8s.io/apiserver/pkg/cel/environment/environment.go#L71-L83)) is a **pair of `*cel.Env`**:

```go
type EnvSet struct {
    compatibilityVersion *version.Version
    newExpressions       *cel.Env  // gated to the apiserver's compatibility version
    storedExpressions    *cel.Env  // permissive (runs old + new expressions)
}
```

`Env(envType)` ([line 115](staging/src/k8s.io/apiserver/pkg/cel/environment/environment.go#L115)) returns one or the other. `NewExpressions` is used when the apiserver *validates* a freshly submitted policy spec; `StoredExpressions` is used at runtime.

`EnvSet.Extend(opts...)` ([line 219](staging/src/k8s.io/apiserver/pkg/cel/environment/environment.go#L219)) clones both envs by calling `cel.Env.Extend(...)` twice, after filtering options by version and chaining a fresh `apiservercel.NewDeclTypeProvider(...)` for any added `DeclTypes`. **One `EnvSet.Extend` = two `cel.Env.Extend`s**, which is itself a deep copy of every parser/checker/program option, function binding, library, type registry, and feature flag.

### 7.2 The Compiler

`Compiler.CompileCELExpression(accessor, options, envType)` ([cel/compile.go:152-153](staging/src/k8s.io/apiserver/pkg/admission/plugin/cel/compile.go#L152-L153)) is the single entry point that turns expression text into an executable program.

The implementation `*compiler` holds:

- `varEnvs variableDeclEnvs` — 8 pre-built envs (one per `(HasParams, HasAuthorizer, HasPatchTypes)` combination from `mustBuildEnvs` at [compile.go:315](staging/src/k8s.io/apiserver/pkg/admission/plugin/cel/compile.go#L315))
- `templateID *environment.EnvSet` (Patch 0001) — pointer-identity for cache keying; nil = opt out
- `variableSigFn func() string` (Patch 0001) — closure into composition state for variable typing

`compileFresh(...)` ([compile.go:257-313](staging/src/k8s.io/apiserver/pkg/admission/plugin/cel/compile.go#L257-L313)) is the actual cel-go compilation:

```go
env, _ := c.varEnvs[options].Env(envType)                  // pick the right one of 8 envs
ast, issues := env.Compile(expression)                     // parse + type-check  → cel.Ast (~10–80 KB)
prog, _ := env.Program(ast, cel.InterruptCheckFrequency(N)) // plan interpretable → cel.Program
return CompilationResult{Program: prog, OutputType: ast.OutputType(), ExpressionAccessor: accessor}
```

### 7.3 The Composited Compiler

`CompositedCompiler` ([cel/composition.go:42-48](staging/src/k8s.io/apiserver/pkg/admission/plugin/cel/composition.go#L42-L48)) wraps a `Compiler` with the policy-level *composition state* — i.e. the `variables` map.

Construction ([composition.go:66-105](staging/src/k8s.io/apiserver/pkg/admission/plugin/cel/composition.go#L66-L105)) for each policy:

1. Allocate a fresh `*apiservercel.DeclType` (`newMapType`) representing the policy's `variables` map.
2. Extend the input `EnvSet` to introduce `variables` as a CEL variable (1× `EnvSet.Extend`).
3. Build a per-policy `compositionState` (the mutable variables map + the extended env).
4. Build a `compiler` via `newCachedCompiler(state.EnvSet, envSet)` — which calls `mustBuildEnvs` (8 more `EnvSet.Extend`s).

**Per policy, before any expression is even seen: 1 + 8 = 9 `EnvSet.Extend` calls = 18 `cel.Env.Extend` clones.** Half of the 8 also do another Extend for `HasPatchTypes`, so the real count is **13 × 2 = 26 cel-go env clones per policy**.

`CompileAndStoreVariables(...)` ([composition.go:145-156](staging/src/k8s.io/apiserver/pkg/admission/plugin/cel/composition.go#L145-L156)) then compiles each variable expression in declaration order and registers its return type into `state.mapType.Fields` so subsequent expressions can type-check `variables.<name>`.

`CompileCondition(...)` ([composition.go:158-164](staging/src/k8s.io/apiserver/pkg/admission/plugin/cel/composition.go#L158-L164)) returns a `CompositedConditionEvaluator` that wraps the compiled expressions plus the composition state. At evaluation time it injects a per-request `compositionContext` that lazy-evaluates variables.

### 7.4 The Activation

[cel/activation.go:35-65](staging/src/k8s.io/apiserver/pkg/admission/plugin/cel/activation.go#L35-L65) — `newActivation` packs the runtime variable bindings:

```go
type evaluationActivation struct {
    object, oldObject, params, request, namespace,
    authorizer, requestResourceAuthorizer, variables interface{}
}
```

Implements `cel.Activation.ResolveName(name string)` ([line 88](staging/src/k8s.io/apiserver/pkg/admission/plugin/cel/activation.go#L88)) by name-switching against those fields.

### 7.5 Evaluation Path

For each compiled expression, `condition.ForInput(...)` ([cel/condition.go:90-115](staging/src/k8s.io/apiserver/pkg/admission/plugin/cel/condition.go#L90-L115)) calls `activation.Evaluate(ctx, prog)` ([cel/activation.go:119](staging/src/k8s.io/apiserver/pkg/admission/plugin/cel/activation.go#L119)) which:

1. Calls `prog.ContextEval(ctx, activation)` — recursive tree-walking interpreter.
2. Reads `EvalDetails.ActualCost()` and deducts from the per-request budget.
3. Returns the CEL result + remaining budget.

Cost budgets are global per request:

- `celconfig.RuntimeCELCostBudget` for validation rules
- `celconfig.RuntimeCELCostBudgetMatchConditions` for match-condition evaluations

When the budget is exhausted, evaluation is interrupted and the error is treated by `failPolicy`.

---

## 8. Matcher Subsystem

There are **two layers** of matching:

### 8.1 Resource Matching (`matching.Matcher`)

[matching/matching.go:74-127](staging/src/k8s.io/apiserver/pkg/admission/plugin/policy/matching/matching.go#L74-L127) — implements the API-shape `MatchResources` semantics: resource rules + namespace selector + object selector + exclude rules.

`Matches(...)` returns `(bool, schema.GroupVersionResource, schema.GroupVersionKind, error)`. It is called from:

- `policy_matcher.DefinitionMatches` ([generic/policy_matcher.go:67](staging/src/k8s.io/apiserver/pkg/admission/plugin/policy/generic/policy_matcher.go#L67)) — against `policy.Spec.MatchConstraints`
- `policy_matcher.BindingMatches` ([generic/policy_matcher.go:84](staging/src/k8s.io/apiserver/pkg/admission/plugin/policy/generic/policy_matcher.go#L84)) — against `binding.Spec.MatchResources`

A binding matches only if **both** the policy and the binding match.

### 8.2 CEL Match Conditions (`matchconditions.Matcher`)

[webhook/matchconditions/matcher.go:64-80](staging/src/k8s.io/apiserver/pkg/admission/plugin/webhook/matchconditions/matcher.go#L64-L80) — wraps a `ConditionEvaluator` for CEL preconditions evaluated *after* the resource matchers say "this policy applies."

`Match(ctx, attrs, request, budget)` ([line 80](staging/src/k8s.io/apiserver/pkg/admission/plugin/webhook/matchconditions/matcher.go#L80)) evaluates every match-condition expression. Semantics:

- All true → `Matches=true`
- Any false → `Matches=false` (skip this policy)
- Any error → consult `failPolicy` (default: `Fail`)

This is the per-request short-circuit that keeps cheap match conditions from forcing expensive validations to run.

---

## 9. Param Resolution

A policy may declare a `paramKind`; the binding then references an instance via `paramRef` (by name or label selector). The framework manages the informers for these param resources lazily:

[generic/policy_source.go:393-468](staging/src/k8s.io/apiserver/pkg/admission/plugin/policy/generic/policy_source.go#L393-L468) — `ensureParamsForPolicyLocked`:

1. If `paramKind` is nil → no informer, return.
2. Resolve `paramKind` to a `schema.GroupVersionKind`.
3. Look up an existing `paramInfo` keyed by GVK.
4. If absent: build a `dynamicinformer` (cluster- or namespace-scoped per RESTMapper), start it, store it.
5. Reference-count: when the last policy referencing this GVK is deleted, the informer is shut down.

At dispatch time, `CollectParams` ([generic/policy_dispatcher.go:267-396](staging/src/k8s.io/apiserver/pkg/admission/plugin/policy/generic/policy_dispatcher.go#L267-L396)):

1. If no `paramRef` → return `[nil]` so the policy is evaluated once with `params == nil`.
2. If `paramRef.Name` set → look up that single param object.
3. If `paramRef.Selector` set → list matching params from the informer.
4. If nothing matches and `parameterNotFoundAction == Deny` → synthesize a deny decision; if `Allow` → skip.

Each resolved param produces one `PolicyInvocation`, so a label-selector binding can multiply evaluations across many param objects.

---

## 10. Failure Policy and Validation Actions

Two orthogonal axes control behavior on failure.

### 10.1 `FailurePolicy` (policy-level)

`Fail` (default) or `Ignore`. Applies to:
- Compile errors at policy load time.
- Runtime CEL evaluation errors.
- Cost budget exhaustion.
- Match condition errors.

`policyDecisionActionForError(fp)` ([validating/validator.go](staging/src/k8s.io/apiserver/pkg/admission/plugin/policy/validating/validator.go), uses `failPolicy`) → maps `Fail`→`Deny` and `Ignore`→`Admit`.

### 10.2 `ValidationActions` (binding-level)

A list combining `Deny`, `Audit`, `Warn`. Applied per-decision in [validating/dispatcher.go:227-255](staging/src/k8s.io/apiserver/pkg/admission/plugin/policy/validating/dispatcher.go#L227-L255):

| Action | Effect on Deny decisions |
|---|---|
| `Deny` | Add to `deniedDecisions` → request will be rejected |
| `Audit` | Emit a structured audit annotation (request still admitted unless `Deny` is also set) |
| `Warn` | Emit an HTTP `Warning:` header (visible in `kubectl` output) |

A single binding can have e.g. `[Audit, Warn]` to observe-without-blocking.

---

## 11. Type Checking

Beyond the runtime path, VAP supports **static type checking** at policy *write* time:

[validating/typechecking.go](staging/src/k8s.io/apiserver/pkg/admission/plugin/policy/validating/typechecking.go):

- `TypeChecker.Check(policy)` ([line 108](staging/src/k8s.io/apiserver/pkg/admission/plugin/policy/validating/typechecking.go#L108)) returns warnings that surface in `policy.status.conditions`.
- `CreateContext(policy)` ([line 141](staging/src/k8s.io/apiserver/pkg/admission/plugin/policy/validating/typechecking.go#L141)) walks `MatchConstraints.ResourceRules`, looks up each `(group,version,kind)` via the schema resolver, and produces a list of `cel.Type`s the expressions might bind `object`/`oldObject` to.
- `CheckExpression()` ([line 195](staging/src/k8s.io/apiserver/pkg/admission/plugin/policy/validating/typechecking.go#L195)) compiles the expression once per discovered type; warnings are aggregated.

A cap of `maxTypesToCheck = 10` ([line 45](staging/src/k8s.io/apiserver/pkg/admission/plugin/policy/validating/typechecking.go#L45)) keeps type-checking bounded for wildcard rules like `resources: ["*"]`.

This runs out-of-band on the policy storage path (REST validation), not on every admission request.

---

## 12. Concurrency Model

| Component | Thread safety |
|---|---|
| `policySource.policies` | `atomic.Pointer[[]PolicyHook]` — lock-free reads; writer holds `s.lock` |
| `policySource.compiledPolicies` map | guarded by `s.lock`; only mutated in `refreshPolicies` |
| `paramsCRDControllers` map | guarded by `s.lock` |
| `cel.Program` | goroutine-safe per cel-go's contract (immutable once built) |
| `validator` | immutable after construction; safe for concurrent `Validate` |
| Process-wide compile cache (`compileCache`) | `k8s.io/utils/lru.Cache` (RW-mutex internally); pointer swap via `atomic.Pointer` |
| Hit/miss counters | `atomic.Int64` |
| `compositionContext` | per-request value; not shared across requests |

**Key invariant:** the data plane never blocks on the control plane. Reading the active policy set is one atomic load; the refresh worker writes to a private map and atomically swaps a slice pointer when finished.

---

## 13. Memory Model + Cache

### 13.1 Per-policy fixed cost (the `F` term)

Every `compilePolicy` call pays:

- 1× `EnvSet.Extend` in `NewCompositedCompiler` (variables map declaration) → 2 cel-go env clones
- 12× `EnvSet.Extend` in `mustBuildEnvs` (8 envs, of which 4 do an extra Extend for `HasPatchTypes`) → 24 cel-go env clones
- **Total: 26 cel-go env clones per policy ≈ 150–500 KB**

This is *unchanged* by Patch 0001. Patch 0002 attacks it by sharing the `*CompositedCompiler` across policies.

### 13.2 Per-expression cost (the `C` term)

Each `compileFresh` call:

- `env.Compile(text)` → `cel.Ast`: parse + type-check, ~10–80 KB
- `env.Program(ast, ...)` → `cel.Program`: builds a fresh `Dispatcher` (copying every function binding in the env), a fresh `AttributeFactory`, an `Interpretable` tree mirroring the AST. **Most of the per-program memory is the dispatcher copy**, not the interpretable tree.

### 13.3 Patch 0001's cache

The patch wraps `CompileCELExpression` with a process-wide LRU keyed by a SHA-256 over: env-template pointer identity, normalized expression text, env type (`NewExpressions`/`StoredExpressions`), `OptionalVariableDeclarations` bits, sorted return-type signature, and an in-scope variable signature.

[cel/compile_cache.go](staging/src/k8s.io/apiserver/pkg/admission/plugin/cel/compile_cache.go):

```go
const defaultCompileCacheSize = 5000
var globalCompileCache atomic.Pointer[lru.Cache]
var compileCacheHits, compileCacheMisses atomic.Int64
```

On hit: return the shared `cel.Program`, rebind the `ExpressionAccessor` to the caller's instance, increment hit counter.
On miss: run `compileFresh`, insert successful results, increment miss counter.

**Errors are not cached.** Failed compilations re-attempt every time.

The `templateID *environment.EnvSet` field on `compiler` controls opt-in: only `NewCompositedCompiler` (which uses the singleton template) opts in. `NewCompiler` callers (tests, webhook matcher with custom cost limits) leave `templateID = nil` and bypass the cache entirely — preventing custom `cel.ProgramOption`s from contaminating shared programs.

### 13.4 Memory growth model

Pre-patch: `Total ≈ N × (F + E_p × C)` where N = policy count, E_p = expressions per policy, C ≈ 60 KB per program.

Post-patch (Patch 0001 only): `Total ≈ N × F + min(U, LRU_cap) × C` where U = unique expression texts cluster-wide.

For templated multi-tenant clusters where U ≪ N × E_p, this collapses memory by ~10–20×. For the all-unique adversarial case it is approximately a no-op (LRU caps overhead at ~300 MB).

---

## 14. End-to-End Sequence Diagram

```
TIME ──►

Startup:
  apiserver.RegisterAllPlugins
    └─► validating.Register ─────────────────────────────────────────────────────────────►
                                                                                          │
                                                                                          ▼
                                                                                  validating.NewPlugin
                                                                                          │
                                                                                          ▼
                                                                                  generic.Plugin{
                                                                                    sourceFactory,
                                                                                    dispatcherFactory,
                                                                                    compilePolicy,
                                                                                  }

  apiserver.startCustomResourceServer
    └─► Plugin.ValidateInitialization
            ├─► matcher = matching.NewMatcher(...)
            ├─► source = sourceFactory(...)
            ├─► dispatcher = dispatcherFactory(...)
            ├─► go source.Run(ctx)        ─────────► informers + refreshPolicies loop (1Hz)
            └─► go dispatcher.Start(ctx)

Control plane (informer triggered):
  VAP/VAPB add/update/delete event
    └─► policySource.notify() → policiesDirty.Store(true)
                                                  │
  refreshPolicies tick (every 1s):                ▼
    └─► if policiesDirty: calculatePolicyData()
            ├─► for each (policy, bindings):
            │     ├─► compilePolicyLocked(policy):
            │     │     ├─► cache hit by ResourceVersion? → reuse validator
            │     │     └─► else: compilePolicy(policy) → cel.NewCompositedCompiler →
            │     │                                     → 13× EnvSet.Extend →
            │     │                                     → CompileCondition × N →
            │     │                                     → env.Compile + env.Program (cache check)
            │     └─► ensureParamsForPolicyLocked(policy)
            └─► policies.Store(&newHooks)            ◄── atomic swap

Data plane (per HTTP request):
  client → apiserver
    └─► auth chain → mutating chain → validating chain
            └─► Plugin.Validate(attr)
                    └─► generic.Plugin.Dispatch
                            └─► hooks := source.Hooks()              ◄── lock-free atomic load
                                policyDispatcher.Dispatch(hooks):
                                  for each PolicyHook:
                                    if !DefinitionMatches: skip
                                    for each Binding:
                                      if !BindingMatches: skip
                                      params := CollectParams(...)
                                      for each param:
                                        invocations += {P,B,param,Eval}

                                validating.dispatcher.Dispatch(invocations):
                                  for each invocation:
                                    decisions, audits := hook.Evaluator.Validate(...)
                                       ├─► if compileError != nil: synthetic decision
                                       ├─► if matchConditions: celMatcher.Match
                                       │     └─► returns Matches={true,false} or error
                                       ├─► if !Matches: return []
                                       ├─► newActivation(object, oldObject, params, ...)
                                       ├─► validationFilter.ForInput
                                       │     └─► for each compiled expr:
                                       │           prog.ContextEval(ctx, activation)
                                       │           deduct cost; check budget
                                       ├─► messageFilter.ForInput  → override messages
                                       └─► auditAnnotationFilter.ForInput

                                    for each decision:
                                      apply ValidationActions: Deny|Audit|Warn

                                  publish audit annotations
                                  if any Deny: return Forbidden status error
                                  else: return nil

  apiserver returns 2xx (admit) or 4xx (deny) to client
```

---

## 15. Source File Reference Map

```
staging/src/k8s.io/apiserver/pkg/admission/plugin/
├── policy/
│   ├── generic/                                    ← framework shared with MAP
│   │   ├── plugin.go              ◄── generic.Plugin[H]: registration + lifecycle
│   │   ├── policy_source.go       ◄── informers + refreshPolicies + compile cache
│   │   ├── policy_dispatcher.go   ◄── per-request match + param resolution + invocation building
│   │   ├── policy_matcher.go      ◄── DefinitionMatches / BindingMatches façade
│   │   ├── policy_test_context.go
│   │   ├── interfaces.go          ◄── Source[H], Dispatcher[H], Hook, Evaluator
│   │   ├── accessor.go            ◄── PolicyAccessor / BindingAccessor
│   │   └── composite_policy_source.go
│   │
│   ├── validating/                                 ← VAP-specific specialization
│   │   ├── plugin.go              ◄── validating.Plugin: Validate(), compilePolicy()
│   │   ├── dispatcher.go          ◄── per-invocation Validate + ValidationActions
│   │   ├── validator.go           ◄── the compiled struct; runs all CEL filters
│   │   ├── policy_decision.go     ◄── PolicyDecision + PolicyAuditAnnotation enums
│   │   ├── message.go             ◄── MessageExpressionCondition accessor
│   │   ├── typechecking.go        ◄── static type-checker for the storage path
│   │   ├── accessor.go            ◄── concrete accessors for v1.VAP / v1.VAPB
│   │   ├── initializer.go
│   │   ├── interface.go
│   │   └── errors.go
│   │
│   ├── matching/
│   │   └── matching.go            ◄── resource rules + namespace/object selectors
│   │
│   ├── mutating/                                   ← MAP plugin (sibling of validating)
│   │   └── ...
│   │
│   └── internal/                                   ← shared helpers
│
├── cel/                                            ← CEL compilation façade
│   ├── compile.go                 ◄── Compiler + CompileCELExpression + mustBuildEnvs
│   ├── compile_cache.go           ◄── Patch 0001: process-wide LRU + fingerprint
│   ├── composition.go             ◄── CompositedCompiler + variable composition state
│   ├── condition.go               ◄── ConditionCompiler + ConditionEvaluator (validations/match/audit)
│   ├── filter.go                  ◄── filter primitives
│   ├── activation.go              ◄── runtime variable resolver + cost deduction
│   ├── interface.go               ◄── ExpressionAccessor + ConditionEvaluator interfaces
│   └── mutation.go                ◄── (mutating-side compiler)
│
└── webhook/matchconditions/
    └── matcher.go                 ◄── CEL match-condition evaluator (also used by VAP)


staging/src/k8s.io/apiserver/pkg/cel/
├── environment/
│   └── environment.go             ◄── EnvSet (NewExpressions/StoredExpressions) + Extend semantics
├── library/                       ◄── Kubernetes-specific CEL function libraries
└── ...


vendor/github.com/google/cel-go/
├── cel/
│   ├── env.go                     ◄── *cel.Env + Extend + Compile + Program
│   ├── program.go                 ◄── newProgram + prog struct + Eval/ContextEval
│   └── ...
├── interpreter/
│   ├── planner.go                 ◄── AST → Interpretable tree
│   ├── interpreter.go             ◄── NewInterpretable
│   ├── attributes.go              ◄── AttributeFactory
│   └── ...
└── checker/                       ◄── type-checker
```

---

## Appendix A — Glossary

| Term | Meaning |
|---|---|
| **VAP** | `ValidatingAdmissionPolicy` — the policy definition resource |
| **VAPB** | `ValidatingAdmissionPolicyBinding` — binds a VAP to a scope and concrete params |
| **MAP / MAPB** | Mutating equivalents (sibling plugin, same generic framework) |
| **Hook** | A loaded `(Policy, Bindings, Evaluator)` triple used by the dispatcher |
| **Evaluator** | Compiled form of a policy — a `validator` for VAP, a `mutator` for MAP |
| **Validator** | The struct that holds compiled CEL filters and runs them per request |
| **Filter** | A `ConditionEvaluator` over a list of expressions (validations, match, audit) |
| **Activation** | The runtime binding of CEL variable names (`object`, `params`, ...) to values |
| **CompositedCompiler** | Per-policy compiler that owns the policy's `variables` map |
| **EnvSet** | Pair of `(NewExpressions, StoredExpressions)` cel-go envs |
| **`cel.Program`** | Tree-walking-interpreter form of a type-checked AST + a private function dispatcher |
| **`cel.Ast`** | Type-checked AST (declarative) |
| **PolicyInvocation** | One `(Policy, Binding, Param)` tuple to evaluate against the current request |
| **PolicyDecision** | Result of one `Validation` expression: Admit / Deny / Error + Message |
| **PolicyAuditAnnotation** | Result of one `AuditAnnotation` expression: a key/value pair for audit |
| **FailurePolicy** | Per-policy: `Fail` (default) or `Ignore` for compile/runtime errors |
| **ValidationActions** | Per-binding: any combination of `Deny`, `Audit`, `Warn` |
| **ParamKind / ParamRef** | Optional CRD or built-in resource referenced as `params` in CEL |
| **MatchConstraints** | Resource-rules + selectors at the policy level |
| **MatchResources** | Same shape, but at the binding level (intersection) |
| **MatchConditions** | CEL preconditions evaluated per request as a fast-path filter |

---

## Appendix B — Cross-Reference With This Repo's Patches

| Patch | What it changes | Where in this doc |
|---|---|---|
| [0001-cel-add-process-wide-compiled-program-cache.patch](./0001-cel-add-process-wide-compiled-program-cache.patch) | LRU on `cel.Program` keyed by structural fingerprint | §13.3 |
| [0002-cel-share-composited-compilers-across-policies.patch](./0002-cel-share-composited-compilers-across-policies.patch) | Share the `*CompositedCompiler` (envs) across policies | §13.1, §7.3 |
| [0003-cel-add-expression-normalization-utility.patch](./0003-cel-add-expression-normalization-utility.patch) | Whitespace canonicalization for cache key | §13.3 |

Issue: <https://github.com/kubernetes/kubernetes/issues/131417>