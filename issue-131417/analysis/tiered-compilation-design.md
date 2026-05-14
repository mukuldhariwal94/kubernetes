# Tiered / demand-driven CEL compilation for ValidatingAdmissionPolicy — detailed design

> Companion to [`architecture-redesign.md`](./architecture-redesign.md) (which makes the
> high-level case for demand-driven residency) and the local-optimization patch series in
> [`../patches/`](../patches/). This document is the **engineering-level** treatment: exact
> mechanics of what is cached today, the cel-go memory layout, a concrete three-tier cache
> design, eviction/concurrency algorithms, and a point-by-point trade-off analysis at
> realistic Kubernetes scale (10 000 – 100 000 policies, high admission throughput,
> memory-constrained control planes, large multi-tenant clusters).
>
> **Scope.** Production code paths only. `*_test.go` files, benchmark scaffolding, mocks,
> and the debug-binary / heap-dump profiling recipes are validation tooling, not part of
> the runtime memory model, and are excluded from all accounting below. AST validators
> (`cel.ASTValidators(...)`) and the `UnversionedLib` wrapper are compile-time-only and
> retain no heap, so they are likewise out of the memory picture (they do count toward
> compile *latency*, see §2).

---

## 0. TL;DR

* **Today VAP is `O(N_policies × E_expressions_per_policy × bytes_per_cel.Program)` resident,
  unconditionally, forever** — compiled eagerly off the informer path, never evicted except
  on policy delete, no size bound. There is no leak; the design is simply "compile
  everything, keep everything".
* The patch series (0001–0012) **divides the constant** (per-`cel.Program` cost from
  ~150–250 KB → ~30–60 KB, and dedups identical expression text across policies). For the
  *common* real-world shape — templated/multi-tenant clusters where 10 k–100 k policies
  share a few hundred to a few thousand *distinct* expressions — that alone very likely
  closes the gap (≈ a few hundred MB), because the dedup cache key collapses `N_policies`
  to `N_distinct_expressions`.
* For the residual case — many *genuinely unique* expressions — the asymptote only breaks
  by making `cel.Program` residency **demand-driven**: hot expressions stay compiled in a
  bounded cache; cold ones live as a `CheckedExpr` proto (re-`Plan` on demand) or just a
  raw string; `cel.Program` is materialized lazily on first matching admission request.
* The price is **request-path compile cost on cold misses** (≈0.3–5 ms/expression,
  ≈5–75 ms for a whole cold policy on an apiserver-shaped env) and a **cold-start warm-up
  window** with elevated p99. The pragmatic answer is therefore **hybrid**: keep eager
  compilation for a *bounded* working set (the hot/active policies — recovers today's
  "zero compile on the request path" property for the common case) and only the long cold
  tail goes lazy.
* Eviction should be **W-TinyLFU (frequency-gated admission)**, not plain LRU — LRU
  thrashes under scan/churn and lets a noisy cold tenant evict a quiet hot tenant
  (fairness). The cache must be **sharded** (or fronted by a lock-free top-K snapshot) to
  avoid becoming the new contention point on a 10 k+ req/s admission path.

---

## 1. What is cached today — exact mechanics

### 1.1 The compile site (off the request path)

VAP compilation runs **out of band**, in a background goroutine, not on any admission
request:

```
informer add/update/delete (VAP or VAPB)
  → policySource.notify()  → policiesDirty.Store(true)
  → wait.Until(policySource.refreshPolicies, 1*time.Second, ...)         [policy_source.go]
      refreshPolicies():
        if !policiesDirty.Swap(false) { return }                          # debounced
        policies := calculatePolicyData()                                 # under s.lock (one fat mutex)
          for each policy:
            PolicyHook{ Evaluator: compilePolicyLocked(policySpec) }
              compilePolicyLocked:
                key := {Namespace, Name}
                if compiledPolicies[key].policyVersion == policy.ResourceVersion: reuse  # only dedup that exists
                else: compiledPolicies[key] = { policy.ResourceVersion, compiler(policySpec) }
        s.policies.Store(&policies)                                        # atomic swap of the whole slice
```

`compiler(policySpec)` for VAP is `validating.compilePolicy(policy)`:

```
compilePolicy(policy):
  template := getCompositionEnvTemplateWithStrictCost()          # sync.Once singleton EnvSet
  fc := cel.NewCompositedCompiler(template)                      # NEW per policy: state + Extend({variables}) + 8-env varEnvs matrix
  fc.CompileAndStoreVariables(variables, ...)                    # → V × CompileCELExpression
  Validator{
    matcher:               matchconditions.NewMatcher(fc.CompileCondition(matchConditions, ...))   # M × CompileCELExpression
    validationFilter:      fc.CompileCondition(validations, ...)                                   # Vd × CompileCELExpression
    auditAnnotationFilter: fc.CompileCondition(auditAnnotations, ...)                               # A × CompileCELExpression
    messageFilter:         fc.CompileCondition(messageExpressions, ...)                             # Vd × CompileCELExpression
  }
```

and `CompileCELExpression` ([compile.go](../../staging/src/k8s.io/apiserver/pkg/admission/plugin/cel/compile.go)) is:

```
CompileCELExpression(accessor, opts, mode):
  env  := c.varEnvs[opts].Env(mode)         # pick 1 of 8 *cel.Env
  ast  := env.Compile(text)                 # PARSE + TYPE-CHECK  → *cel.Ast  (transient: discarded after .OutputType())
  _    := cel.AstToCheckedExpr(ast)         # builds a CheckedExpr proto … and throws it away  (patch 0012)
  prog := env.Program(ast, InterruptCheckFrequency(...))     # PLAN  → cel.Program  (RETAINED)
  return { Program: prog, OutputType: ast.OutputType() }
```

### 1.2 The retention root

Everything is reachable from `policySource.policies atomic.Pointer[[]PolicyHook]`. Per
policy the retained closure is, roughly:

```
PolicyHook
 ├─ Policy spec (the VAP object)                                ~few KB
 ├─ Bindings []                                                 ~few KB each
 └─ Evaluator (= validating.Validator)
     ├─ matcher        → ConditionEvaluator → []CompilationResult{ Program: cel.Program }   ← M programs
     ├─ validationFilter   → []CompilationResult                                            ← Vd programs
     ├─ auditAnnotationFilter → []CompilationResult                                          ← A programs
     ├─ messageFilter   → []CompilationResult                                                ← Vd programs
     └─ (the CompositedCompiler / compositionState that produced them, incl. the 8-env varEnvs matrix and per-policy variables Extend)
```

