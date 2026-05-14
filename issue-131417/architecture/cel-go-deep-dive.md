# cel-go Deep Dive — Architecture, Usage, and Optimization

A code-grounded reference for building high-performance Go systems on top of
[`github.com/google/cel-go`](https://github.com/google/cel-go). Every claim
below points back to a file in the cel-go repo so you can verify and trace
behavior yourself.

> Audience: experienced Go engineers integrating CEL into latency- or
> throughput-sensitive paths (admission controllers, rules engines,
> filtering / authz layers).

---

## 1. High-Level Architecture

cel-go is a **multi-stage compiler + tree-walking interpreter**. A string
expression travels through five distinct stages, each with its own package:

| Stage            | Package                          | Output                                                             | When it runs |
|------------------|----------------------------------|--------------------------------------------------------------------|--------------|
| Parse            | [parser/](parser/)               | `*ast.AST` (untyped syntax tree, macros expanded)                  | Compile time |
| Macro expansion  | [parser/macro.go](parser/macro.go) | Rewritten AST (e.g. `.all()` → `__comprehension__`)              | During parse |
| Type-check       | [checker/](checker/)             | Typed `*ast.AST` (every node has a `*types.Type`)                  | Compile time |
| Plan             | [interpreter/planner.go](interpreter/planner.go) | `Interpretable` tree (executable nodes)              | `env.Program` |
| Evaluate         | [interpreter/interpretable.go](interpreter/interpretable.go) | `ref.Val` result + optional `*EvalDetails`         | Runtime      |

### Core Components

- **`cel.Env`** ([cel/env.go:134](cel/env.go#L134)) — the configuration
  container. Holds the variable/function declarations, macros, container
  (namespace), the `types.Adapter` (Go → CEL value conversion) and
  `types.Provider` (type metadata), and a lazily initialized type checker
  (`chk *checker.Env`). The checker is built once via `sync.Once`
  ([cel/env.go:160](cel/env.go#L160)).
- **Parser** ([parser/parser.go:88](parser/parser.go#L88)) — ANTLR-based
  (`antlr "github.com/antlr4-go/antlr/v4"`). Produces an untyped AST and
  expands macros inline.
- **Checker** ([checker/checker.go:51](checker/checker.go#L51)) — tree-walks
  the parsed AST, resolves overloads, attaches `*types.Type` to every node
  (`TypeMap: map[int64]*types.Type`).
- **AST** — `*ast.AST` (in `common/ast`) carries node IDs, source info,
  and the type map. The `cel.Ast` wrapper ([cel/env.go:48](cel/env.go#L48))
  is the public handle.
- **Program** ([cel/program.go:31](cel/program.go#L31)) — a planned,
  immutable, thread-safe executable. Built once via `env.Program(ast, ...)`.
- **Evaluator** — the planner ([interpreter/planner.go:71](interpreter/planner.go#L71))
  lowers each AST node to an `Interpretable`
  ([interpreter/interpretable.go:32](interpreter/interpretable.go#L32))
  with an `Eval(Activation) ref.Val` method. Evaluation is a recursive
  walk; there is no bytecode VM.

### Compile-time vs Runtime

| Compile-time (do once)                                       | Runtime (do per request)                       |
|--------------------------------------------------------------|------------------------------------------------|
| Construct `Env` with `NewEnv` / `Extend`                     | Build / reuse an `Activation` (input bindings) |
| `env.Compile(src)` → parse + check                           | `prg.Eval(activation)`                         |
| `env.Program(ast, opts...)` → plan + decorate (constfold,…)  | `prg.ContextEval(ctx, activation)` for cancel  |

The boundary is critical: **anything you can hoist out of the request path
(env creation, compilation, program construction) should be hoisted.**

---

## 2. Architecture Diagram

```
┌─────────────────────────────────────────────────────────────────────────┐
│                          COMPILE TIME (do once)                         │
│                                                                         │
│   "request.user in admins && resource.type == 'doc'"                    │
│                       │                                                 │
│                       ▼                                                 │
│              ┌──────────────────┐    Macros (has, all, exists,          │
│              │   parser/        │    map, filter, exists_one)           │
│              │   ANTLR + macros │ ─► expanded into __comprehension__    │
│              └────────┬─────────┘                                       │
│                       │ *ast.AST (untyped)                              │
│                       ▼                                                 │
│              ┌──────────────────┐    Resolves overloads,                │
│              │   checker/       │    sets *types.Type on every node     │
│              │   type checker   │    via TypeMap                        │
│              └────────┬─────────┘                                       │
│                       │ *ast.AST (typed) ── wrapped as cel.Ast         │
│                       ▼                                                 │
│              ┌──────────────────┐    env.Program(ast, opts):            │
│              │ interpreter/     │     • planner.Plan walks AST          │
│              │ planner          │     • applies decorators              │
│              │ + decorators     │       (constfold, regex precompile,   │
│              └────────┬─────────┘        observe, interrupt, exhaust)   │
│                       │                                                 │
│                       ▼                                                 │
│              ┌──────────────────┐                                       │
│              │     PROGRAM      │ ◄── stateless, thread-safe, cachable  │
│              │  (Interpretable  │     SHARE this across goroutines      │
│              │      tree)       │                                       │
│              └────────┬─────────┘                                       │
└────────────────────── │ ────────────────────────────────────────────────┘
                        │
┌────────────────────── │ ────────────────────────────────────────────────┐
│                       │           RUNTIME (per request)                 │
│                       ▼                                                 │
│   inputs: map[string]any  ─►  Activation  ──►  prg.Eval(act)            │
│   (or custom Activation)      (ResolveName)        │                    │
│                                                    │                    │
│         ┌──────────────────────────────────────────┘                    │
│         ▼                                                               │
│   tree-walk Interpretable nodes                                         │
│   ─ InterpretableConst    (literal)                                     │
│   ─ InterpretableAttribute (var / field access via attrFactory)         │
│   ─ InterpretableCall      (function dispatch via Dispatcher)           │
│   ─ InterpretableConstructor (list/map/struct init)                     │
│         │                                                               │
│         ▼                                                               │
│   ref.Val  (+ *EvalDetails when OptTrackState / OptTrackCost set)       │
└─────────────────────────────────────────────────────────────────────────┘

    Allocation hotspots:                  Optimization opportunities:
    ─────────────────────                  ──────────────────────────
    • parser AST nodes        (compile)   • OptOptimize: constfold + regex
    • checker TypeMap entries (compile)   • NewConstantFoldingOptimizer
    • Interpretable tree      (program)   • Share Env + Program (immutable)
    • ref.Val boxing          (eval)      • Avoid map[string]any: custom
    • Activation map lookup   (eval)        Activation skips boxing
```

---

## 3. API Documentation (Go)

### A. Basic Usage

```go
import (
    "github.com/google/cel-go/cel"
)

// 1. Build the env once (process- or package-scoped).
env, err := cel.NewEnv(
    cel.Variable("x", cel.IntType),
)
if err != nil { panic(err) }

// 2. Compile (parse + check). Cache the AST.
ast, iss := env.Compile("x > 10")
if iss.Err() != nil { panic(iss.Err()) }

// 3. Plan into a Program. Cache the Program.
prg, err := env.Program(ast)
if err != nil { panic(err) }

// 4. Evaluate as many times as you like, from any goroutine.
out, _, err := prg.Eval(map[string]any{"x": 15})
// out.Value() == true
```

Key entry points: [`cel.NewEnv`](cel/env.go#L340),
[`Env.Compile`](cel/env.go#L447), [`Env.Program`](cel/env.go#L673),
[`Program.Eval`](cel/program.go#L301).

`NewEnv` extends a pre-configured stdlib env. Use
[`cel.NewCustomEnv`](cel/env.go#L363) when you want to **subset** the
standard library (e.g. drop `string` extensions in untrusted contexts).

### B. Advanced Usage

**Custom functions** ([cel/decls.go:195](cel/decls.go#L195)):

```go
env, _ := cel.NewEnv(
    cel.Variable("s", cel.StringType),
    cel.Function("reverse",
        cel.Overload("reverse_string",
            []*cel.Type{cel.StringType}, cel.StringType,
            cel.UnaryBinding(func(v ref.Val) ref.Val {
                s := v.(types.String)
                runes := []rune(string(s))
                for i, j := 0, len(runes)-1; i < j; i, j = i+1, j-1 {
                    runes[i], runes[j] = runes[j], runes[i]
                }
                return types.String(string(runes))
            }))),
)
```

**Custom types** — register protobuf messages or Go structs via
[`cel.Types(...)`](cel/options.go#L333) /
[`cel.CustomTypeAdapter`](cel/options.go#L151) /
[`cel.CustomTypeProvider`](cel/options.go#L163). The `Adapter`
([common/types/ref/provider.go:54](common/types/ref/provider.go#L54))
converts native Go values to `ref.Val`; the `Provider` resolves type
metadata and constructs values.

**Macros** are defined in [parser/macro.go](parser/macro.go). The standard
set (`has`, `all`, `exists`, `exists_one`, `map`, `filter`) is in
`parser.AllMacros` ([parser/macro.go:435](parser/macro.go#L435)). You can
register custom macros via `cel.Macros(...)`.

**Partial evaluation** — enable `cel.EvalOptions(cel.OptPartialEval)` and
pass a `PartialActivation` with `[]*AttributePattern` describing the
unknown attributes. Unresolved branches return an `unknown` value rather
than erroring, enabling residual expressions.

**Errors / Issues** — `Compile` returns `(*Ast, *Issues)`. Always check
`iss.Err() != nil` ([cel/env.go:947](cel/env.go#L947)). `Issues` aggregates
parser and checker errors with source locations.

**Cancellation** — use [`Program.ContextEval`](cel/program.go#L352) and
tune [`cel.InterruptCheckFrequency(n)`](cel/options.go#L723). Comprehension
iterations sample the context every `n` steps; on cancel you get
`interpreter.InterruptError`.

### C. Reusability Patterns

- **`Env`**: build once, share read-only across goroutines. To add more
  variables/functions for a sub-scope, call
  [`env.Extend(opts...)`](cel/env.go#L478) — it returns a *new* Env with
  copied state, leaving the parent untouched.
- **`Ast`**: immutable; safe to cache by source-string hash.
- **`Program`**: explicitly documented stateless, thread-safe, and
  cachable ([README.md:94](README.md#L94)). Cache by `(envID, source)`.
- **Shared evaluation layer**: a typical pattern is a sync.Map keyed by
  expression source (or a normalized hash) holding `Program` values, with
  a single shared `Env` per logical scope.

```go
type Engine struct {
    env  *cel.Env
    prgs sync.Map // map[string]cel.Program
}

func (e *Engine) Eval(src string, in map[string]any) (ref.Val, error) {
    if v, ok := e.prgs.Load(src); ok {
        out, _, err := v.(cel.Program).Eval(in)
        return out, err
    }
    ast, iss := e.env.Compile(src)
    if iss.Err() != nil { return nil, iss.Err() }
    prg, err := e.env.Program(ast, cel.EvalOptions(cel.OptOptimize))
    if err != nil { return nil, err }
    actual, _ := e.prgs.LoadOrStore(src, prg)
    out, _, err := actual.(cel.Program).Eval(in)
    return out, err
}
```

---

## 4. Internal Mechanics

### `env.Compile(src)`
1. Calls `Env.Parse` ([cel/env.go:652](cel/env.go#L652)) → ANTLR parse +
   macro expansion → `*ast.AST` with source info.
2. Calls `Env.Check` ([cel/env.go:397](cel/env.go#L397)) → lazily builds
   `*checker.Env` (sync.Once-guarded) → walks the AST resolving identifiers
   and overloads, populates the `TypeMap`.
3. Returns `(*Ast, *Issues)`. Allocations: parser nodes, ANTLR token
   stream (released after parse), TypeMap entries proportional to AST
   size.

### `env.Program(ast, opts...)`
1. Resolves `ProgramOption`s into a `prog` struct
   ([cel/program.go](cel/program.go)).
2. Builds an `interpreter.Dispatcher` populated with function bindings
   (guarded by `sync.Once` per Env, [cel/env.go:149](cel/env.go#L149)).
3. Constructs a `planner` with the dispatcher, type provider/adapter,
   `AttributeFactory`, and container.
4. `planner.Plan(ast.Expr())` recursively converts each `ast.Expr` to an
   `Interpretable` node and applies all configured **decorators**
   ([interpreter/decorators.go:24](interpreter/decorators.go#L24)) — e.g.
   `decObserveEval` (for state tracking), `decInterruptFolds`,
   `decDisableShortcircuits` (exhaustive eval), constant folding,
   regex precompile.
5. Returns a `Program`. Allocations: the `Interpretable` tree (lives for
   the program's lifetime), regex `*regexp.Regexp` if `OptOptimize`, any
   constant-folded `ref.Val`s.

### `prg.Eval(input)`
1. Wraps `input` in an `Activation` if it's a map
   ([cel/program.go:301](cel/program.go#L301)).
2. Recursively walks the `Interpretable` tree:
   - `InterpretableConst.Eval` returns its cached `ref.Val`.
   - `InterpretableAttribute.Eval` calls `Resolve(act)` against the
     `AttributeFactory` machinery
     ([interpreter/attributes.go](interpreter/attributes.go)) which handles
     namespacing, optional fields, and unknown propagation.
   - `InterpretableCall.Eval` evaluates args and dispatches via the
     `Dispatcher` (function table).
   - `InterpretableConstructor.Eval` builds lists/maps/structs.
3. Allocations per call: `ref.Val` boxing for intermediate results, map
   lookups in the activation, any list/map literals constructed by the
   expression. The `Interpretable` tree itself is **read-only and shared**.

**Immutability/reuse summary**:

| Object         | Mutable? | Safe to share across goroutines after build? |
|----------------|----------|-----------------------------------------------|
| `Env`          | Read-only after construction | Yes (lazy fields are sync-protected) |
| `Ast`          | Immutable | Yes |
| `Program`      | Immutable | **Yes — designed for it** |
| `Activation`   | Effectively read-only per Eval | **No** — one per eval / goroutine |
| `EvalDetails`  | Mutated during Eval | No — one per eval |

---

## 5. Performance & Optimization

### A. Compilation

- **Never compile in the request path.** `Compile` runs the full ANTLR
  pipeline + type checker; it dominates the cost of one Eval by orders of
  magnitude.
- **Cache `Program`**, not just `Ast`. The plan + decorator pass is also
  non-trivial; `Program` is the right caching unit.
- **Share one `Env`** per logical scope. `Env.Extend` is for *adding*
  declarations to a derived scope — it copies internal maps, so don't call
  it on the hot path either.
- **Pre-compile at startup** when the expression set is known. For
  user-supplied expressions, use a bounded LRU keyed by source hash.

### B. Execution

- **Use `OptOptimize`** ([cel/options.go:686](cel/options.go#L686)) when
  the program will run more than a handful of times. It folds constant
  subexpressions and **pre-compiles regex literals** in `matches()`
  (turning a per-eval `regexp.Compile` into a one-time cost and surfacing
  bad regexes at program-build time).
- **Wrap with `NewConstantFoldingOptimizer`**
  ([cel/folding.go:60](cel/folding.go#L60)) for AST-level rewrite when
  many of your expressions share constant inputs.
- **Avoid `cel.OptExhaustiveEval` in production.** It disables
  short-circuit `&&` / `||`, evaluating both branches always.
- **Avoid `cel.OptTrackState` / `cel.OptTrackCost`** unless you need them
  — they wrap every `Interpretable` in observers, which is non-zero cost.
- **Prefer custom `Activation`** over `map[string]any` when the input is
  a struct you already hold. Implementing
  [`interpreter.Activation`](interpreter/activation.go#L27) directly skips
  per-key map allocation and lets you return native Go values that the
  `Adapter` boxes only as needed.
- **Use proto messages or registered types** for nested/object inputs.
  Reflection over arbitrary `interface{}` is the slow path; the type
  provider can route registered types through faster code.

### C. Memory

- For **many similar expressions**: normalize source strings (trim,
  canonicalize whitespace) before keying your Program cache so trivially
  different sources share a planned program.
- For **one expression evaluated against many inputs**: build the
  `Program` once with `OptOptimize`, then funnel every request through it.
- For **bounded user-supplied expressions**: use an LRU with a memory
  budget, not just a count — large comprehensions can produce sizable
  `Interpretable` trees.
- The `Interpretable` tree is the primary long-lived allocation per
  Program. There is no per-Eval heap arena; reduce per-Eval garbage by
  shaping inputs to avoid forcing `Adapter.NativeToValue` to allocate
  wrapper `ref.Val`s for primitive types.

---

## 6. Production Best Practices

### Thread Safety

- **`Env`**: safe for concurrent reads after construction. The lazy
  checker init is sync.Once-guarded
  ([cel/env.go:160](cel/env.go#L160)); the dispatcher init is sync.Once
  guarded per Program ([cel/program.go:196](cel/program.go#L196)).
- **`Ast`**: immutable, safe.
- **`Program`**: explicitly designed to be shared; `Eval` and
  `ContextEval` are reentrant.
- **`Activation`**: **not** safe to share — instantiate per evaluation.
  Hierarchical activations require exclusive access to the parent chain.

### Lifecycle

```
Process start ──► build Env(s) ──► compile + plan known Programs
Per request   ──► build Activation ──► prg.ContextEval(ctx, act) ──► consume ref.Val
```

For dynamic (user-supplied) expressions, add an LRU between
"per request" and "compile + plan", with a hard timeout on `Compile`
(it can be slow for pathological inputs).

### Sandboxing & Safety for User Input

- Start from `cel.NewCustomEnv` and add only the stdlib subsets you
  actually need. Avoid extensions like `ext.Strings()` regex helpers if
  you don't need them — they expand the attack surface.
- Always set `cel.CostLimit(...)` ([cel/options.go:761](cel/options.go#L761))
  to bound CPU per Eval. Pair with a checker `CostEstimator` to reject
  too-expensive expressions at compile time.
- Always use `ContextEval` with a deadline.
- Tune `cel.InterruptCheckFrequency` low enough that long comprehensions
  honor cancellation promptly.
- Reject expressions whose checked AST exceeds a node-count budget
  (walk `ast.Expr()` after `Check`).

---

## 7. Example Use Cases

### A. Admission / Authorization Policy

```go
env, _ := cel.NewEnv(
    cel.Variable("request",  cel.MapType(cel.StringType, cel.DynType)),
    cel.Variable("resource", cel.MapType(cel.StringType, cel.DynType)),
    cel.Variable("subject",  cel.MapType(cel.StringType, cel.DynType)),
)

// Compile each policy ONCE at policy-load time.
ast, iss := env.Compile(`
    subject.role == "admin" ||
    (resource.owner == subject.id && request.verb in ["get","list"])
`)
if iss.Err() != nil { return iss.Err() }

prg, _ := env.Program(ast,
    cel.EvalOptions(cel.OptOptimize),
    cel.CostLimit(10_000),
)

// Per request: Eval is cheap and safe to call concurrently.
allow, _, err := prg.ContextEval(ctx, map[string]any{
    "request":  reqMap,  "resource": resMap,  "subject": subMap,
})
```

Design notes: keep one `Env` per policy schema; one `Program` per loaded
policy; never call `Compile` in the admission hot path.

### B. Feature Flag / Rules Engine

Each rule is a CEL expression; users author them in a UI.

```go
type Rule struct {
    ID      string
    Source  string
    Program cel.Program // built at rule-save time
}

func (r *RuleSet) Match(ctx context.Context, ev Event) ([]string, error) {
    in := ev.AsActivation() // custom Activation; no map[string]any
    var hits []string
    for _, r := range r.Rules {
        v, _, err := r.Program.ContextEval(ctx, in)
        if err != nil { continue }
        if v == types.True { hits = append(hits, r.ID) }
    }
    return hits, nil
}
```

Design notes: validate + compile the rule once at *save* time so bad
expressions are rejected before they reach production. Reuse one
`Activation` across all rules in a single match call (it doesn't escape
the goroutine).

### C. Streaming Filter / Validator

```go
prg, _ := env.Program(ast,
    cel.EvalOptions(cel.OptOptimize),
    cel.InterruptCheckFrequency(64),
    cel.CostLimit(50_000),
)

for msg := range incoming {
    out, _, err := prg.ContextEval(ctx, msg) // msg implements Activation
    if err != nil || out != types.True { continue }
    forward(msg)
}
```

Design notes: implementing `interpreter.Activation` directly on `msg`
removes per-message map allocation. The `Program` is shared across all
worker goroutines.

---

## 8. Common Pitfalls

| Pitfall | Why it hurts | Fix |
|---|---|---|
| Calling `env.Compile` per request | ANTLR + type-check dominates Eval cost | Cache `Program`s |
| Building `cel.NewEnv` per request | Allocates registries, copies stdlib decls | Build once, share |
| Passing `map[string]any` for a hot struct | Per-call map allocation + repeated `NativeToValue` boxing | Implement `interpreter.Activation` |
| Ignoring `iss.Err()` after `Compile` | `prg` may be nil or behave unexpectedly | Always check |
| Using `OptExhaustiveEval` in prod | Disables short-circuit; doubles work on `&&`/`||` | Only enable for debugging |
| Untrusted source without `CostLimit` | Adversarial expressions can pin a CPU | `CostLimit` + `ContextEval` deadline |
| `regexp` in `matches()` without `OptOptimize` | Regex compiled on every Eval | `cel.EvalOptions(cel.OptOptimize)` |
| Sharing one `Activation` across goroutines | No internal locking | One `Activation` per Eval |

---

## 9. Final Intuition

> **A CEL string becomes a pre-walked, pre-typed tree of tiny Go closures
> that you call directly.**

Concretely, cel-go does at compile time everything that doesn't depend on
your inputs:

1. **Parse** the source into a syntax tree (ANTLR).
2. **Expand macros** (`.all`, `.exists`, `.map`, …) into primitive
   comprehension nodes.
3. **Type-check**: bind every identifier and operator to a concrete
   overload, attaching a `*types.Type` to every node.
4. **Plan**: walk that typed tree and emit one `Interpretable` Go object
   per node — each is essentially a struct holding pointers to its child
   `Interpretable`s plus an `Eval(Activation) ref.Val` method.
5. **Decorate**: fold constants, pre-compile regexes, and (optionally)
   wrap nodes for state/cost tracking or interruption.

What's left at runtime is the irreducible kernel: read your inputs out of
the `Activation` and walk the planned tree, calling `Eval` on each node.
There is no parsing, no type resolution, no overload search, and no
reflection-driven dispatch on the hot path. The `Program` you cache is a
ready-to-execute graph; calling `prg.Eval` is closer to invoking a
hand-written Go function than interpreting source.

That's why the single most important rule for production cel-go is:
**push everything into program build, share the `Program`, and pass inputs
via the lightest-weight `Activation` you can manage.**

---

## Appendix: Quick File Map

| Concern | File |
|---|---|
| Public Env API | [cel/env.go](cel/env.go) |
| Public Program API & EvalDetails | [cel/program.go](cel/program.go) |
| Env / Program options | [cel/options.go](cel/options.go) |
| Variable / Function / Type decls | [cel/decls.go](cel/decls.go) |
| Constant folding optimizer | [cel/folding.go](cel/folding.go) |
| Parser entry point | [parser/parser.go](parser/parser.go) |
| Standard macros | [parser/macro.go](parser/macro.go) |
| Type checker | [checker/checker.go](checker/checker.go) |
| Planner (AST → Interpretable) | [interpreter/planner.go](interpreter/planner.go) |
| Interpretable node types | [interpreter/interpretable.go](interpreter/interpretable.go) |
| Activation interfaces | [interpreter/activation.go](interpreter/activation.go) |
| Attribute resolution | [interpreter/attributes.go](interpreter/attributes.go) |
| Decorators (constfold, observe, …) | [interpreter/decorators.go](interpreter/decorators.go) |
| Runtime cost tracker | [interpreter/runtimecost.go](interpreter/runtimecost.go) |
| `ref.Val` / `ref.Type` | [common/types/ref/reference.go](common/types/ref/reference.go) |
| Type adapter / provider | [common/types/ref/provider.go](common/types/ref/provider.go) |
