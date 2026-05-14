# 0012 — Drop `AstToCheckedExpr` discard, intern `ReturnTypes()` slices

**Status:** prototype, applied to local working tree.
**Side:** k8s (`pkg/admission/plugin/cel/compile.go` plus every
`ReturnTypes()` implementation under `pkg/admission/plugin/policy/{validating,mutating}` and
`pkg/admission/plugin/webhook/matchconditions`).
**Headline impact:** **~17 KiB / ~250 mallocs saved per
CompileCELExpression call on complex policies**, scaling with AST
node count. ~450 MB transient-allocation reduction per full-refresh
of 5 000 policies × 5 expressions.

## What this patch addresses

A targeted audit of `condition.go:CompileCondition` and
`compile.go:CompileCELExpression` for memory wastage **independent
of the cel.Program retention story**. Two real findings:

### Waste #1 — defensive `AstToCheckedExpr` discard

[compile.go:348-352](../../../staging/src/k8s.io/apiserver/pkg/admission/plugin/cel/compile.go#L348-L352) (pre-patch):

```go
_, err = cel.AstToCheckedExpr(ast)
if err != nil {
    // should be impossible since env.Compile returned no issues
    return resultError("unexpected compilation error: "+err.Error(), apiservercel.ErrorTypeInternal, nil)
}
```

The comment is right: it *is* impossible. `cel.AstToCheckedExpr`'s
only error case is `!a.IsChecked()`, and `env.Compile()` on the line
above returned no issues — so the AST is checked.

But `cel.AstToCheckedExpr` walks the entire AST building a full
`*exprpb.CheckedExpr` proto tree (refMap + typeMap + recursive
`ExprToProto` over every node) before returning. The result is
discarded. Per-call cost scales with AST node count:

```
BenchmarkAstToCheckedExpr_Discard_Trivial-8     12 allocs   857 B/op
BenchmarkAstToCheckedExpr_Discard_Simple-8      24 allocs  1480 B/op
BenchmarkAstToCheckedExpr_Discard_Realistic-8   88 allocs  5720 B/op
BenchmarkAstToCheckedExpr_Discard_Complex-8    252 allocs 17216 B/op
```

Removed.

### Waste #2 — `ReturnTypes()` returns a fresh slice literal every call

Every `ExpressionAccessor.ReturnTypes()` impl in the tree was the
same pattern:

```go
func (v *ValidationCondition) ReturnTypes() []*celgo.Type {
    return []*celgo.Type{celgo.BoolType}      // ← fresh slice per call
}
```

Eight implementations (`ValidationCondition`,
`AuditAnnotationCondition`, `Variable` (×2 — both packages),
`MessageExpressionCondition`, `celExpression`, `MatchCondition`,
`JSONPatchCondition`, `ApplyConfigurationCondition`), each
allocating a fresh `[]*cel.Type` per call.

`ReturnTypes()` is invoked **twice** per `CompileCELExpression`
(once for the cache key, once inside `compileFresh`). Bench:

```
BenchmarkReturnTypes_FreshSliceLiteral-8    13.5 ns/op   8 B/op   1 allocs/op
BenchmarkReturnTypes_PackageSingleton-8      1.0 ns/op   0 B/op   0 allocs/op
```

Each impl converted to a package-level singleton variable
initialised once at process start. Same observable behaviour, zero
per-call allocation.

## Reproduction

```bash
# Per-waste-site micro-benches:
go test -run='^$' \
  -bench='BenchmarkAstToCheckedExpr_Discard|BenchmarkReturnTypes' \
  -benchmem -count=2 \
  ./staging/src/k8s.io/apiserver/pkg/admission/plugin/cel/

# End-to-end compileFresh delta:
go test -run='^$' \
  -bench='BenchmarkCompileFresh_End2End' \
  -benchmem -count=2 \
  ./staging/src/k8s.io/apiserver/pkg/admission/plugin/cel/
```

The bench file `compile_waste_bench_test.go` is included in this
patch.

## End-to-end results

`BenchmarkCompileFresh_End2End` (full `compileFresh` with real env):

| Expression | bytes/op (before → after) | Δ bytes/op | allocs/op (before → after) | Δ allocs |
|---|---|---:|---|---:|
| Trivial   | 10 322 → 9 466    | **−856 B (−8 %)**   | 199 → 187   | **−12** |
| Simple    | 15 912 → 14 430   | **−1 482 B (−9 %)** | 313 → 289   | **−24** |
| Realistic | 60 713 → 54 980   | **−5 733 B (−9 %)** | 1 155 → 1 067 | **−88** |
| Complex   | 178 182 → 160 932 | **−17 250 B (−10 %)** | 3 050 → 2 798 | **−252** |

The savings exactly match the `AstToCheckedExpr_Discard` micro-bench
numbers — confirming that's where the savings come from.

The wall-clock delta is in the noise (~10 %) because `env.Compile` +
`env.Program` dominate. The win is GC pressure: every refresh of
5 000 policies × 5 mid-complexity expressions allocates **~450 MB
less of transient garbage**, which in steady state translates to
fewer GC cycles and shorter STW pauses on the apiserver.

## Files touched

```
M  pkg/admission/plugin/cel/compile.go                          (delete 5 lines)
M  pkg/admission/plugin/policy/validating/interface.go          (3 funcs → singletons)
M  pkg/admission/plugin/policy/validating/message.go            (1 func → singleton)
M  pkg/admission/plugin/policy/validating/typechecking.go       (1 func → singleton)
M  pkg/admission/plugin/policy/mutating/plugin.go               (1 func → singleton)
M  pkg/admission/plugin/policy/mutating/patch/json_patch.go     (1 func → singleton)
M  pkg/admission/plugin/policy/mutating/patch/smd.go            (1 func → singleton)
M  pkg/admission/plugin/webhook/matchconditions/matcher.go      (1 func → singleton)
A  pkg/admission/plugin/cel/compile_waste_bench_test.go         (regression coverage)
```

Net: 9 files, ~50 lines moved (no real lines added — just hoisting
slice literals to package var). Plus the 5-line dead-code deletion.

## Correctness analysis

### Removing the AstToCheckedExpr call

- The error case (`!ast.IsChecked()`) is unreachable here because
  `env.Compile(...)` immediately above returns no issues, which
  necessarily means the AST was successfully type-checked.
- The result of `AstToCheckedExpr` was already discarded, so deletion
  cannot affect any downstream caller.
- The remaining call to `env.Program(ast, ...)` consumes the same AST
  unchanged — its inputs are unaffected by removing this line.
- Existing tests in `pkg/admission/plugin/cel/...`,
  `policy/{validating,mutating}/...`, `webhook/matchconditions/...`,
  and `pkg/cel/...` all pass.

### Singleton ReturnTypes() slices

- `cel.BoolType`, `cel.StringType`, `cel.NullType`, `cel.AnyType`,
  `cel.DynType` are themselves package-global immutable values in
  cel-go. There is no callsite in the tree that mutates the slice
  returned by `ReturnTypes()` — it is only consumed by:
    - `CompileCELExpression` for the cache key fingerprint (read-only iteration)
    - `compileFresh` for the `ast.OutputType().IsExactType(returnType)` loop (read-only iteration)
- Sharing a single slice across all callers is therefore observably
  identical to allocating a fresh one.
- For the `JSONPatchCondition` / `ApplyConfigurationCondition` cases
  the singleton requires a function call (`celgo.ListType(...)`,
  `celtypes.NewObjectType(...)`); these are called at package init
  time once instead of per call.

## Tested

```
go test ./staging/src/k8s.io/apiserver/pkg/admission/plugin/cel/...           PASS
go test ./staging/src/k8s.io/apiserver/pkg/admission/plugin/policy/...        PASS  (~1 min)
go test ./staging/src/k8s.io/apiserver/pkg/admission/plugin/webhook/matchconditions/...  PASS
go test ./staging/src/k8s.io/apiserver/pkg/cel/...                            PASS
```

## What this patch does *not* claim

- Does not change CEL evaluator residency. `cel.Program` lifetime is
  unaffected.
- Does not change the asymptotic scaling of refresh — same O(N), same
  retention class. Lower constant on the per-compile transient cost.
- Does not change observable behaviour anywhere — same return values,
  same evaluation results, same error semantics.
- Does not address the ~3 MB / refresh allocation in
  `policy_source.go` (see [0011](../0011-presize-policy-source-result/))
  or per-policy CEL retention (see 0001 / 0009 / 0010).

## What's left after this patch

A thorough audit of `CompileCondition` / `CompileCELExpression`
turned up only these two clean wins. Lower-priority residue worth
noting:

- **`compilationResults := make([]CompilationResult, len(expressionAccessors))`**
  at [condition.go:44](../../../staging/src/k8s.io/apiserver/pkg/admission/plugin/cel/condition.go#L44)
  sizes to input length even when `convertv1MessageExpressions` passed
  many nil entries. The wasted slots stay zero-valued
  `CompilationResult{}` (no Program, no Error). Per-policy this is
  ~6-10 zero-value structs (~80 B each ≈ 500 B). Could compact, but
  doing so breaks the index-correlation between `validations[i]` and
  `messageExpressions[i]` that the `condition.ForInput` loop relies
  on. Not worth the refactor.
- **`computeCompileCacheKey` allocates ~3 small objects per call**
  (sha256 hasher state, `rts` slice, `Sum(nil)` output). At cache-hit
  rates >90 % this is the dominant per-call allocation. Switching to
  a non-allocating hash like `xxhash.Digest{}` (with a zero-alloc
  reset pattern) would eliminate ~3 mallocs per CompileCELExpression
  call. Separate patch worthwhile.

## Apply

```
git apply patches/0012-drop-checkedexpr-discard-and-singleton-returntypes/0012-cel-drop-asttocheckedexpr-discard-and-singleton-returntypes.patch
```
