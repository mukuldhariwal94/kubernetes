# Patch C — checker populates `ReferenceInfo.Name`; `newProgram` uses direct lookup

## Approach

Modify the CEL type-checker to write `fn.Name()` into `ReferenceInfo.Name`
whenever it resolves a function call. `newProgram` then reads `ref.Name`
directly from the AST's `refMap` and performs an O(1) lookup into
`e.functions` — no reverse index to build, no iteration over `e.functions`,
no per-program map beyond a small `seen` dedup set.

```
checker.go — at every function-resolution site:
  checkedRef = ast.NewFunctionReference(overload.ID())
  checkedRef.Name = fn.Name()          // ← new: record function name in AST

program.go (checked path):
  seen := map[string]struct{}{}         // dedup by function name

  for ref in a.ReferenceMap():          // O(R) ≈ O(8–30)
    fnName := ref.Name                  // O(1) — already in AST
    if fnName == "" || len(ref.OverloadIDs) == 0:
        continue                        // ident / constant node, not a fn call
    if fnName in seen: continue
    seen[fnName] = {}
    fn := e.functions[fnName]           // O(1) — direct map lookup
    fn.Bindings() + disp.Add()

  else (unchecked / empty refMap):
    original full loop — unchanged
```

The key difference from Patches A and B: the function name is now carried in
the AST itself, so `newProgram` never needs to inspect `e.functions` at all to
decide what to bind. Resolution is fully delegated to the checker, which
already has `fn` in scope.

## Files changed

| File | Change |
|---|---|
| `vendor/github.com/google/cel-go/checker/checker.go` | Set `checkedRef.Name = fn.Name()` at the three `NewFunctionReference` call sites in `resolveOverload` and `checkOptional` |
| `vendor/github.com/google/cel-go/cel/program.go` | Branch checked/unchecked; walk `refMap`, filter by non-empty `Name` + non-empty `OverloadIDs`, direct `e.functions[fnName]` lookup |

No changes to `env.go` or `decls.go`.

## Why `ReferenceInfo.Name` was safe to use

`ReferenceInfo.Name` is already used for *identifier* references
(`NewIdentReference(name, value)`) but was always left empty for *function*
references (`NewFunctionReference(overloads...)`). Filling it in for function
nodes is backward-compatible — existing consumers that read `Name` for
function refs will now see the function name instead of `""`, which is a
strictly richer piece of information and does not break the existing `Equals`
or serialisation logic.

The two node types are still distinguishable in `newProgram`: a function ref
has `len(ref.OverloadIDs) > 0`; an ident ref has `len(ref.OverloadIDs) == 0`.

## Checker modification sites

```go
// site 1 — logical AND / OR (early-return path)
// checker.go ~L326
checkedRef = ast.NewFunctionReference(overload.ID())
checkedRef.Name = fn.Name()           // ← added

// site 2 — all other functions, first matching overload
// checker.go ~L357
checkedRef = ast.NewFunctionReference(overload.ID())
checkedRef.Name = fn.Name()           // ← added

// site 3 — optional field selection (_?._ operator)
// checker.go ~L170
ref := ast.NewFunctionReference("select_optional_field")
ref.Name = "select_optional_field"    // ← added
c.setReference(e, ref)
```

## Time complexity

Let:
- **F** = `len(e.functions)` ≈ 157
- **R** = `len(refMap)` ≈ 8–30 (call nodes in the AST)
- **B** = distinct functions used ≈ 4–15

| Operation | Cost | Frequency |
|---|---|---|
| Walk `refMap` + name lookups | O(R) ≈ O(8–30) | Per `newProgram` call |
| `e.functions[fnName]` | O(1) | Per unique function |
| `Bindings()` + `disp.Add()` | O(B) ≈ O(4–15) | Per `newProgram` call |
| **Total — optimised path** | **O(R)** ≈ **O(8–30)** | Per program |
| **Total — fallback path** | O(F) = O(157) | Per program (unchecked AST) |

This is the tightest hot-path of all three approaches. Checker cost is O(1)
extra per function resolution — amortised over a check that was already O(F).

## Space complexity

| Structure | Location | Size | Lifetime |
|---|---|---|---|
| `ref.Name` string | AST (`ReferenceInfo`) | O(R) × avg name len | AST lifetime |
| `seen` string set | per `newProgram` | O(B) ≈ 4–15 entries | freed after call |
| Dispatcher entries added | per `newProgram` | O(B × avgBindings) | program lifetime |
| `*functions.Overload` structs saved | per program | ~(V − B×2) ≈ ~330 | not allocated |

Compared to Patch B, no index (~5.5 KB) is held on `Env`. The only extra
memory is one `string` field per function-call node in the AST (already
allocated; this fills a previously-zero `Name` field in the existing
`ReferenceInfo` struct).

## Comparison with other patches

| | Patch A | Patch B | **Patch C** |
|---|---|---|---|
| Per-program hot path | O(F × U) ≈ O(3 140) | O(R × O) ≈ O(20–90) | **O(R) ≈ O(8–30)** |
| `e.functions` iteration | yes | no | **no** |
| Env-level index | none | ~5.5 KB | **none** |
| Checker change | no | no | **yes** |
| Extra memory per compiled AST | 0 | 0 | **O(R) strings** |

## Safety

| Case | Behaviour |
|---|---|
| Unchecked AST | `a.IsChecked()` false → original full loop |
| Checked AST, empty refMap | `len(refMap) == 0` → original full loop |
| Ident / constant nodes | `ref.Name != ""` but `len(ref.OverloadIDs) == 0` → skipped |
| Function not in `e.functions` | `fn == nil` guard → skipped gracefully |
| Planner PATH-3 bypass operators | `_&&_`, `_||_` etc. go through `resolveOverload` and get `Name` set; handled correctly |
| `select_optional_field` | Explicitly sets `Name` at the one non-`resolveOverload` site |

## Apply

```bash
git apply 0013-cel-reduce-overload-bindings/patch-c/0013-celgo-reduce-overload-bindings-patch-c.patch
```