So per policy: `(M + 2·Vd + A + V) cel.Program`s + one `CompositedCompiler` + an 8-entry
env matrix (each entry is a `cel.Env` clone — two cel-go envs, since `EnvSet` holds a
`NewExpressions` and a `StoredExpressions` env). The patch series attacks every term:
0001 dedups `cel.Program` by expression text, 0002 dedups the `CompositedCompiler`,
0004/0005/0010 shrink/share the env matrix, 0006 drops the variables Extend for
zero-variable policies, 0009 collapses the per-program dispatcher, 0008 caps the dedup
cache, 0012 removes the discarded `CheckedExpr` + singleton `ReturnTypes`.

### 1.3 The request path (zero compilation today)

```
Plugin.Validate
 → generic.Plugin.Dispatch
   → policies := source.Hooks()                # *atomic.Pointer load — LOCK-FREE
   → for each PolicyHook whose Matcher matches the request:
       hook.Evaluator.Validate(...)
         → matcher.Match → ConditionEvaluator.ForInput
             → newActivation(...)              # *evaluationActivation — NEW per request per filter, NOT pooled
             → CompilationResult.Program.ContextEval(ctx, activation)
                 → cel-go: ctxEvalActivation (POOLED via ctxActivationPool), CostTracker (per eval), result ref.Vals
```

**No `cel.Program` is built on the request path today.** The only per-request allocations
are the `*evaluationActivation` (k8s side, unpooled), the lazy `variables` map for
variable-bearing policies (per request — patch 0006 removes it for zero-variable
policies), the cel-go `ctxEvalActivation` (pooled), a small per-eval `CostTracker`, and
the intermediate `ref.Val`s produced during evaluation (∝ expression complexity).

### 1.4 Eviction today

* policy **deleted** → its `compiledPolicies[key]` entry is removed in
  `calculatePolicyData` (the "for policyKey range compiledPolicies; if not in current set:
  delete" loop) and it drops out of the next `policies` slice.
* policy **updated** (ResourceVersion change) → recompiled, the new `Validator` replaces
  the old in the map and slice; the old becomes garbage.
* **Otherwise: never.** No size bound, no TTL, no LRU. A live policy's `Validator` (and
  all its `cel.Program`s) is resident for as long as the policy exists, whether or not it
  has ever matched a request.

That is the entire scaling story: **resident set = total live policy count, not active
policy count.**

---

## 2. Is compile cost large enough to be worth deferring?

There is no public micro-benchmark in-tree, but the shape is clear:

* The base env (`MustBaseEnvSet`) registers the cel-go standard library (~48 functions /
  ~168 overloads / ~15 macros) **plus 18 k8s/ext libraries** (URLs, Regex, Lists, Authz,
  Quantity, IP, CIDR, Format, AuthzSelectors, Semver; ext.Strings v2, ext.Sets,
  ext.TwoVarComprehensions, ext.Lists v3) — call it **100+ functions, several hundred
  overloads**. Type-checking resolves every call against that table; `cel.EagerlyValidateDeclarations(true)`
  moves *declaration* validation to env-build time (already done in master), but
  per-expression *overload resolution* still scales with table size.
* `cel.NewCompositedCompiler` builds an **8-entry env matrix**, each entry two
  `cel.Env.Extend` clones — the in-tree comment on `EnvSet.Extend` literally calls it
  "expensive". That's per policy today (0002/0004/0005 fix it).
* `env.Program` (`Plan`) walks the type-checked AST building the interpretable tree, **and
  copies all of `e.functions` into a fresh dispatcher** (`for fn := range e.functions {
  disp.Add(fn.Bindings()...) }` — the `FunctionDecl.Bindings` heap node, ≈40 % of retained
  compilation heap in the issue-131417 profile; 0009 shares it).
* `cel.AstToCheckedExpr` builds a proto tree of the whole expression — and `CompileCELExpression`
  throws it away (0012).

Order-of-magnitude: **~0.3–5 ms to compile one expression** on an apiserver-shaped env
(parse ≈ tens of µs, check ≈ hundreds of µs–low ms, plan ≈ hundreds of µs–low ms,
dispatcher build ≈ tens–hundreds of µs), so a policy with 10–15 expressions is
**~5–75 ms** end-to-end. That is *large* relative to a healthy admission p50 (single-digit
ms), which is precisely why today's design pushes it off the request path and onto a 1 s
debounced background tick. **Conclusion: yes, the cost is big enough that moving it onto
the request path is a real regression — pure lazy is unattractive; the cost must be hidden
behind a hybrid (§4.7) or amortized by dedup (§4.1).**

---

## 3. The cost of each representation, and the cel-go memory layout

### 3.1 Tiers of representation, smallest → largest

