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

## Reading order

1. **investigation.md** — for issue context and how the problem was scoped.
2. **master-analysis.md** — for the per-patch reasoning behind 0001/0002/0003.
3. **extended-analysis.md** — for the empirical profile and the additional
   patches it justified.
4. **celgo-memory-api-audit.md** — when looking for *additional* tuning
   opportunities beyond the patch series.
