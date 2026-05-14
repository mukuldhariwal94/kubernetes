# ValidatingAdmissionPolicy — CEL Compilation Architecture

Diagram of where CEL expressions enter the apiserver, are compiled, cached,
and evaluated. Annotations call out the current hotspots for memory and
duplicate-work. This is the baseline the optimizations in
[plugin-optimization-analysis.md](plugin-optimization-analysis.md) target.

---

## 1. High-level flow (Mermaid)

```mermaid
flowchart TD
    subgraph informers[Shared Informers]
      A1[ValidatingAdmissionPolicy<br/>SharedIndexInformer]
      A2[ValidatingAdmissionPolicyBinding<br/>SharedIndexInformer]
    end

    A1 -- AddFunc / UpdateFunc / DeleteFunc --> N[notify: policiesDirty = true]
    A2 -- AddFunc / UpdateFunc / DeleteFunc --> N

    N --> W{refreshPolicies<br/>worker tick<br/>(every 1s)}
    W --> CPD[calculatePolicyData<br/>generic/policy_source.go:277]

    CPD --> CPL[compilePolicyLocked<br/>generic/policy_source.go:470]
    CPL -- resourceVersion match? --> HIT[cache hit:<br/>reuse compiledPolicyEntry]
    CPL -- miss --> CVP[validating.compilePolicy<br/>validating/plugin.go:147]

    CVP --> NCC[cel.NewCompositedCompiler<br/>cel/composition.go:64]
    NCC -- 1x Extend for 'variables' map --> NC[cel.NewCompiler<br/>cel/compile.go:159]
    NC -. after fix .-> LAZY[lazy envFor cache<br/>built on first use]
    NC -. before fix .-> EAGER[mustBuildEnvs:<br/>8 x Extend, only 1-2 used]

    CVP --> CAV[CompileAndStoreVariables]
    CVP --> CMC[CompileCondition: matchConditions]
    CVP --> CVL[CompileCondition: validations]
    CVP --> CAA[CompileCondition: auditAnnotations]
    CVP --> CME[CompileCondition: messageExpressions]

    CAV --> CCE1[compiler.CompileCELExpression]
    CMC --> CCE2[compiler.CompileCELExpression]
    CVL --> CCE3[compiler.CompileCELExpression]
    CAA --> CCE4[compiler.CompileCELExpression]
    CME --> CCE5[compiler.CompileCELExpression]

    CCE1 & CCE2 & CCE3 & CCE4 & CCE5 --> ENV[env.Compile + env.Program<br/>cel-go]
    ENV --> PROG[cel.Program retained<br/>~30-50 KB each]
    PROG --> VAL[Validator struct<br/>stored in PolicyHook]

    VAL --> DISP[Dispatcher<br/>validating/dispatcher.go]
    DISP -- per request --> EVAL[condition.ForInput<br/>cel/condition.go:90]
    EVAL --> PROGEXEC[prog.ContextEval]

    classDef hot fill:#ffdddd,stroke:#c33,color:#000
    classDef cache fill:#ddffdd,stroke:#3c3,color:#000
    class EAGER,PROG hot
    class HIT,LAZY cache
```

---

## 2. Per-policy cost breakdown (today, post-lazy-env fix)

```
┌──────────────────────────────────────────────────────────────────┐
│                  Per-Policy Memory Footprint                     │
├──────────────────────────────────────────────────────────────────┤
│                                                                  │
│  NewCompositedCompiler                                           │
│  ├── +1 envSet (variables map type)    ─── unavoidable (~25 KB)  │
│  └── NewCompiler (lazy)                                          │
│      └── envFor(opts) on first use     ─── 1-2 envSets           │
│                                                                  │
│  CompileCondition × 4 call-sites                                 │
│  ├── matchConditions   × N   ┐                                   │
│  ├── validations       × M   │                                   │
│  ├── auditAnnotations  × K   │   each -> cel.Program retained    │
│  └── messageExpression × M   │        ~30-50 KB / program        │
│                              │        NOT deduplicated today     │
│                              ┘                                   │
│                                                                  │
│  compositionState                                                │
│  └── compiledVariables[name] = CompilationResult                 │
│      (one per spec.variables entry; also retained)               │
│                                                                  │
└──────────────────────────────────────────────────────────────────┘

Dominant term (post lazy-env fix):
  mem(policy) ≈ 25 KB + 1.. 2 × envSet + Σ exprs × ~40 KB
```

---

## 3. Where duplicate work hides

Three duplication axes exist **today** that the compiler pipeline does not
exploit:

```
                                  ┌─────────────────────────────────┐
 axis A: intra-policy duplicates  │ Policy "deny-privileged"        │
 (same policy repeats an expr)    │   matchConditions:              │
                                  │     - "has(object.spec)"  ◄── A │
                                  │   validations:                  │
                                  │     - "has(object.spec) &&..."  │
                                  └─────────────────────────────────┘

 axis B: intra-tenant duplicates   ┌──────────────┐  ┌──────────────┐
 (same CEL text across policies,   │ Policy ns-a  │  │ Policy ns-b  │
  same variable declarations)      │ "object.kind │  │ "object.kind │
                                   │  == 'Pod'"   │  │  == 'Pod'"   │
                                   └──────────────┘  └──────────────┘
                                          ▲                 ▲
                                          └───── share? ────┘

 axis C: stored-expression stability  (a policy's CEL is immutable until the
                                       policy resourceVersion changes; today
                                       any informer update forces a full
                                       recompile even when Spec.Validations
                                       is byte-identical)
```

Axis A and C are straightforward to exploit inside the admission plugin
without crossing module boundaries. Axis B (cross-policy sharing) is where
the largest memory win is, but requires a well-keyed process-wide cache.

---

## 4. Caching layers — current vs. proposed

```
 CURRENT                                   PROPOSED (incremental)
 ───────────────────────                   ─────────────────────────────────

 per-policy compiledPolicyEntry            per-policy compiledPolicyEntry
   keyed by resourceVersion                  keyed by resourceVersion
         (unchanged)                               (unchanged)
                                                       +
                                           per-compiler expression cache
                                             keyed by (expr, opts, envType)
                                             — dedupes axis A
                                                       +
                                           process-wide program cache
                                             keyed by (expr, opts, envType,
                                                       varSignature,
                                                       compatVersion)
                                             — dedupes axis B
                                             bounded LRU, e.g. 4096 entries
```

The two new layers are additive, read-through, and do **not** change the
Validator / PolicyHook lifecycle. Cache entries are invalidated implicitly
by the existing resourceVersion check in `compilePolicyLocked`, because a
change to a policy's expressions produces a new cache key — so stale
entries are garbage-collected via LRU eviction rather than explicit
invalidation.

---

## 5. Request path (unchanged by these optimizations)

```
 apiserver request
        │
        ▼
 Plugin.Validate ── generic.Plugin.Dispatch ── policyMatcher (namespace/label)
        │
        ▼
 for each matching PolicyHook:
        │
        ▼
 dispatcher.Dispatch
        │
        ├── matcher.Match             (runs match-condition programs)
        ├── validator.Validate        (runs validation programs)
        │       └── condition.ForInput
        │              └── prog.ContextEval     ◄── cel-go executes compiled Program
        └── auditAnnotations / messageExpression
```

The optimizations target **compile time** only. Request-path behavior —
cost budget, audit annotations, failure policy, reinvocation — is untouched.
