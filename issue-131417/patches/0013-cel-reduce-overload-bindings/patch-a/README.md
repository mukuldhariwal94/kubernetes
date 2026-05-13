# Patch A — usedOIDs set + `fnOverlapsOIDs` helper

## Approach

Build a `usedOIDs` set from the type-checked AST's `ReferenceMap`, then
filter `e.functions` to only those whose overload IDs intersect that set.
Two explicit branches: optimised path for checked ASTs, original full loop
as a verbatim fallback for unchecked ASTs.

```
refMap := a.ReferenceMap()
if a.IsChecked() && len(refMap) != 0:

    usedOIDs ← flatten refMap[*].OverloadIDs      // O(|refMap| × avgOIDs)

    for fn in e.functions:                         // O(|e.functions|)
        if fnOverlapsOIDs(fn, usedOIDs):           //   O(|usedOIDs|) × O(1)
            fn.Bindings() + disp.Add()

else:
    // original loop — unchanged
    for fn in e.functions: fn.Bindings() + disp.Add()
```

`fnOverlapsOIDs` (added to `program.go`) iterates `usedOIDs` and calls
`fn.HasOverloadID(oID)` per entry.

`HasOverloadID` (added to `decls.go`) does a direct O(1) lookup into the
internal `FunctionDecl.overloads map[string]*OverloadDecl`, avoiding the
`[]*OverloadDecl` slice allocation that `OverloadDecls()` performs each call.

## Files changed

| File | Change |
|---|---|
| `vendor/github.com/google/cel-go/cel/program.go` | Branch checked/unchecked; add `fnOverlapsOIDs` helper |
| `vendor/github.com/google/cel-go/common/decls/decls.go` | Add `HasOverloadID(id string) bool` to `FunctionDecl` |

## Time complexity

Let:
- **F** = `len(e.functions)` ≈ 157
- **U** = `len(usedOIDs)` ≈ 5–20 (unique OIDs referenced by the expression)
- **R** = `len(refMap)` ≈ 8–30 (call nodes in the AST)
- **O** = total overloads per function ≈ 2 avg

| Step | Cost | Notes |
|---|---|---|
| Flatten `refMap` → `usedOIDs` | O(R × O) ≈ O(20) | one pass over refMap entries |
| `fnOverlapsOIDs` per function | O(U) × O(1) | iterate usedOIDs, O(1) map lookup each |
| Total — optimised path | **O(F × U)** ≈ O(3 140) | ~157 × 20 checks |
| Total — fallback path | O(F) = O(157) | unchanged from upstream |

## Space complexity

| Structure | Per-program allocation | Notes |
|---|---|---|
| `usedOIDs` map | O(U) ≈ 5–20 string keys | freed after `newProgram` returns |
| `Dispatcher` entries added | O(B × 2) ≈ O(10–30) | B = bound functions ≈ 4–15 |
| `*functions.Overload` structs | O(B × 2) | only needed functions |
| **Saved vs baseline** | **~(F−B) × avgBindings** ≈ **~330 structs** | ~64–128 B each |

### Baseline (upstream)

| Structure | Size |
|---|---|
| `Dispatcher` entries | ~345 per program |
| `*functions.Overload` structs | ~345 per program |

### After patch (typical VAP expression, 5 functions used)

| Structure | Size |
|---|---|
| `Dispatcher` entries | ~10–15 |
| `*functions.Overload` structs | ~10–15 |
| Wasted allocations | 0 |

At **1 000 compiled programs**: ~330 000 fewer `*functions.Overload`
allocations → **~21–42 MB heap saved** (at 64–128 B per struct).

## Safety

| Case | Behaviour |
|---|---|
| Unchecked AST | `a.IsChecked()` false → original full loop |
| Checked AST, empty refMap | `len(refMap) == 0` → original full loop |
| Operator bypassed by planner (PATH-3) | No OID in refMap → function safely skipped; planner does not consult dispatcher for these |
| Multi-OID call node | All OIDs flattened into `usedOIDs`; owning function included if any matches |

## Apply

```bash
git apply 0013-cel-reduce-overload-bindings/patch-a/0013-celgo-reduce-overload-bindings.patch
```
