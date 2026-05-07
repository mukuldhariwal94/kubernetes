# 0009 — Share `interpreter.Dispatcher` per `*cel.Env` (Patch D, cel-go)

**Status:** drafted, applied to local branch (commit `776f7fc`).
**Side:** **cel-go vendor** (`vendor/github.com/google/cel-go/cel/`).
**Headline impact (cel-go side):** **~62 % of compilation-subsystem heap
on the reference profile** (FunctionDecl.Bindings 40.5 % +
defaultDispatcher.Add 22.8 %).

## Problem

In `cel-go`,
[`vendor/github.com/google/cel-go/cel/program.go:172-204`](../../../vendor/github.com/google/cel-go/cel/program.go#L172-L204),
`newProgram` did this **per call**:

```go
disp := interpreter.NewDispatcher()                    // fresh, empty
…
for _, fn := range e.functions {                       // every function in the env
    bindings, err := fn.Bindings()                     // materializes closures
    …
    err = disp.Add(bindings...)                        // copies each into disp
}
```

Two consequences observed in pprof:

- `decls.(*FunctionDecl).Bindings` retains 40.5 % of total heap (~700 MB
  on a 1.7 GB snapshot) — every program holds its own copy of every
  stdlib + Kubernetes-library function-binding closure.
- `interpreter.(*defaultDispatcher).Add` retains 22.8 % (~400 MB) — the
  per-program overload-id table populated from those bindings.

These are functionally constant **per `*cel.Env`** — they depend only on
`e.functions`, which is immutable after env construction. Two programs
built from the same env populate byte-for-byte identical dispatchers.

## What it does

- Adds three private fields to `cel.Env`: `dispOnce sync.Once`,
  `dispCache interpreter.Dispatcher`, `dispErr error`.
- Adds `(e *Env) sharedDispatcher() (interpreter.Dispatcher, error)` —
  builds the dispatcher lazily, populates it from `e.functions`,
  guarded by `sync.Once`.
- `newProgram` now calls `e.sharedDispatcher()` and wraps the result
  with `interpreter.ExtendDispatcher(parent)`. The deprecated
  `Functions(...)` `ProgramOption` (the only caller that mutates
  `p.dispatcher`) writes to the per-program overlay; the shared parent
  is never mutated.

`e.functions` is deep-copied by `Env.Extend` into the child env, so
every extended env builds *its own* shared dispatcher on first use.
Parent and child envs do not alias dispatchers — additive `Function()`
declarations on a child env behave exactly as before.

## Why preserves semantics

- `Dispatcher.Add` is the only mutation; all reads (`FindOverload`,
  dispatch lookups) are read-only. Once `e.functions` is fully populated
  (which happens during `cel.NewEnv` / `Extend.configure`), the
  dispatcher built from it is observationally immutable.
- Programs built from the same env see the same dispatch behaviour
  whether the dispatcher is shared or per-program.
- The `Functions(...)` `ProgramOption` (deprecated, in
  [`cel/options.go:416-423`](../../../vendor/github.com/google/cel-go/cel/options.go#L416-L423))
  mutates `p.dispatcher`. With this patch `p.dispatcher` is the
  per-program `ExtendDispatcher` overlay, not the shared parent — so
  mutation is local.

## Expected savings

| Workload | Before | After |
|---|---|---|
| 1 000 policies × 5 expressions = 5 000 programs | 5 000 × dispatcher copies | 1 dispatcher per env (typically ≤ a handful per process) |
| 1.7 GB reference snapshot | ~1 GB in Bindings + dispatcher | low-double-digit MB |

Combined with 0001 (program-text dedup), the residual heap of the
compilation subsystem is dominated by env state (addressed by 0004 / 0005
/ 0006) plus the unavoidable per-expression `Interpretable` plan tree
(~5 MB / 1.19 % in the reference profile).

## Tested

```
go test ./vendor/github.com/google/cel-go/cel/...                          PASS
go test ./staging/src/k8s.io/apiserver/pkg/admission/plugin/cel/...        PASS
go test ./staging/src/k8s.io/apiserver/pkg/admission/plugin/policy/...     PASS
go test ./staging/src/k8s.io/apiserver/pkg/cel/...                         PASS
```

## Upstream path

This is a vendor change. The upstream destination is **google/cel-go**
(file a PR in that repo), then SIG API Machinery consumes it via a vendor
bump in k8s/k8s.

## Risks

- **Mutability assumption:** relies on `Dispatcher` being effectively
  immutable after initial population. Today it is — `Add` is only called
  from `newProgram` (mitigated by the `ExtendDispatcher` overlay) and
  from the deprecated `Functions(...)` option. Future cel-go changes
  could violate this; the patch should include a comment locking that
  invariant.
- No backward-compat issues for Kubernetes — internal to cel-go's
  program construction; Kubernetes callers see no API change.

## Apply

```
git apply patches/0009-celgo-share-dispatcher/0009-vendor-cel-go-share-interpreter.Dispatcher-per-cel.E.patch
```

## See also

- Detailed rationale in [`../../analysis/extended-analysis.md`](../../analysis/extended-analysis.md), section 3.4.
- Heap-profile validation in [`../../analysis/master-analysis.md`](../../analysis/master-analysis.md).