| Representation | What it is | Approx size / expression | What you must do to get a `cel.Program` from it |
|---|---|---|---|
| raw CEL string | `"object.spec.replicas <= params.maxReplicas"` | ~30 B – ~1 KB | **parse + check + plan** (full compile) |
| parsed AST | `*ast.AST` (or `*exprpb.ParsedExpr`) + source info | ~2–10 KB | check + plan |
| type-checked AST | `*cel.Ast` = parsed tree + type map + reference map | ~4–20 KB | **plan only** (`env.Program(ast)`) |
| serialized `CheckedExpr` | proto bytes (compresses ~3–5×) | ~4–20 KB raw / ~1–5 KB gz | deserialize + plan |
| `cel.Program` | plan tree + dispatcher + interpreter + cost cfg | **~150–250 KB today; ~30–60 KB after 0009** | nothing (it's the program) |

Two facts dominate the design:

1. **A cold policy held as just its raw string is ~1000× smaller than as a `cel.Program`**
   (post-0009: ~1000×; pre-0009: ~3000–8000×). Held as a type-checked AST it is
   ~3–15× smaller.
2. **The expensive part of compilation is type-checking** (overload resolution against
   100+ functions), not parsing or planning. So a *type-checked-AST* cold tier lets a
   cold→hot transition skip the expensive part and pay only `Plan` (~hundreds of µs);
   a *raw-string* cold tier is smaller but pays the full cost on every cold→hot transition.
   ⇒ argues for a **two-level cold representation** (raw string always; checked AST in a
   larger second LRU).

### 3.2 Anatomy of a `cel.Program` (`*prog`)

```
prog (= cel.Program)
 ├─ *Env (embedded)                       — SHARED, just a pointer; 0 marginal bytes
 ├─ dispatcher  interpreter.Dispatcher    — fresh map of ALL ~hundreds of overloads + closures   ≈150 KB  ← 0009 shares one per Env
 ├─ interpreter interpreter.Interpreter   — holds {dispatcher, Container, provider, adapter, attrFactory}; dead after Plan
 ├─ interpretable / observable            — THE PLAN TREE: tree of evalConst / evalAttr / evalUnary|Binary|VarArgs (with the
 │                                          *functions.Overload impl baked in) / evalOr|And|Eq / evalList|Map / comprehension nodes
 │                                          → ∝ expression size; ~tens–hundreds of bytes per node  ← IRREDUCIBLE
 ├─ defaultVars (empty), interruptCheckFrequency, costLimit *uint64, costOptions []…, callCostEstimator (shared *library.CostEstimator)
 └─ plannerOptions / regexOptimizations   — construction-only; never read again after initInterpretable; retained dead weight
```

Notable: in `prog.Eval` / `prog.ContextEval` **only** `p.interpretable`/`p.observable`,
`p.defaultVars`, `p.interruptCheckFrequency` are touched. `p.dispatcher`, `p.interpreter`,
`p.plannerOptions`, `p.regexOptimizations` are written during `newProgram` and **never read
again** — they are retained dead weight on every program (cheap after 0009 makes the
dispatcher a tiny `ExtendDispatcher` overlay; ~150 KB without 0009). A complementary cel-go
change (orthogonal to 0009) would nil them in `initInterpretable`.

The planner resolves each call to a concrete `*functions.Overload` **at Plan time**
(`p.disp.FindOverload(id)`) and bakes it into the `evalUnary`/`evalBinary`/`evalVarArgs`
node, so at *eval* time the dispatcher is not consulted — which is why a fully-populated
per-program dispatcher is pure waste, and why a *pruned* per-program dispatcher (only the
overloads the AST references) would be tiny but is risky (the planner has fallback paths
that probe the dispatcher for names the checker env omitted). The shared-per-env dispatcher
(0009) is the safe form of "stop duplicating it".

### 3.3 Allocations per compile vs per evaluation

**Per compile** (`CompileCELExpression`, one expression):
- parse: a parsed AST tree + source-info maps — transient (handed to `env.Program`, then GC'd).
- check: a type-checked AST (reference map, type map) — transient on the k8s path; the
  lazily-built `checker.Env` is cached on the env (so once per env, not per compile).
- `env.Program`: a fresh `defaultDispatcher` map; `len(e.functions)` × (`fn.Bindings()` →
  `[]*functions.Overload` + a `*functions.Overload` struct + per-overload guard closures) —
  this is the dominant *retained* allocation pre-0009; a `defaultInterpreter`; an
  `AttributeFactory`; the plan tree; for cost tracking the `ObservableInterpretable`
  wrapper. → ~the whole `cel.Program` footprint.
- k8s extra: `cel.AstToCheckedExpr(ast)` proto tree — allocated then discarded (~17 KB /
  ~250 allocs for a complex expression; patch 0012); singleton `ReturnTypes()` slices
  (0012).

**Per evaluation** (`Program.ContextEval`, one request):
- k8s: `*evaluationActivation` — `new`'d per request per filter, **not pooled** (cel-go's
  `activationPool` only kicks in for `map[string]any` inputs; k8s passes an `Activation`,
  so the pool is bypassed). A `lazy.MapValue` for `variables` — per request, per
  variable-bearing policy (0006 removes for zero-variable policies).
- cel-go: `ctxEvalActivation` — POOLED (`ctxActivationPool`). A `CostTracker` per eval
  (has an `overloadTrackers` map only if per-overload trackers are registered, which k8s
  does not, so it's small). Intermediate `ref.Val`s produced while walking the plan tree —
  ∝ expression complexity; the unavoidable runtime cost.

So: the **retained** heap is born in `env.Program`; the **transient** garbage is born in
`env.Compile` (+ the discarded `CheckedExpr`); the **per-request** garbage is small and
mostly poolable (`evaluationActivation` is the one missed pool).

### 3.4 What is immutable / shareable

| Object | Mutated after construction? | Shareable across…? |
|---|---|---|
| `*cel.Env` (and its `variables`, `functions` map, `macros`, `provider`, `adapter`, `prsr`, lazily-built `chk`) | No — `Env.Extend` deep-copies into the *child*; the parent is untouched | all programs derived from it (already true); all *compilers* derived from the same template (patch 0002) |
| populated `interpreter.Dispatcher` (built from `e.functions`) | No | all programs of an env (patch 0009) |
| `*functions.Overload` + its guard closures | No | everywhere |
| stdlib `Macro`s | No | everywhere |
| type-checked AST / `*cel.Ast` | No | all programs of the *same expression* — i.e. the warm-tier cold representation; also feeds the dedup hot tier |
| the plan tree (`Interpretable`) | No, after `Plan` | only two policies with the *same expression text* share one tree (= dedup, patch 0001) — there is no safe sub-tree sharing across different expressions (nodes carry IDs / source positions) |
| `*evaluationActivation` | Yes (per request) | nothing — but **poolable** between requests |
| `CostTracker` | Yes (per eval) | nothing |

---

## 4. The redesign: a dedup-keyed, three-tier, demand-driven cache

### 4.1 Key on the *normalized expression*, not the policy

This is the load-bearing decision. In real multi-tenant clusters policies are produced by
controllers/templates (Gatekeeper-constraint-style, Kyverno-policy-style, org policy
libraries): **10 k–100 k policy objects, but a few hundred to a few thousand *distinct*
CEL expressions.** If the cache key is `normalize(expressionText) + envType + optionalVarDecls
+ returnTypes + variableSignature` (this is exactly patches 0001 + 0003), then:

* `N_distinct_expressions ≪ N_policies` in the common case → the "hot" cache at 5 k entries
  may simply hold *everything that is ever evaluated* → the problem is gone without any
  laziness at all (just bound the existing dedup cache — patch 0008, bumped post-0009).
* The cold tier exists only for the long tail of genuinely-unique expressions, which is
  small.

Normalization that is **safe**: strip comments, collapse insignificant whitespace, canonicalize
numeric/string literal forms. Normalization that is **unsafe** and not worth it: alpha-renaming
variables (names are semantically meaningful in CEL — `variables.x` vs `variables.y`),
reordering commutative operators (changes short-circuit order and therefore cost-accounting
and observable side effects). So: **textual normalization yes; semantic canonicalization no.**

### 4.2 The three tiers

```
Tier H — HOT (compiled):   key → cel.Program        — bounded (≈5 000 entries post-0009), W-TinyLFU admission/eviction
Tier W — WARM (checked):   key → *cel.Ast | CheckedExpr bytes  — bounded larger (≈50 000), LRU; "skip parse+check on a Tier-H miss"
Tier C — COLD (source):    key → normalized string + envType + optsDecls + returnTypes + varSig  — effectively unbounded, ~bytes each
```

Plus the existing per-policy plumbing: each `Validator`/filter holds, instead of a
`CompilationResult{ Program }`, a **handle**: `{ key, lazy func() (cel.Program, error) }`.
On evaluation the handle does: `Tier H.Get(key)` → hit, use it; miss → `Tier W.Get(key)` →
hit, `prog = env.Program(ast)`, `Tier H.Put(key, prog)` (W-TinyLFU decides admission),
use it; miss → reconstruct the `*cel.Ast` from the Tier-C string via `env.Compile(text)`,
`Tier W.Put`, `prog = env.Program(ast)`, `Tier H.Put`, use it.

(Tier W can be omitted in a v1 — a Tier-H miss just re-runs the full compile from Tier C.
The win from Tier W is purely "don't re-type-check"; it matters under churn / large hot-set
turnover, not in steady state.)

### 4.3 Where it lives in the code

* **`pkg/admission/plugin/cel/compile.go`** — `CompileCELExpression` returns a
  `CompilationResult` whose `Program` is replaced by (or augmented with) a lazy handle; the
  three tiers are package-level singletons (like the existing `globalCompileCache` from
  patch 0001).
* **`pkg/admission/plugin/cel/activation.go`** — `evaluationActivation.Evaluate` resolves
  the handle (`compilationResult.program()`) before `ContextEval`. This is the one place
  the request path now possibly compiles.
* **`pkg/admission/plugin/policy/generic/policy_source.go`** — `compilePolicyLocked` keeps
  doing eager compilation **only for the bounded eager set** (§4.7); for the cold tail it
  stores only the Tier-C entries and the handles, never calling `env.Program`. Frequency
  stats (for W-TinyLFU and for the "active set") are updated on the request path and read
  by `refreshPolicies`.
* No public API change; no change to the `Validator` / `PolicyHook` contract; no change to
  evaluation semantics for a warm/hot policy.

### 4.4 Eviction: W-TinyLFU, not LRU or LFU

* **Plain LRU** on Tier H is the obvious choice and what patch 0001 ships — but it is
  vulnerable to **cache-miss amplification**: a burst of admissions touching many cold
  policies (a scan-like access pattern — e.g. a controller doing a full resync, or a new
  tenant onboarding 1 000 policies whose traffic arrives together) evicts the genuinely-hot
  entries, after which *every* request misses and recompiles → CPU/GC storm → latency
  cliff. This is the single biggest risk of going lazy.
* **Plain LFU** resists scans but has its own pathologies: stale-frequency lock-in (an
  expression that was hot last week never leaves), and it needs aging.
* **W-TinyLFU** (the Caffeine admission policy: a tiny count-min sketch tracks recent
  frequency; on a miss, a candidate is *only admitted* if its sketch frequency exceeds that
  of the LRU victim it would evict; the sketch is periodically halved for aging) gives:
  scan resistance (one-hit-wonders never displace established entries), recency *and*
  frequency awareness, O(1) ops, and — crucially — **fairness**: a noisy cold tenant
  generates lots of *distinct* low-frequency keys that the admission filter rejects, so it
  cannot evict a quiet hot tenant's program. Cost: a count-min sketch (~a few KB for a 5 k
  cache) and a tiny bit of per-access bookkeeping.
* **Per-tenant partitioning** (a sub-cache per namespace with a quota, or a weighted LRU
  keyed by `(namespace, expression)`) is an additional fairness lever for the
  pathological "one tenant owns 90 k of the 100 k policies" case — but W-TinyLFU alone
  handles the common multi-tenant case, and partitioning adds real complexity (quota
  tuning, rebalancing). Recommend W-TinyLFU first; partitioning only if a real cluster
  shows starvation.

### 4.5 Concurrency

Today the request path is essentially lock-free: one `atomic.Pointer` load of the
`[]PolicyHook` snapshot, then immutable `Validator`s. Adding a shared mutable cache on the
request path is the main concurrency hazard. Mitigations, in order of preference:

1. **Lock-free hot tier on top.** A background goroutine (the existing `refreshPolicies`
   tick is a natural home) periodically rebuilds an *immutable* `map[key]cel.Program` of the
   top-K-by-frequency and publishes it via `atomic.Pointer`. The request path does an
   `atomic.Pointer` load + map read for the top-K (lock-free, the overwhelming common case);
   only on a miss does it fall through to the locked W-TinyLFU tier. This preserves today's
   lock-free fast path for the steady state.
2. **Shard the W-TinyLFU tier** into N stripes by `hash(key) % N` (N ≈ GOMAXPROCS×4), each
   with its own mutex + sketch. Cuts contention ~N× and is much simpler than (1). Eviction
   is per-shard (slightly less optimal globally, irrelevant in practice).
3. **`singleflight` per key** for the *compile* step on a miss, so N concurrent admissions
   of the same cold policy trigger exactly one `env.Program` call and the rest wait on it
   (the same `golang.org/x/sync/singleflight` already used for `MustBaseEnvSet`). Without
   this, a burst on one cold policy → N redundant compiles.
4. **Global semaphore** bounding concurrent lazy compiles (e.g. `GOMAXPROCS`), so a burst
   across *many distinct* cold policies queues rather than CPU-storming. Requests waiting
   on the semaphore see a brief latency bump, not a meltdown.
5. The frequency sketch updates must be cheap and lock-light — a sharded count-min sketch,
   or atomic increments into a fixed array; never a global mutex per request.

Recommendation: **(2)+(3)+(4) for v1** (sharded W-TinyLFU + singleflight + compile
semaphore); add **(1)** if profiling shows the sharded-mutex path still hot at extreme
admission rates.

### 4.6 Cold-miss handling and fallback semantics

* On a Tier-H miss the request **waits** for `env.Program` (singleflighted, semaphore-bounded).
  It does *not* fail-open or fail-closed prematurely — the policy was type-checked at VAP
  admission time (`typechecking.go`) and again, presumably, when it last passed through
  `refreshPolicies`, so a *compile error* on the request path is a should-never-happen
  internal error; if it does happen, surface it exactly as today (`CompilationResult.Error`
  → the filter returns an error → `failurePolicy` applies). Do **not** invent a new
  "compiling, try later" admission outcome — that would change semantics.
* Rate-limit + semaphore ensure the worst case is "this request is ~5–50 ms slower",
  bounded, not "the apiserver melts".

### 4.7 Hybrid: keep eager compilation for the working set

Pure-lazy regresses cold-start and first-touch latency. The fix: **`refreshPolicies`
continues to eagerly compile a bounded set** —
- the current top-K hot expressions (from the frequency sketch — survives within a process
  lifetime; lost on restart), **and**
- the policies whose namespace has had admission traffic in the last `T` minutes
  ("active namespaces" — a cheap per-namespace last-seen timestamp updated on the request
  path), **and**
- always at least the first `floor` policies in informer order (so tiny clusters behave
  exactly as today — graceful degradation),
up to a global budget (e.g. `min(5 000, configured-cap)`). Everything beyond the budget is
stored Tier-C only and compiled lazily on first match.

This recovers today's "zero compile on the request path" property for the steady-state
common case, bounds resident `cel.Program`s at the budget, and confines the lazy path to
genuinely-cold policies (which, by the dedup keying in §4.1, are rare). It also means
**cold-start** does the eager pass over the budget set (≈ today's startup cost, capped) and
then warms the rest on demand — the warm-up window has elevated p99 only for the
*never-before-seen-this-process* expressions.

---

## 5. Trade-off analysis (point by point)

**Memory savings.** Scenario: 50 000 policies × ~13 expressions = 650 000 program slots.
- *Templated cluster* (≈1 000 distinct expressions): with dedup keying (patches 0001+0003)
  + 0009, resident = ~1 000 × ~50 KB ≈ **~50 MB**, lazy or not. The redesign adds nothing
  here — the patch series already wins.
- *Unique-expression cluster* (≈650 000 distinct expressions): eager today =
  650 000 × ~150 KB ≈ **~97 GB** (pre-patch) / × ~50 KB ≈ **~33 GB** (post-0009) — clearly
  untenable; **this is the case the redesign exists for.** Lazy with a 5 000-entry hot tier
  + 50 000-entry warm AST tier + raw-string cold:
  hot ≈ 5 000 × 50 KB ≈ 250 MB; warm ≈ 50 000 × ~8 KB ≈ 400 MB; cold ≈ 650 000 × ~200 B ≈
  130 MB → **≈ 0.8 GB total**, a ~40–120× reduction, and *flat* in `N_policies` beyond the
  warm tier. (Drop the warm tier → hot 250 MB + cold 130 MB ≈ 0.4 GB, at the cost of
  re-type-checking on hot-tier turnover.)

**Compile-time vs runtime.** Today: 100 % of compile cost is background, debounced,
amortized; the request path never compiles. Lazy: cold→hot transitions move compile cost
onto the request path. Hybrid (§4.7): the working set is still eager, so steady-state has
*zero* request-path compile; only first-ever-touch of a cold expression pays it. Net trade:
trade a constant ~`N_policies` background-compile load (which today is the only thing
keeping all those programs warm — wasted work for policies that never fire) for an
occasional request-path blip on genuinely-cold policies. Strictly less *total* CPU
(don't compile what's never used) but spikier where it does happen.

**Request latency / p99 / SLA.** Hot/warm path: unchanged (cache hit; possibly +1 sharded-mutex
acquire if not using the lock-free top-K front). Cold-miss path: **+~5–75 ms** for the
whole policy on first touch (Plan-only if Tier W hit: +~hundreds of µs–few ms;
parse+check+plan if Tier C only: +~5–75 ms), once per cold→hot transition.
Admission requests have an overall apiserver request timeout (typically 30–60 s) — a
sub-100 ms blip is well within budget for p99 but *will* show up in p999 and in any "first
request after onboarding a new tenant" measurement. Mitigations: §4.7 hybrid (most
expressions are pre-warmed), §4.5 semaphore (bounded worst case), Tier W (cheap re-warm),
and — for the truly latency-sensitive — allow operators to pin the eager budget high
(degrades to today's behavior). **Net SLA impact: negligible p50/p99 in steady state;
a bounded p999 tax and a cold-start warm-up window.**

**Cache eviction.** §4.4 — W-TinyLFU, not LRU; sharded; aged; per-tenant partitioning only
if needed. Plain LRU (patch 0001) is fine for the *bounded dedup cache* use (where the cap
is generous relative to the working set) but inadequate as the eviction policy for a
demand-driven hot tier under adversarial/scan load.

**Admission throughput.** Hot path: ~unchanged (the cache hit is a map read; the only new
cost is a possible sharded-mutex acquire — sub-µs — or nothing with the lock-free front).
Cold path: a compile blocks one request goroutine for ms; with the semaphore, a burst of
cold misses queues (throughput dips briefly, doesn't collapse). Steady-state throughput
under the hybrid is the same as today. The thing that *improves* throughput indirectly:
much smaller live heap → cheaper GC → less STW-ish stalling under memory pressure.

**Concurrency / lock contention.** §4.5. New shared mutable state on the request path is
the hazard; sharding + a lock-free top-K front + singleflight neutralize it. The frequency
sketch must be sharded/atomic, never a global lock.

**GC pressure.** Today: compilation garbage (transient AST/CheckedExpr churn) is generated
on the 1 s refresh tick — predictable, low rate, but the *retained* set is enormous (huge
live heap → expensive mark phase, frequent GC under a tight `GOGC`). Lazy: cold compiles
generate the same transient garbage but *on the request path* — spikier, correlated with
traffic to cold namespaces, bounded by the compile semaphore. The big win: **the retained
set shrinks ~10–100×**, so each GC cycle is cheaper and the heap fits comfortably under
`GOGC=100` instead of needing aggressive `GOMEMLIMIT`/`GOGC` tuning. Net: more frequent
*small* allocations, far less *retained* — GC-positive if the hot set is stable,
GC-neutral-to-negative only if a cluster has pathological cold→hot churn (in which case the
semaphore bounds it).

**Program reuse.** Dedup keying (§4.1) makes the hot tier hold *expressions*, not
*policies* — 1 000 policies sharing `object.spec.replicas <= 5` ⇒ one `cel.Program`,
warmed on the first request through any of them. This is the multiplier that makes 5 000
hot slots enough for a 100 k-policy cluster. Within an expression, no further reuse is
available (cel-go has constant-folding via `OptOptimize`, already enabled, but not
cross-/intra-expression CSE of plan nodes; not worth pursuing).

**Policy churn.** Today: a CI/CD pipeline or operator that creates/updates many policies
generates a steady background recompile load (every churned policy is recompiled on the
next tick whether or not it's used). Lazy: a churned policy that nobody evaluates is
*never compiled* — its old Tier-H/W entries age out, its Tier-C string is replaced, done.
Big win for "policy library / GitOps reconcile" churn. Caveat: a churned policy that *is*
hot pays a recompile on next request after each update — same as today, just deferred.

**Burst traffic.** A sudden burst of requests into a namespace whose policies are all cold:
without mitigation, a simultaneous compile storm. With §4.5: singleflight collapses
duplicates (per-expression), the semaphore bounds concurrent distinct compiles to
~`GOMAXPROCS`, and the rest queue for a few ms. Realistically: the first wave to a cold
namespace pays the compile, the warm-namespace tracking (§4.7) then keeps it eager. The
worst case is "first burst after onboarding is ~tens of ms slower per request until the
namespace's expressions are warm" — acceptable for nearly all SLAs; pin the eager budget
or pre-warm namespaces with known traffic patterns if not.

**Warm-up / cold start.** Today: zero warm-up — `refreshPolicies` runs before the apiserver
serves and everything is compiled. Lazy/hybrid: cold start does the eager pass over the
budget set (≈ today's startup cost, capped) then warms the rest on demand → a warm-up
window with elevated p999 for first-touch expressions. **This is the principal regression.**
Mitigations: keep the eager budget reasonably large (≥ the typical active set); compile the
budget in informer-sync order so the most-recently-changed (likely most-relevant) policies
warm first; optionally persist the frequency-sketch top-K to a configmap/file and re-seed
on restart (extra complexity — probably not worth it for v1).

**Cache-miss amplification.** §4.4 — the W-TinyLFU admission filter is specifically the
defense: scan/burst access generates low-frequency candidates that fail admission and so do
not evict the established hot set, breaking the thrash loop. Without it (plain LRU under
adversarial load) the lazy design is *worse* than today. With it, it's robust.

**Fairness between hot and cold tenants.** W-TinyLFU's frequency-gated admission gives this
mostly for free (a cold tenant's many distinct low-frequency keys can't displace a hot
tenant's high-frequency ones). For the extreme "tenant A owns 95 % of all policies" case,
add per-namespace cache quotas (§4.4) so A's cold tail can't consume more than its share of
the hot tier even if its aggregate frequency is high. The eager budget (§4.7) should
likewise be apportioned with a per-namespace cap, not first-come-first-served, so a tenant
that registers 50 k policies at boot doesn't fill the eager budget and starve everyone else.

**Multi-tenant, huge policy counts.** This is the design target. The combination that
makes it work: (a) dedup keying → resident ∝ distinct expressions, not policies; (b) a
bounded hot tier with W-TinyLFU → resident ∝ active expressions, not total; (c) per-tenant
fairness on both the hot tier and the eager budget → no tenant starves another; (d) the
hybrid eager set → no warm-up tax for the active tenants. A 100 k-policy / 5 000-tenant
cluster where each tenant's traffic touches ~10 expressions ⇒ ~50 k active expressions ⇒
warm tier holds them all (~400 MB) + hot tier holds the global top 5 k (~250 MB) + cold
strings for the rest (~130 MB) ⇒ ~0.8 GB, flat, vs ~33 GB eager post-0009 / ~97 GB today.

---

## 6. Alternative architectures, ranked

| # | Architecture | What it is | Memory vs today | Request-path cost | Complexity | Verdict |
|---|---|---|---|---|---|---|
| A | **Bounded LRU dedup-program cache** | patches 0001 + 0003 + 0008, key = normalized expr; cap ~1.5 k → ~5 k post-0009 | huge in templated clusters (resident ∝ distinct exprs); none in unique-expr clusters | none (still eager-compiled into the cache off-path) | **low** | **Ship now.** Solves the common case outright. |
| B | LFU hot-expression cache | as A but LFU eviction | same | none | low | Marginal over A; stale lock-in; skip in favor of D. |
| C | **Async background compilation, bounded** | `refreshPolicies` only eager-compiles the working set (top-K + active namespaces + floor); cold tail stays uncompiled until used | resident ∝ working set, not total — **breaks the asymptote**, still off-path for the warm case | none for warm; first-touch compile for cold | medium | **The pragmatic core of the redesign** (= §4.7). |
| D | **Tiered cache (hot Program + warm AST + cold string), W-TinyLFU** | §4 in full | resident ∝ active set, flat in `N_policies`; ~0.4–0.8 GB at 100 k unique exprs | warm hit = none; Tier-H miss/Tier-W hit = +Plan (~ms); cold = +full compile | medium-high | **The full answer.** Layer it on top of A+C. |
| E | Speculative warming | compile on policy create, *evict if unused within T*; or pre-warm a namespace's policies on first touch | resident ∝ recently-created ∪ recently-active | none if guess right; wasted compile if wrong | medium | Useful add-on to C/D; not a standalone strategy. |
| F | Precompile only for active namespaces | a special case of C: the "working set" = policies in namespaces with recent admission traffic | resident ∝ active-namespace policies | none for active; first-touch for newly-active | low-medium | Good cheap heuristic; fold into C's working-set definition. |
| G | Policy grouping / shared execution plans | group policies with an identical *set* of expressions; one shared `Validator` | resident ∝ distinct policy *shapes* | none | medium | A generalization of dedup to the whole `Validator`; real win when controllers stamp out byte-identical policies; complements D. |
| H | Bytecode-like reusable IR | serialize `CheckedExpr` proto as the portable IR; re-`Plan` on demand | the IR *is* Tier W of D | re-Plan on demand | — | **This is Tier W.** A true bytecode VM for CEL is a cel-go rewrite — out of scope; note that the tree-walking interpreter's per-node objects are themselves a (small, irreducible-for-now) cost. |
| I | Pooled activations / interpreters | pool `evaluationActivation` between requests; nil the dead `dispatcher`/`interpreter`/planner-opt fields on `prog` after `Plan` (cel-go) | small constant per-request alloc reduction; the `prog`-nil change saves ~150 KB/program *without* 0009 (cheap-to-add either way) | none | low | **Do it** — orthogonal, cheap, stacks with everything. |
| J | Per-tenant partitioned cache | sub-cache per namespace with a quota | resident ∝ Σ per-tenant quotas | none extra | medium | Fairness backstop for pathological multi-tenant skew; only if a real cluster shows starvation. |

**Recommended composition: A (now) → C (the hybrid eager budget) → D (the tiered lazy
cache with W-TinyLFU) + G + I, with J held in reserve.**

---

## 7. Specific sub-questions

* **Is AST-only caching sufficient?** No, not by itself — you still pay `env.Program`
  (`Plan` + the dispatcher build, the retained-heap-heavy stage) on every evaluation if you
  don't cache the `cel.Program`. AST caching is valuable as the *warm tier* (skip the
  expensive parse+check on a hot-tier miss), not as the only tier.
* **Can interpreter nodes be partially reused?** Across different expressions: no — plan
  nodes carry expression IDs / source positions and the tree shape is expression-specific;
  no safe sub-tree sharing. Within an expression: cel-go does constant folding (`OptOptimize`,
  already on) but not common-subexpression elimination; adding CSE is a cel-go change with
  modest payoff and real risk (it perturbs cost accounting and IDs) — not worth it. The
  only "reuse" lever is whole-expression dedup (patch 0001), which the redesign already
  centers on.
* **Is `env.Program()` the real memory-heavy stage?** Yes — it produces the `cel.Program`,
  which holds the per-program dispatcher copy (≈150 KB pre-0009; eliminable) and the plan
  tree (∝ expression; irreducible). `env.Compile` (parse+check) produces *transient*
  garbage that the k8s path discards. So: defer `env.Program`, keep at most the cheap
  `env.Compile` output (the checked AST) or just the string.
* **Are dispatcher tables duplicated unnecessarily?** Yes — `cel.newProgram` builds a fresh
  `defaultDispatcher` per program and copies *all* of `e.functions` into it regardless of
  what the expression uses (the planner then only references the few overloads it needs).
  Patch 0009 (share one dispatcher per `Env`) fixes the duplication; nil-ing the dead
  `prog.dispatcher`/`prog.interpreter` after `Plan` (a separate cel-go change) fixes the
  retention even without 0009.
* **Can activation reuse reduce allocations?** Yes — `*evaluationActivation` is allocated
  per request per filter and **not pooled** (cel-go's `activationPool` only triggers for
  `map[string]any` inputs; k8s passes an `Activation`). A `sync.Pool` for it removes one
  per-request allocation on the hot path. The cel-go `ctxEvalActivation` is already pooled.
* **Can expression normalization deduplicate equivalent policies?** Yes — comment/whitespace/
  literal-form normalization (patches 0003 + 0001) collapses templated policies that differ
  only cosmetically into one cache entry; this is the multiplier that makes a small hot tier
  sufficient. Deeper *semantic* canonicalization (variable alpha-renaming, commutative
  reordering) is unsafe in CEL (changes evaluation/short-circuit/cost semantics) and not
  worth pursuing.

---

## 8. Eager vs lazy — head-to-head

| Dimension | Eager (today) | Eager + bounded dedup cache (patches) | Hybrid lazy (C+D, recommended) |
|---|---|---|---|
| Resident `cel.Program`s | `O(N_policies × E)` | `O(N_distinct_expr)`, capped | `O(active_distinct_expr)`, capped (~5 k hot + ~50 k warm-AST) |
| Resident bytes @100 k unique exprs | ~97 GB (pre) / ~33 GB (0009) | ~33 GB (cap = ∞) or capped | **~0.4–0.8 GB** |
| Resident bytes @100 k policies, ~1 k distinct exprs | ~15 GB (pre) / ~3 GB (0009) | **~50 MB** | ~50 MB (no improvement — A already wins) |
| Request-path compile | never | never | only first-touch of a cold expression |
| Steady-state p50/p99 | baseline | baseline | baseline |
| p999 / first-touch / onboarding latency | baseline | baseline | + bounded blip (~ms–tens of ms) |
| Cold-start warm-up | none | none | bounded eager pass + on-demand tail (elevated p999 during) |
| Background CPU (compile) | `O(N_policies)` per churn — wasted for unused policies | `O(N_distinct_expr)` per churn | `O(working_set)` — don't compile the unused |
| GC: live heap | huge | small-ish (templated) / huge (unique) | small, flat |
| GC: garbage rate | low, on the 1 s tick | low, on the tick | tick + traffic-correlated cold compiles (semaphore-bounded) |
| Lock contention on request path | none (atomic snapshot) | none (atomic snapshot of the slice; dedup cache only touched at compile time) | sharded cache mutex or lock-free top-K front + singleflight |
| Robustness under scan/churn/burst | trivially robust (nothing to evict) | robust (cap is generous) | needs W-TinyLFU + semaphore — robust *with* them, fragile *without* |
| Multi-tenant fairness | N/A (everything resident) | N/A | needs W-TinyLFU + per-tenant quotas on hot tier & eager budget |
| Implementation risk | — | low | medium-high (eviction/refcount/concurrency bugs → stale-program or leak) |

**Reading:** for the *templated* majority of real clusters, the patch series ("eager +
bounded dedup cache") is the right call — simple, low-risk, and it already collapses
`N_policies` to `N_distinct_expr`. The full lazy/tiered design earns its complexity only
for clusters with a large number of *genuinely unique* expressions and tight memory — and
even there, it should be built *on top of* the patch series (dedup keying, 0009's cheap
programs, 0006's zero-variable fast path), not instead of it.

---

## 9. Recommendations, estimates, bottlenecks, risks

**Sequenced recommendation**

1. **Ship patches 0009 → 0001 → 0008(cap, bumped) → 0012 → 0006 → {0004+0005 | 0010} → 0002 → 0003.**
   Outcome: per-program ~150–250 KB → ~30–60 KB; cache keyed on normalized expression text
   → resident ∝ distinct expressions. Estimated reduction for the common templated cluster:
   from `O(N_policies)` GB to **~tens–hundreds of MB**. Low risk. (Also do the §6-I items:
   pool `evaluationActivation`; nil the dead `prog` fields in cel-go.)
2. **Add the hybrid eager budget (architecture C / §4.7):** `refreshPolicies` eager-compiles
   only `working_set = top-K-hot ∪ active-namespace-policies ∪ first-floor-policies`, capped;
   the cold tail stores Tier-C only. Per-policy frequency + per-namespace last-seen are
   updated on the request path. This breaks the `O(N_policies)` resident asymptote while
   keeping the request path compile-free for the active set. Medium effort, contained to
   `policy_source.go` + `compile.go`.
3. **Add Tier W (checked-AST LRU) and lazy materialization on Tier-H miss (architecture D / §4.2–4.6):**
   the full demand-driven tiered cache with sharded W-TinyLFU + singleflight + a compile
   semaphore. Estimated reduction for the unique-expression / huge-multi-tenant cluster:
   from ~33 GB (post-0009 eager) to **~0.4–0.8 GB**, flat in `N_policies`.
4. **Add per-tenant fairness (J)** only if a real cluster shows hot/cold-tenant starvation.

**Estimated memory-reduction potential**

* Patch series alone, templated cluster: **~100–1000× → tens–hundreds of MB.**
* Patch series alone, unique-expression cluster: ~3–8× (constant-divide only; still GBs).
* + hybrid (2) + tiered (3), unique-expression / huge multi-tenant: **~40–120× over
  post-0009 eager; resident becomes flat in `N_policies`** → sub-GB at 100 k policies.

**Scalability bottlenecks that remain**

* The per-request `*evaluationActivation` and the per-policy *non-CEL* `Validator` overhead
  (the convert-slices, the four filter wrappers) — small per policy, but `O(N_policies)` if
  the `Validator`s themselves are kept resident for cold policies. The redesign should also
  make the cold `Validator` lightweight (store the binding refs + Tier-C handles, not a
  fully-built filter chain) — otherwise you've made `cel.Program`s lazy but kept an
  `O(N_policies)` tail of `Validator` plumbing.
* The base env itself: two near-identical mega-`cel.Env`s per `EnvSet` (`NewExpressions`
  and `StoredExpressions`), each with the full 100+-function map. Shared, so a fixed cost
  (not `O(N)`), but a few MB that `StdLibSubset` + library trimming + a CoW `e.functions`
  (cel-go) could shave — diminishing returns vs. the items above.
* The tree-walking interpreter: each plan node is a heap object; a true bytecode VM would
  shrink that, but it's a cel-go-scale project and the per-node cost is small relative to
  the dispatcher/duplication problems already addressed.
* The `refreshPolicies` single "fat-fingered" `s.lock` — fine today (background, 1 s tick)
  but if the eager-budget recompute grows expensive, it serializes; finer-grained locking
  (per-policy or copy-on-write of the `compiledPolicies` map) is a known follow-up the
  in-tree comment already flags.

**Risks / regressions**

* **Cold-start and first-touch latency** — the headline regression; mitigated by the hybrid
  eager budget (degrades to today's behavior when the budget ≥ the policy count) and the
  compile semaphore (bounds the worst case).
* **Cache-miss amplification under scan/churn/burst** — fatal with plain LRU; fine with
  W-TinyLFU + semaphore. The eviction policy is not optional.
* **Request-path lock contention** — neutralized by sharding + a lock-free top-K front +
  singleflight; must be measured at 10 k+ req/s before shipping.
* **Correctness of eviction/invalidation** — a bug that serves a stale `cel.Program` after a
  policy update (wrong `variableSignature` in the key, missed invalidation) is a *semantic*
  regression, not just a perf one; the cache key must include everything that affects
  compilation (envType, optionalVarDecls, returnTypes, variableSignature — exactly the
  patch-0001 key), and policy-update invalidation must be airtight (refcount or
  generation-stamped entries).
* **Memory leak via refcounting bugs** — if Tier-C/W entries are refcounted by policy and a
  decrement is missed on delete, they accumulate; prefer a generation-stamped sweep
  (`refreshPolicies` drops entries for keys no live policy references) over manual refcounts.
* **Operator-visibility regression** — today every policy's compile error is known eagerly
  (surfaced in status / `ConfigurationError`); a lazy design must still type-check eagerly
  (it does — `typechecking.go` at VAP admission time, plus the eager-budget pass) so that a
  syntactically/typically-broken policy is still rejected/flagged without waiting for a
  matching request. Keep the type-check eager; only defer the *plan*.
* **Complexity** — the tiered design is substantially more code and more failure modes than
  "compile everything, keep forever"; for many clusters the patch series is the better
  cost/benefit. Gate the lazy path behind a feature flag and a configurable cap; default it
  conservatively.

---

## 10. Relationship to the patch series

The patches in [`../patches/`](../patches/) and this redesign are **complementary, not
alternative**:

* 0001 (dedup `cel.Program` by normalized expression text) + 0003 (normalization) **are the
  cache keying** the redesign assumes — without dedup, a bounded hot tier can't cover a
  huge-policy cluster.
* 0009 (share the dispatcher per `Env`) **makes each cached program cheap enough** that a
  5 k-entry hot tier is ~250 MB, not ~750 MB.
* 0008 (cap the cache) **is** the bounded hot tier in embryo — bump the cap post-0009 and
  swap LRU → W-TinyLFU and it's most of architecture A.
* 0006 (zero-variable fast path) and 0004/0005/0010 (lazy/shared env matrix) and 0002
  (shared compiler) **shrink the per-policy plumbing** that the cold tail would otherwise
  keep resident.
* 0012 (drop the discarded `CheckedExpr`) **frees the proto** that Tier W would otherwise
  want to keep — or, conversely, *keep* it (don't discard) and it becomes Tier W's stored
  form for free.

So the practical roadmap is: **land the patch series first** (it's the low-risk majority of
the win), then **add the hybrid eager budget** (breaks the asymptote with little new
request-path risk), then **add the tiered lazy cache with W-TinyLFU** (the full answer for
the extreme-scale / unique-expression / memory-constrained case), behind a feature gate.
