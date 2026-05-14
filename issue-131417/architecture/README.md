# Architecture & reference

Code-grounded reference docs. Every claim points back to a file:line in
the kubernetes or cel-go source tree, so the citations are the
authority — read the code, not the prose.

| Doc | Scope |
|---|---|
| [vap-architecture.md](vap-architecture.md) | Top-down VAP architecture in `kube-apiserver`: object model, plugin lifecycle, control-plane compilation pipeline, data-plane evaluation. |
| [vap-deep-dive.md](vap-deep-dive.md) | End-to-end deep dive: REST request → etcd → informer sync → CEL compilation → binding matching → evaluation → decision. Most exhaustive narrative. |
| [vap-cel-compilation.md](vap-cel-compilation.md) | Focused diagram pack for the CEL compilation path: how expressions enter, are compiled, cached, and evaluated. Annotated with current memory hotspots. |
| [cel-go-deep-dive.md](cel-go-deep-dive.md) | Reference for `github.com/google/cel-go` itself: parser → checker → interpreter, env/program/dispatcher internals, cost tracking, optimizer hooks. The substrate the rest of the analysis sits on. |

## Reading order for a newcomer

1. **vap-architecture.md** — "where am I in kube-apiserver".
2. **vap-cel-compilation.md** — the specific subsystem the patches target.
3. **cel-go-deep-dive.md** — the upstream library being driven.
4. **vap-deep-dive.md** — when the others leave a question open.
