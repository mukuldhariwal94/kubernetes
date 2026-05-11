# Analysis & investigation

The analytical narrative behind the patches in
[`../patches/`](../patches/). Read after the architecture references in
[`../architecture/`](../architecture/) — these docs assume you know
where in `kube-apiserver` the VAP plugin lives and what cel-go's
parser → checker → planner pipeline looks like.

| Doc | Scope |
|---|---|
| [investigation.md](investigation.md) | Original investigation of issue #131417: codebase mapping, repro plan, hypothesis chain that produced the first three patches. |
| [master-analysis.md](master-analysis.md) | Deep-dive analysis written alongside patches 0001–0003. Memory model derivation, root cause, optimization-approach matrix, success criteria. |
| [extended-analysis.md](extended-analysis.md) | Follow-up analysis with empirical heap-profile data (pprof + sweep benchmark). Adds patches 0004 / 0005 / 0006 / 0009. Re-orders priorities against the first-principles model — pprof revealed `FunctionDecl.Bindings` + `Dispatcher` as ~62 % of heap, which 0009 directly attacks. |
| [celgo-memory-api-audit.md](celgo-memory-api-audit.md) | Audit of every cel-go public API option (EvalOption / ProgramOption / EnvOption / Library) for memory-saving levers. Top recommendations at the end. Useful when adding new compile sites or tuning existing ones. |
| [architecture-redesign.md](architecture-redesign.md) | First-principles re-evaluation of VAP's evaluator residency model, with a Kyverno comparison. Argues that all the patches together divide the constant but leave the `O(N_policies)` asymptote intact, and that the right structural change is **demand-driven `cel.Program` materialization** behind a `CheckedExpr`-backed Tier-0 cache. |
| [tiered-compilation-design.md](tiered-compilation-design.md) | Engineering-level design for the demand-driven model: exact mechanics of what is cached today, the cel-go `cel.Program` memory layout, a concrete three-tier cache (hot `cel.Program` / warm checked-AST / cold string), W-TinyLFU eviction, sharded/lock-free concurrency, a hybrid eager budget, and a point-by-point trade-off analysis (memory, latency, throughput, GC, churn, burst, warm-up, miss amplification, multi-tenant fairness). Production code paths only — test/mock/benchmark scaffolding excluded. |

## Reading order

1. **investigation.md** — for issue context and how the problem was scoped.
2. **master-analysis.md** — for the per-patch reasoning behind 0001/0002/0003.
3. **extended-analysis.md** — for the empirical profile and the additional
   patches it justified.
4. **celgo-memory-api-audit.md** — when looking for *additional* tuning
   opportunities beyond the patch series.
5. **architecture-redesign.md** — for the first-principles case that the
   patches divide the constant but don't change the `O(N_policies)` asymptote.
6. **tiered-compilation-design.md** — for the concrete demand-driven /
   tiered-cache design that does change the asymptote, and the full trade-off
   analysis at extreme cluster scale.
