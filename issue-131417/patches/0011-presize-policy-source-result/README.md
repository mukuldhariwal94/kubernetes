# 0011 — Pre-size `calculatePolicyData` result slice to unique-policy count

**Status:** prototype, applied to local working tree.
**Side:** k8s (`staging/src/k8s.io/apiserver/pkg/admission/plugin/policy/generic/policy_source.go`).
**Headline impact:** **~790 KiB allocated and ~32 % wall-clock saved per
refresh** in multi-tenant / many-bindings-per-policy clusters. Zero
impact in single-binding-per-policy clusters. One-line fix.

## Why I went looking here

The user asked: "is there minimum overhead in the generic
`policy_source.go` that can be optimised further?" My initial
hypothesis was reflective `meta.Accessor(policySpec)` in
`compilePolicyLocked` ([policy_source.go:471](../../../staging/src/k8s.io/apiserver/pkg/admission/plugin/policy/generic/policy_source.go#L471))
since it runs once per policy on every refresh tick. **That hypothesis
was wrong** — see [Findings § 1](#findings) below. The actual
hot-spot was elsewhere.

## Findings

A focused micro-benchmark
(`policy_source_bench_test.go`) measured each candidate hot spot.

### 1. `meta.Accessor` is *not* a per-policy bottleneck

```
BenchmarkMetaAccessorOnly-8                40 ns/op  0 B/op  0 allocs/op
BenchmarkCompilePolicyLocked_CacheHit-8   ~150 ns/op  0 B/op  0 allocs/op
```

Typed `*v1.ValidatingAdmissionPolicy` satisfies `metav1.Object`
directly (via embedded `ObjectMeta`), so `meta.Accessor` returns the
embedded value with zero reflection allocation. The cache-hit branch
in `compilePolicyLocked` is essentially free. **Initial hypothesis
discarded.**

### 2. The accessor wrappers are also not a bottleneck

`s.newBindingAccessor(b)` and `s.newPolicyAccessor(p)` look like
per-call heap allocations, but escape analysis stack-allocates them
when the wrapper doesn't escape (production callsites use the
accessor briefly for `GetPolicyName()` / `GetParamKind()` then
discard). Bench:

```
BenchmarkBindingAccessorAlloc-8   ~0.5 ns/op  0 B/op  0 allocs/op
```

### 3. The actual per-refresh cost is in the result-slice over-sizing

A simulated `calculatePolicyData` body at 100 policies × 100 bindings
each (10 000 bindings, 100 unique policies):

```
BenchmarkRefreshSimulated_100x100-8           1 035 113 B/op  410 µs/op
BenchmarkRefreshSimulated_100x100_PreSized-8    240 488 B/op  278 µs/op
```

Difference = **~795 KiB / refresh, −32 % wall-clock**. The cause is
[policy_source.go:304](../../../staging/src/k8s.io/apiserver/pkg/admission/plugin/policy/generic/policy_source.go#L304):

```go
result := make([]PolicyHook[P, B, E], 0, len(bindingList))
```

`len(bindingList)` is the binding count, but the slice will only
ever grow to `len(policiesToBindings)` (the *unique* policy count).
At 100:1 binding-to-policy ratio that's a 100× over-allocation. With
`sizeof(PolicyHook) ≈ 80 bytes`, ~800 KiB per refresh of dead
backing array.

Refresh runs every 1 s when the dirty bit is set (i.e. on every
informer event). So this is **steady-state churn**, not a one-time
cost.

### 4. The 1-binding-per-policy case is unaffected

```
BenchmarkRefreshSimulated_10k_NoChange-8   ~3 MB/op  10 080 allocs/op
```

When `len(bindingList) == len(policiesToBindings)` the sizing is
already correct and the patch is a no-op. The bulk of the 3 MB at
10 k bindings is the per-policy single-element `[]B` slices and the
PolicyHook entries themselves — not the result-slice header.

## The fix

```diff
-       result := make([]PolicyHook[P, B, E], 0, len(bindingList))
+       // Capacity is the unique-policy count, not the binding count. In
+       // multi-tenant clusters where many bindings reference the same
+       // policy, len(bindingList) over-allocates by O(bindings - policies),
+       // which at 100 bindings/policy × 100 policies wastes ~800 KB per
+       // refresh. len(policiesToBindings) is the exact upper bound.
+       result := make([]PolicyHook[P, B, E], 0, len(policiesToBindings))
```

`len(policiesToBindings)` is the exact upper bound on the loop body:
the loop iterates over `policiesToBindings` and may skip entries
(deleted policies) but cannot exceed it. Strictly safe.

## Reproduction

```bash
go test -run='^$' \
  -bench='BenchmarkRefreshSimulated_100x100|BenchmarkBindingAccessorAlloc|BenchmarkMetaAccessorOnly|BenchmarkCompilePolicyLocked' \
  -benchmem -count=3 \
  ./staging/src/k8s.io/apiserver/pkg/admission/plugin/policy/generic/...
```

The bench file `policy_source_bench_test.go` is included in this
patch and constructs a minimal `*policySource` with a stubbed
compiler/accessor — no real informer machinery, so the numbers
above isolate exactly the framework cost.

## Architectural tweaks worth considering (not in this patch)

The pre-size fix closes the obvious low-hanging branch. The remaining
~3 MB/refresh allocation at 10 k bindings comes from:

1. **The fresh `policiesToBindings` map every refresh.**
   `clear()`-and-reuse on a single map field of `policySource`
   (the lock is held during refresh, so it's safe single-threaded)
   would save ~1 map header + bucket allocation per refresh. Tiny
   absolute saving but eliminates the dominant non-PolicyHook alloc
   site.

2. **The fresh per-policy `[]B` slices in `policiesToBindings`.**
   Each unique-policy key gets a freshly grown `append`-backed slice.
   At 10 k policies that's 10 k slice header / backing-array pairs per
   refresh, which is 95 % of the `allocs/op` count in the bench.
   Difficult to eliminate without restructuring (the slices end up in
   the published `PolicyHook.Bindings` field, so they cannot be
   reused across refreshes safely).

3. **Granular dirty tracking — the architectural one.** Today
   [policy_source.go:67](../../../staging/src/k8s.io/apiserver/pkg/admission/plugin/policy/generic/policy_source.go#L67)
   is a single `atomic.Bool`. Every informer event (Add/Update/Delete
   on any policy or binding, including a no-op annotation update)
   sets it, triggering a full N-policy rebuild within 1 s. At 10 k
   policies in a churning cluster (operators applying labels,
   periodic reconcilers, etc.) this is one full ~3 MB rebuild per
   informer batch.

   A natural next step is to replace the `atomic.Bool` with a
   `sync.Map[types.NamespacedName]struct{}` (or a small slice
   guarded by a mutex) tracking *which* policies / bindings changed.
   Then `refreshPolicies` can:

   - Drain the dirty set
   - For unchanged policies: copy the prior `PolicyHook` entry
     unchanged (Bindings slice, Evaluator, ParamInformer, etc.)
   - For changed policy keys: re-read the binding cache for that
     key only, recompute its `PolicyHook`, splice into the new
     slice
   - `atomic.Store` the new slice

   The new slice is still O(N) in size (downstream consumers expect
   a flat list), but the per-refresh work becomes **O(K_changed)**
   instead of O(N). For typical low-churn steady-state (K_changed = 0
   between events; small after a single binding update) this collapses
   the per-refresh cost from milliseconds + megabytes to microseconds
   + zero allocation.

   This is a self-contained refactor of `calculatePolicyData` —
   no API change to `Source[T]` consumers. Worth a separate KEP-class
   patch; not bundled here so the obvious sizing fix can land
   independently.

4. **The 1-second `wait.Until` polling interval
   ([policy_source.go:47](../../../staging/src/k8s.io/apiserver/pkg/admission/plugin/policy/generic/policy_source.go#L47)).**
   This bounds policy-change latency at 1 s; reducing it cuts
   latency at proportional CPU cost. Combined with #3 (incremental
   refresh), the polling interval could safely drop to 100 ms or
   below since each tick would do near-zero work in steady state.

## What this patch does *not* claim

- Does not solve the per-policy CEL retention discussed in the rest of
  the patch series. CEL programs are unaffected.
- Does not change the asymptote — refresh is still O(N) per tick. The
  constant is lower, that's all. (#3 above is what would change the
  asymptote.)
- Does not change observable behaviour in any way: same slice contents,
  same atomic-pointer publication, same lock semantics.

## Apply

```
git apply patches/0011-presize-policy-source-result/0011-policy-generic-presize-refresh-result-slice.patch
```
