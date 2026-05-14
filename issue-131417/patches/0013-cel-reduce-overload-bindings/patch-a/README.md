# Patch A — inline `usedOIDs` set + `OverlapsOIDs` method on `FunctionDecl`

## Approach

Build a `usedOIDs map[string]struct{}` inside `newProgram` from the AST's
`ReferenceMap`, then iterate `e.functions` and skip any function whose
declared overloads do not intersect the used set. A new `OverlapsOIDs` method
on `FunctionDecl` performs the intersection check by iterating the function's
own (small, constant) overload set and probing the (larger) `usedOIDs` map.

```
newProgram (checked path):

  // Step 1 — flatten refMap into a lookup set
  usedOIDs := map[string]struct{}{}
  for ref in a.ReferenceMap():         O(R) ≈ O(8–30)
    for oID in ref.OverloadIDs:        O(avgOIDs) ≈ O(1–3)
      usedOIDs[oID] = {}

  // Step 2 — filter e.functions
  for fn in e.functions:               O(F) = O(157)
    if !fn.OverlapsOIDs(usedOIDs):     O(|fn.overloads|) ≈ O(1–4)  ← iterate the smaller set
      continue
    fn.Bindings() + disp.Add()         only for matched fns (~4–15)
```

### Why iterate `fn.overloads`, not `usedOIDs`

Set intersection is cheapest when you iterate the **smaller** side and probe
the **larger** side:

| Direction | Outer loop | Inner probe | Per-function cost |
|---|---|---|---|
| Previous (wrong) | `usedOIDs` — up to ~20 entries | `fn.HasOverloadID` — O(1) | O(\|usedOIDs\|) ≈ O(20) |
| **Current (correct)** | `fn.overloads` — **1–4 constant entries** | `usedOIDs[id]` — O(1) | **O(\|fn.overloads\|) ≈ O(1–4)** |

`fn.overloads` is a constant, per-function property (it never changes after
`Env` construction). `usedOIDs` grows with expression complexity and is
always ≥ 1 entry. Iterating `fn.overloads` guarantees the shortest inner
loop and the earliest possible short-circuit.

### `OverlapsOIDs` vs the old `fnOverlapsOIDs` helper

The old package-level helper in `program.go` looped over `usedOIDs` and called
`fn.HasOverloadID()` for each entry (wrong side). The new method lives on
`FunctionDecl` in `decls.go`, iterates the internal `f.overloads` map
directly (no slice allocation, no intermediate copy), and probes `usedOIDs`
once per declared overload:

```go
// common/decls/decls.go
func (f *FunctionDecl) OverlapsOIDs(set map[string]struct{}) bool {
    for id := range f.overloads {   // 1–4 iterations max
        if _, ok := set[id]; ok {
            return true
        }
    }
    return false
}
```

## Files changed

| File | Change |
|---|---|
| `vendor/github.com/google/cel-go/common/decls/decls.go` | Add `OverlapsOIDs(set map[string]struct{}) bool` to `FunctionDecl` |
| `vendor/github.com/google/cel-go/cel/program.go` | Branch checked/unchecked; call `fn.OverlapsOIDs(usedOIDs)`; remove old `fnOverlapsOIDs` helper and unused `decls` import |

`HasOverloadID` remains for point lookups elsewhere; `OverlapsOIDs` is the
set-intersection variant.

## Time complexity

Let:
- **F** = `len(e.functions)` ≈ 157
- **R** = `len(refMap)` ≈ 8–30
- **O** = avg OIDs per call node ≈ 1–3  → `|usedOIDs|` ≈ U ≈ 5–20
- **K** = avg overloads per function ≈ **1–4** (constant)
- **B** = functions matched ≈ 4–15

| Operation | Cost |
|---|---|
| Build `usedOIDs` | O(R × O) ≈ O(20) |
| Outer loop over `e.functions` | O(F) = O(157) |
| Per-function intersection check | **O(K) ≈ O(1–4)** |
| `Bindings()` + `disp.Add()` (matched only) | O(B) ≈ O(4–15) |
| **Total — optimised path** | **O(R×O) + O(F×K)** ≈ **O(20 + 630)** ≈ **O(650)** |
| Previous total (wrong direction) | O(R×O) + O(F×U) ≈ O(20 + 3140) ≈ O(3160) |
| **Total — fallback path** | O(F) = O(157) — unchanged |

**~5× improvement** in the filtering step vs the previous direction.

## Space complexity

| Structure | Size | Lifetime |
|---|---|---|
| `usedOIDs` map | O(U) ≈ 5–20 entries × 16 B | freed after `newProgram` returns |
| Dispatcher entries added | O(B × avgBindings) ≈ O(10–30) entries | program lifetime |
| `*functions.Overload` structs saved | ~(V − B×2) ≈ ~330 structs | never allocated |

No env-level state is added. All per-program allocations are freed immediately
after `newProgram` returns.

## Safety

| Case | Behaviour |
|---|---|
| Unchecked AST | `a.IsChecked()` false → original full loop |
| Checked AST, empty refMap | `len(refMap) == 0` → original full loop |
| Function with 0 overloads | `OverlapsOIDs` loop body never executes → returns false → skipped |
| Env extensions | No state on `Env`; each `newProgram` builds its own `usedOIDs` |
| Planner PATH-3 bypass operators | `fn.Bindings()` always adds the fn-name fallback key; correct dispatch guaranteed |

## Apply

```bash
git apply 0013-cel-reduce-overload-bindings/patch-a/0013-celgo-reduce-overload-bindings.patch
```
