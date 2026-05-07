# 0008 — Shrink process-wide compile-cache cap to 1500

**Status:** drafted, applied to local branch (commit `1c91ba1`).
**Side:** k8s (`staging/src/k8s.io/apiserver/pkg/admission/plugin/cel`).
**Headline impact:** caps cache footprint at ~300–400 MB (was ~1 GB).

## Problem

[`compile_cache.go`](../../../staging/src/k8s.io/apiserver/pkg/admission/plugin/cel/compile_cache.go)
on the `compiler-cache-fix` branch sized its LRU at 5 000 entries against a
~30–200 KB per-program estimate. Empirical heap profiles from a real
apiserver (snapshot at 1.7 GB inuse, see
[`../../analysis/celgo-memory-api-audit.md`](../../analysis/celgo-memory-api-audit.md))
put the retained cost of a single `cel.Program` at **~150–250 KB** once
you account for the per-program `FunctionDecl.Bindings` copy and the
interpreter dispatcher table populated from it. At 5 000 entries the
cache could grow to ~1 GB on its own.

## What it does

One-line change: `defaultCompileCacheSize = 5000 → 1500`. Templated
workloads (the case that benefits most from program-level dedup) only
need a few hundred distinct keys, so the hit rate is preserved.

```diff
-// 30-200 KB), so 5000 entries cap the cache footprint at a few hundred MB
-// even on pathological clusters.
-const defaultCompileCacheSize = 5000
+// retained cost of a single cel.Program at ~150-250 KB (the per-program
+// FunctionDecl.Bindings + interpreter Dispatcher copy dominate, see
+// issue-131417). 1500 entries caps the cache footprint at ~300-400 MB on
+// clusters with thousands of distinct expressions while preserving near-
+// optimal hit rates on templated workloads.
+const defaultCompileCacheSize = 1500
```

## Pairs with

**0009.** That patch eliminates the bulk of the per-entry retained cost
by sharing the interpreter Dispatcher across programs of an env. After
0009 lands the cap can be revisited upward (3 000–5 000 again would be
viable since each entry would retain ~30–50 KB instead of ~200 KB).

## Apply

```
git apply patches/0008-shrink-cache-cap/0008-cel-shrink-process-wide-compile-cache-cap-to-1500.patch
```

## Tests

Existing `TestCompileCache_BoundedSize` covers the cap behaviour; no new
tests required.
