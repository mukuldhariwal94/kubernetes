# 0013 — cel-go: reduce dispatcher overload bindings per CEL program

## Problem

Every call to `cel.Env.Program()` (via `newProgram`) unconditionally iterates
all ~157 registered `FunctionDecl` entries in the environment and calls
`fn.Bindings()` on each one, adding all ~345 overload entries to the
per-program `Dispatcher`.

For a typical Kubernetes ValidatingAdmissionPolicy expression that uses only
4–15 distinct functions, **~98 % of the dispatcher entries are allocated and
immediately wasted** — they occupy heap for the lifetime of every compiled
`cel.Program` but are never looked up at evaluation time.

At scale (hundreds of policies × multiple validations + match conditions) the
wasted allocation compounds significantly on the apiserver heap.

### Root cause

```go
// vendor/github.com/google/cel-go/cel/program.go  (upstream)
for _, fn := range e.functions {   // iterates all ~157 functions
    bindings, _ := fn.Bindings()   // allocates *functions.Overload structs
    disp.Add(bindings...)          // adds ~2–3 entries per function
}
```

The `Dispatcher` is a `map[string]*functions.Overload` — one entry per
allocated binding. The type-checker already knows exactly which overload IDs
are needed (stored in `ast.ReferenceMap()[id].OverloadIDs`) but this
information was never used to filter the bindings.

---

## Approaches

Three independent implementations of the same fix are provided. All share the
same two-path structure in `newProgram`:

```
if a.IsChecked() && len(refMap) != 0 {
    // OPTIMISED PATH — only add bindings for functions the AST actually uses
} else {
    // ORIGINAL PATH — unchecked AST, adds all bindings unchanged
}
```

### At a glance

| | **Patch A** | **Patch B** | **Patch C** |
|---|---|---|---|
| Folder | [`patch-a/`](patch-a/) | [`patch-b/`](patch-b/) | [`patch-c/`](patch-c/) |
| Files changed | `program.go`, `decls.go` | `program.go`, `env.go` | `program.go`, `checker.go` |
| Per-program hot path | O(F × U) ≈ O(3 140) | O(R × O) ≈ O(20–90) | O(R) ≈ O(8–30) |
| Env overhead | none | ~5.5 KB (index, once) | none |
| `e.functions` loop | kept (skips non-matching) | eliminated | eliminated |
| External API change | no | no | no |
| Status | ready | ready | ready |

---

### Patch A — inline `usedOIDs` set in `newProgram`

**Folder:** [`patch-a/`](patch-a/)
**Files:** `cel/program.go`, `common/decls/decls.go`

Builds a `usedOIDs map[string]struct{}` from `refMap` inside `newProgram`,
then iterates `e.functions`. For each function a new helper `fnOverlapsOIDs`
calls `fn.HasOverloadID(oID)` — a direct O(1) lookup into the unexported
`overloads` map — for every entry in `usedOIDs`. Functions with no overlap
are skipped entirely.

`HasOverloadID` is the only addition to `decls.go`; it avoids the
`[]*OverloadDecl` slice allocation that `OverloadDecls()` would otherwise
perform on every call.

```
newProgram (checked path):
  1. usedOIDs ← flatten refMap         O(R × O) ≈ O(20)
  2. for fn in e.functions:            O(F) = O(157)
       if fnOverlapsOIDs(fn, usedOIDs) O(|usedOIDs|) × O(1) per ID
         fn.Bindings() + disp.Add()
```

**Trade-offs:** simplest change, no new fields on any struct, but still
iterates all 157 functions per program.

**See:** [`patch-a/README.md`](patch-a/README.md)

---

### Patch B — OID→FunctionDecl index cached on `Env`

**Folder:** [`patch-b/`](patch-b/)
**Files:** `cel/program.go`, `cel/env.go`

Adds `oidToFnOnce sync.Once` + `oidToFn map[string]*FunctionDecl` fields to
`Env` and a new `overloadIndex()` method that builds the map lazily on the
first `newProgram` call. Each subsequent call gets the cached pointer in O(1).

`newProgram` then walks `refMap` directly — one O(1) map lookup per OID — and
deduplicates owning functions via a `seen map[*FunctionDecl]struct{}` pointer
map. The `e.functions` loop is **eliminated from the per-program hot path**.

