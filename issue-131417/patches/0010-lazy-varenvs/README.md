# 0010 — Lazify per-compiler `varEnvs`, share request/namespace DeclTypes

**Status:** ready (well-formed diff; `git apply` verified on top of 0001).
**Side:** k8s (`staging/src/k8s.io/apiserver/pkg/admission/plugin/cel/compile.go`
plus a new benchmark in `policy/validating`).
**Headline impact:** **~14× cut in per-policy compile-time allocation,
~24 % cut in per-policy retained heap**, with no change to public API
or observable evaluation behaviour.

**Prerequisite:** patch **0001** (process-wide compiled-program cache). The diff
edits the `compiler` struct, `newCachedCompiler` and `compileFresh` introduced
by 0001 and the `"strings"` import it adds, so it cannot apply to a bare master;
apply 0001 first.

**Mutually exclusive with patches 0004 + 0005.** 0010 and 0004 are two
implementations of the same idea (lazy `varEnvs`) and both reshape the same
region of `compile.go`; 0005 builds on 0004. Pick **either** `{0004 (+0005)}`
**or** `{0010}` — never both. 0010 additionally hoists `BuildRequestType` /
`BuildNamespaceType` into package-level singletons, which 0004 does not.
(0010 is compatible with 0006, which doesn't touch `compile.go`.)

## What this patch is *not*

This patch is **not** about CEL programs. It addresses the
**per-policy framework overhead** that is paid even when a policy has
zero validations, zero matchConditions, zero auditAnnotations, zero
messageExpressions, and zero variables. That overhead is significant
and was previously hidden behind the larger CEL retention numbers.

It is also distinct from patches 0004 / 0005 / 0006 in this directory:

- **0004** lazifies the `varEnvs` matrix per compiler. This patch
  achieves the same *retained-heap* effect with a much simpler
  implementation, plus also hoists `BuildRequestType` /
  `BuildNamespaceType` to package-level singletons (which 0004 does
  not).
- **0005** shares the `varEnvs` matrix *across* compilers via a
  process-wide template-keyed pool. That is a strictly bigger
  optimisation than this patch (drops the per-policy term to roughly
  zero) but at substantial complexity. This patch is the much smaller
  intermediate step.
- **0006** removes the unconditional variables-overlay Extend in
  `NewCompositedCompiler` for zero-variable policies. That is a
  parallel, additive optimisation — see "What is left" below.

In short: 0010 is the patch you ship today. 0005 + 0006 are what you
want once 0010 has bedded in.

## Problem statement

Every successful `compilePolicy(policy)` call routes through
`cel.NewCompositedCompiler(template)` →
`newCachedCompiler(state.EnvSet, envSet)` →
**`mustBuildEnvs(state.EnvSet)`**. That last step builds **8**
`*environment.EnvSet`s eagerly — the full Cartesian product of:

- `HasParams ∈ {false, true}`
- `HasAuthorizer ∈ {false, true}`
- `HasPatchTypes ∈ {false, true}`

= 8 distinct `OptionalVariableDeclarations` keys, each requiring two
cel-go `cel.Env.Extend` clones (one for `NewExpressions`, one for
`StoredExpressions`) = **16 cel-go env clones per compiler**.

VAP only ever asks for **2** of those 8 combinations
(`{HasParams: hasParam, HasAuthorizer: true}` for validations / audits
and `{HasParams: hasParam, HasAuthorizer: false}` for message
expressions). MAP asks for at most 4. So 4–6 of every compiler's 8
env slots are dead weight from the moment of construction.

In addition, each `mustBuildEnvs` call rebuilds the same static
`namespaceType` / `requestType` `*apiservercel.DeclType` trees via
`BuildNamespaceType()` / `BuildRequestType()` — these are pure
functions over compile-time constants and could be process-globals.

## What the patch does

```go
type variableDeclEnvs struct {
    base          *environment.EnvSet
    namespaceType *apiservercel.DeclType
    requestType   *apiservercel.DeclType
    cache         sync.Map // OptionalVariableDeclarations -> *envEntry
}

type envEntry struct {
    once sync.Once
    env  *environment.EnvSet
    err  error
}

func (v *variableDeclEnvs) get(opts OptionalVariableDeclarations) (*environment.EnvSet, error) {
    actual, _ := v.cache.LoadOrStore(opts, &envEntry{})
    e := actual.(*envEntry)
    e.once.Do(func() {
        e.env, e.err = createEnvForOpts(v.base, v.namespaceType, v.requestType, opts)
    })
    return e.env, e.err
}

var (
    cachedNamespaceType = BuildNamespaceType()
    cachedRequestType   = BuildRequestType()
)
```

`mustBuildEnvs` is deleted. `compiler.varEnvs` becomes
`*variableDeclEnvs`. `CompileCELExpression`'s lookup site changes
from `c.varEnvs[options].Env(envType)` to a `varEnvs.get(options)`
call.

Concurrent compiles for the same `OptionalVariableDeclarations` key
serialise through `sync.Once` and observe the same env (or the same
error). Concurrent compiles for *different* keys run lock-free
through the `sync.Map`.

## Empirical results

### Methodology

The benchmark file
`staging/src/k8s.io/apiserver/pkg/admission/plugin/policy/validating/per_policy_overhead_bench_test.go`
isolates the non-CEL framework cost two ways:

1. `BenchmarkPerPolicyOverhead_Empty` repeatedly calls
   `compilePolicy(emptyPolicy)` where `emptyPolicy` has zero CEL
   anything. Measured via `go test -bench -benchmem`.
2. `TestRetainedHeapPerEmptyPolicy` compiles 5 000 empty policies,
   retains all of them in a slice, triple-`runtime.GC()`s, and
   reports `runtime.MemStats.HeapInuse` delta divided by N.

To reproduce:

```bash
# Compile-time allocations (transient + retained):
go test -run='^$' -bench='BenchmarkPerPolicyOverhead' \
  -benchmem -benchtime=2000x -count=3 \
  ./staging/src/k8s.io/apiserver/pkg/admission/plugin/policy/validating/...

# Retained-heap floor at scale:
go test -run='TestRetainedHeapPerEmptyPolicy|TestRetainedHeapPerOneValidation' \
  -v -count=1 \
  ./staging/src/k8s.io/apiserver/pkg/admission/plugin/policy/validating/...

# To see allocation hot-spots before/after:
go test -run='^$' -bench='BenchmarkPerPolicyOverhead_Empty$' \
  -benchmem -benchtime=2000x -memprofile=/tmp/empty.mem -count=1 \
  ./staging/src/k8s.io/apiserver/pkg/admission/plugin/policy/validating/...
go tool pprof -top -alloc_space /tmp/empty.mem | head -25
```

### Numbers

| Metric                                           | Before     | After       | Delta    |
|--------------------------------------------------|------------|-------------|----------|
| **Retained heap per policy** (5 000 empty pols)  | 56.4 KiB   | 42.6 KiB    | **−24.5 %** |
| HeapInuse @ 5 000 policies                       | 275.4 MiB  | 208.0 MiB   | −67.4 MiB |
| **bytes/op** (per compile)                       | 784 042 B  | 55 598 B    | **−93.0 %** |
| **allocs/op** (per compile)                      | 6 904      | 349         | **−94.9 %** |
| **ns/op** (per compile)                          | 1 451 797  | 94 149      | **−93.5 %** |
| Mallocs/policy (retain test)                     | 6 903      | 350         | −95 %    |

Equivalent compile cost / one trivial validation policy: 99 µs
(was 1.5 ms), confirming the eager 8-env build was the dominant cost
even when the actual CEL compile is trivial.

### Allocation hot-spot analysis (pre-patch)

Top allocators inside `compilePolicy(emptyPolicy)`, captured with
`go tool pprof -alloc_space`:

```
30.29 %  cel.(*Env).Extend
24.55 %  checker.(*Group).copy
 7.65 %  fmt.Sprintf                   ← in apiservercel.buildDeclTypes
 7.59 %  cel.(*Env).configure  Macros
 4.78 %  apiservercel.buildDeclTypes
 4.03 %  decls.(*FunctionDecl).Merge
 3.86 %  decls.(*FunctionDecl).OverloadDecls
```

92.98 % of these allocations sit beneath `mustBuildEnvs` (= 8 ×
`createEnvForOpts` per compiler), which this patch eliminates for
unrequested combinations.

### Allocation hot-spot analysis (post-patch)

After this patch the pprof picture for `compilePolicy(emptyPolicy)`
is dominated by:

```
~75 %    cel.(*Env).Extend            ← from the single variables-overlay Extend
~10 %    NewObjectType / NewDeclField ← per-policy "kubernetes.variables" DeclType
~10 %    *condition / *CompositedConditionEvaluator wrapper structs (4×)
~ 5 %    everything else
```

The remaining `Env.Extend` cost is the unconditional variables overlay
in `NewCompositedCompiler` itself ([composition.go:69](../../../staging/src/k8s.io/apiserver/pkg/admission/plugin/cel/composition.go#L69)).
For zero-variable policies that overlay is unnecessary — see "What is
left" below.

## Correctness analysis

- **Functional equivalence:** `createEnvForOpts(base, namespaceType,
  requestType, opts)` is a pure function of its inputs. Memoising it
  with `sync.Once` cannot change observed behaviour — every caller
  receives the same `*environment.EnvSet` it would have received
  pre-patch.
- **Error semantics:** if `createEnvForOpts` returns an error for a
  given `opts`, that error is cached and returned to every subsequent
  caller for the same `opts`, exactly as it was pre-patch (where
  `mustBuildEnvs` would `panic` and the panic would surface at
  `NewCompiler` time).
  - Eager fail-fast → lazy fail-on-first-use is a **timing change**,
    not a behaviour change. The expression-compile path (which is the
    only consumer of the returned env) treats env-load failure as a
    compile error, identical to today.
- **Race safety:** `sync.Map.LoadOrStore` is the standard
  promote-on-first-write idiom; `sync.Once` guarantees `createEnvForOpts`
  runs at most once per key, with happens-before ordering from the
  initialiser to all subsequent observers. Vetted by `go test -race`
  via the existing test suite.
- **Hoisted DeclTypes:** `BuildRequestType` / `BuildNamespaceType`
  return freshly-allocated `*DeclType` trees on every call. Their
  outputs are mutated by no caller (they only flow into `cel.Variable`
  / `DeclTypes` registration). Safe to share.

## Scope of test pass

```
go test ./staging/src/k8s.io/apiserver/pkg/admission/plugin/cel/...           PASS  (0.99 s)
go test ./staging/src/k8s.io/apiserver/pkg/admission/plugin/policy/...        PASS  (~1 min)
go test ./staging/src/k8s.io/apiserver/pkg/cel/...                            PASS  (~1 min)
```

Including the existing `validating/admission_test.go`,
`mutating/compilation_test.go`, the cel/composition / condition /
compile_cache test suites, and the typechecking path that runs through
`NewCompositedCompilerForTypeChecking`.

## What is left (where the residual 42.6 KiB / policy goes)

The post-patch breakdown of the per-policy retained 42.6 KiB:

| Source | Approx KiB | Addressed by |
|---|---:|---|
| 1 unconditional variables-overlay env clone in `NewCompositedCompiler` | ~25 | **0006** |
| 1 lazy env clone (the first `OptionalVariableDeclarations` key requested) | ~8 | 0005 (process-wide pool) |
| Per-policy `*DeclType` for `kubernetes.variables` + empty Fields map | ~3 | **0006** |
| `compositionState`, `*compiler`, `*conditionCompiler`, `*mutatingCompiler`, `*CompositedCompiler` | ~2 | n/a (intrinsic) |
| 4 × `*CompositedConditionEvaluator` + 4 × `*condition` (even when filters are empty) | ~3 | **0010 follow-up** (see below) |
| `*validator` struct + spec back-pointer | ~1 | n/a |

So this patch closes ~24 % of the floor; **0006** would close another
~30 %, and a small follow-up to elide empty `CompileCondition` calls
(replace with a process-global no-op `ConditionEvaluator` singleton)
would close another ~7 %. Stacked, the per-policy retained floor
could plausibly drop from 56 → ~25 KiB — an additional ~150 MB saved
at the 10 000-policy mark.

## What this patch does *not* claim

- Does not touch any CEL evaluator residency. Once a policy has at
  least one expression, the `cel.Program` retained per expression is
  unchanged.
- Does not change the asymptotic scaling — per-policy cost is still
  `O(1)` constant. Only the constant is reduced.
- Does not address the architectural redesign discussed in
  [`../../analysis/architecture-redesign.md`](../../analysis/architecture-redesign.md).
  That redesign breaks the asymptote; this patch shrinks the constant.

## Apply

```
git apply patches/0001-program-cache/0001-cel-add-process-wide-compiled-program-cache.patch
git apply patches/0010-lazy-varenvs/0010-cel-lazify-per-compiler-varenvs.patch
```

Do **not** also apply patch 0004 or 0005 — see the mutual-exclusion note above.
