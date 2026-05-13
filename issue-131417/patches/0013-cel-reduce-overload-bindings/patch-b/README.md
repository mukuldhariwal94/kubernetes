# Patch B — OID→FunctionDecl index cached on `Env`

## Approach

Build a reverse index (overload ID → `FunctionDecl`) once per `Env` via
`sync.Once` and store it on the `Env` struct. Each `newProgram` call retrieves
the cached index in O(1) and walks the AST's `refMap` directly — one O(1)
lookup per overload ID, no iteration over `e.functions` at all.

```
// env.go — built once per Env, shared across all programs
overloadIndex():
    oidToFnOnce.Do:
        for fn in e.functions:          // O(|e.functions|) = O(157)
            for o in fn.OverloadDecls():// O(total overloads) = O(345)
                oidToFn[o.ID()] = fn
    return oidToFn                      // O(1) thereafter

// program.go — per newProgram call
refMap := a.ReferenceMap()
if a.IsChecked() && len(refMap) != 0:

    oidIndex := e.overloadIndex()       // O(1) — cached map pointer
    seen     := map[*FunctionDecl]{}    // dedup by pointer

    for ref in refMap:                  // O(|refMap|) ≈ O(8–30)
        for oID in ref.OverloadIDs:     // O(avgOIDs) ≈ O(1–3)
            fn := oidIndex[oID]         // O(1) map lookup
            if fn not in seen:
                seen[fn] = {}
                fn.Bindings() + disp.Add()

else:
    // original loop — unchanged
    for fn in e.functions: fn.Bindings() + disp.Add()
```

The key difference from Patch A: the `e.functions` loop is **eliminated from
the hot path entirely**. Instead of iterating all 157 functions per program,
`newProgram` only touches the small number of call nodes present in the
expression (~8–30 `refMap` entries with ~1–3 OIDs each).

## Files changed

| File | Change |
|---|---|
| `vendor/github.com/google/cel-go/cel/env.go` | Add `oidToFnOnce sync.Once`, `oidToFn map[string]*FunctionDecl` fields + `overloadIndex()` method |
| `vendor/github.com/google/cel-go/cel/program.go` | Branch checked/unchecked; walk `refMap` OIDs through `oidIndex` |

No changes to `decls.go` or `checker.go`.

## Time complexity

Let:
- **F** = `len(e.functions)` ≈ 157
- **V** = total declared overloads ≈ 345 (= index build cost, paid once per Env)
- **R** = `len(refMap)` ≈ 8–30 (call nodes in the AST)
- **O** = avg OIDs per call node ≈ 1–3
- **B** = bound functions per expression ≈ 4–15

| Operation | Cost | Frequency |
|---|---|---|
| Build `oidToFn` index | O(V) = O(345) | Once per `Env` via `sync.Once` |
| Retrieve index | O(1) | Per `newProgram` call |
| Walk `refMap` + OID lookups | O(R × O) ≈ O(20–90) | Per `newProgram` call |
| `Bindings()` + `disp.Add()` | O(B) ≈ O(4–15) | Per `newProgram` call |
| **Total — optimised path** | **O(R × O)** ≈ **O(20–90)** | Per program |
| **Total — fallback path** | O(F) = O(157) | Per program (unchecked AST) |

Compared to Patch A (O(F × U) ≈ O(3 140) per program), Patch B reduces the
per-program hot path by ~35–150×. The index build cost O(V) is amortised
across all programs compiled from the same `Env` — in practice thousands.

## Space complexity

| Structure | Location | Size | Lifetime |
|---|---|---|---|
| `oidToFn` index | `Env` struct | O(V) ≈ 345 entries × 16 B ≈ **5.5 KB** | Env lifetime |
| `seen` map | per `newProgram` | O(B) ≈ 4–15 pointer entries | freed after call |
| Dispatcher entries added | per `newProgram` | O(B × avgBindings) ≈ O(10–30) entries | program lifetime |
| `*functions.Overload` structs saved | per program | ~(V − B×2) ≈ **~330 structs** | not allocated |

### Baseline (upstream) vs after patch

| Metric | Before | After |
|---|---|---|
| Per-program `Bindings()` calls | 157 | ~4–15 |
| Per-program dispatcher entries | ~345 | ~10–30 |
| Per-program `*functions.Overload` allocs | ~345 | ~10–30 |
| Env overhead | 0 | ~5.5 KB (once, shared) |

At **1 000 compiled programs**: ~330 000 fewer `*functions.Overload`
allocations → **~21–42 MB heap saved** per 1 000 programs, at the cost of
one 5.5 KB index per `Env`.

## Safety

| Case | Behaviour |
|---|---|
| Unchecked AST | `a.IsChecked()` false → original full loop |
| Checked AST, empty refMap | `len(refMap) == 0` → original full loop |
| Env extension | New `Env` gets zero-value `sync.Once`; index rebuilt on first use |
| Concurrent `newProgram` | `sync.Once` guarantees single build, safe concurrent reads |
| Multi-OID call node | All OIDs walked; `seen` deduplicates the owning function |
| Planner PATH-3 bypass | Those operators produce no `OverloadIDs` in refMap → not in `usedOIDs` → safely skipped |

## Apply

```bash
git apply 0013-cel-reduce-overload-bindings/patch-b/0013-celgo-reduce-overload-bindings-patch-b.patch
```