```
Env.overloadIndex() — called once per Env:
  for fn in e.functions:           O(F) = O(157)
    for o in fn.OverloadDecls():   O(total overloads) = O(345)
      oidToFn[o.ID()] = fn

newProgram (checked path):
  oidIndex ← e.overloadIndex()    O(1) — cached pointer
  seen     ← map[*FunctionDecl]{}
  for ref in refMap:               O(R) ≈ O(8–30)
    for oID in ref.OverloadIDs:    O(avgOIDs) ≈ O(1–3)
      fn ← oidIndex[oID]           O(1)
      if fn not in seen: Bindings() + disp.Add()
```

**Trade-offs:** 35–150× faster per-program lookup vs Patch A at the cost of a
~5.5 KB index per `Env` instance. Index is rebuilt automatically when `Env` is
extended (zero-value `sync.Once` on derived Env).

**See:** [`patch-b/README.md`](patch-b/README.md)

---

### Patch C — function name stored in `ReferenceInfo` by checker

**Folder:** [`patch-c/`](patch-c/)
**Files:** `cel/program.go`, `checker/checker.go`

Modifies the type-checker to write `fn.Name()` into `ReferenceInfo.Name` at
every function-resolution site in `resolveOverload` (and the `select_optional_field`
site in `checkOptional`). `newProgram` reads `ref.Name` directly from the AST's
`refMap` — one O(1) lookup into `e.functions` per unique function name. No
reverse index on `Env`, no iteration over `e.functions` at all.

```
checker.go — three resolution sites:
  // logical AND/OR early-return path
  checkedRef = ast.NewFunctionReference(overload.ID())
  checkedRef.Name = fn.Name()          // ← new

  // all other functions, first matching overload
  checkedRef = ast.NewFunctionReference(overload.ID())
  checkedRef.Name = fn.Name()          // ← new

  // optional field selection
  ref := ast.NewFunctionReference("select_optional_field")
  ref.Name = "select_optional_field"   // ← new

newProgram (checked path):
  seen ← map[string]struct{}{}        // dedup by fn name
  for ref in refMap:                  O(R) ≈ O(8–30)
    fnName ← ref.Name                 O(1) — already in AST
    if fnName == "" || len(ref.OverloadIDs) == 0:
      continue                        // ident / constant node, not a fn call
    if fnName in seen: continue
    fn ← e.functions[fnName]          O(1) — direct map lookup
    fn.Bindings() + disp.Add()
```

**Trade-offs:** tightest hot path of all three approaches (O(R) per program,
no index overhead), at the cost of touching the checker. The `Name` field in
`ReferenceInfo` was always empty for function references — filling it in is
backward-compatible and makes the AST carry richer information.

**See:** [`patch-c/README.md`](patch-c/README.md)

---

## Complexity summary

| Metric | Upstream | Patch A | Patch B | Patch C |
|---|---|---|---|---|
| Per-program `Bindings()` calls | 157 | ~4–15 | ~4–15 | ~4–15 |
| Per-program dispatcher entries | ~345 | ~10–30 | ~10–30 | ~10–30 |
| Per-program `e.functions` iterations | 157 | 157 | 0 | 0 |
| Env-level index overhead | 0 | 0 | ~5.5 KB | 0 |
| Checker change required | no | no | no | yes |

---

## Safety (all approaches)

- **Unchecked AST**: `a.IsChecked()` false → original full loop. Zero behaviour change.
- **Checked AST, empty refMap**: `len(refMap) == 0` → original full loop.
- **Planner PATH-3 bypass operators** (`_&&_`, `_||_`, `_?_:_`, etc.): these
  operators are planned without consulting the dispatcher; they produce no
  `OverloadIDs` in `refMap` or are reached by function-name fallback. All
  approaches handle this correctly — either via `fn.Bindings()` which always
  adds the fn-name key, or via direct `e.functions[fnName]` lookup.
- **Env extensions**: all three approaches are safe — Patch B rebuilds its
  index via a zero-value `sync.Once` on the derived `Env`.

---

## Apply

```bash
# Patch A
git apply 0013-cel-reduce-overload-bindings/patch-a/0013-celgo-reduce-overload-bindings.patch

# Patch B
git apply 0013-cel-reduce-overload-bindings/patch-b/0013-celgo-reduce-overload-bindings-patch-b.patch

# Patch C
git apply 0013-cel-reduce-overload-bindings/patch-c/0013-celgo-reduce-overload-bindings-patch-c.patch
```
