# kubernetes/kubernetes#131417 — VAP / CEL memory investigation

> [High memory consumption caused by CEL in `ValidatingAdmissionPolicy`
> and `ValidatingAdmissionPolicyBinding`](https://github.com/kubernetes/kubernetes/issues/131417).

A bundle of code-grounded reference docs, analysis, and patches that
together attack the per-policy CEL memory blow-up in
`kube-apiserver`.

## Layout

```
issue-131417/
├── README.md              ← this file
├── architecture/          ← reference + diagrams (read first)
├── analysis/              ← investigation, root cause, API audit
├── patches/               ← one folder per patch, each with a README
├── benchmarks/sweep/      ← controlled cel-go sweep (envs × programs)
└── tools/                 ← synthetic VAP generator
```

## Where to start

| If you want to … | Start here |
|---|---|
| Understand where VAP sits in `kube-apiserver` | [architecture/vap-architecture.md](architecture/vap-architecture.md) |
| Understand the CEL compilation hot path | [architecture/vap-cel-compilation.md](architecture/vap-cel-compilation.md) |
| Read the original investigation | [analysis/investigation.md](analysis/investigation.md) |
| Read the deepest analysis with empirical heap data | [analysis/extended-analysis.md](analysis/extended-analysis.md) |
| See the cel-go API surface for memory tuning | [analysis/celgo-memory-api-audit.md](analysis/celgo-memory-api-audit.md) |
| Apply a patch | [patches/](patches/) |
| Reproduce the per-program / per-env cost model | [benchmarks/sweep/](benchmarks/sweep/) |
| Generate synthetic VAP load | [tools/generate_policies.py](tools/generate_policies.py) |
| Test the patches on a kind cluster (offline-friendly) | [testing-on-kind.md](testing-on-kind.md) |

## Patches at a glance

The full table with status, side (k8s vs cel-go), and dependency graph
lives in [patches/README.md](patches/README.md). Highlights:

- **0009** ([`patches/0009-celgo-share-dispatcher/`](patches/0009-celgo-share-dispatcher/))
  — cel-go vendor change. Shares `interpreter.Dispatcher` per `*cel.Env`.
  Targets ~62 % of compilation-subsystem heap on the reference profile.
- **0001** ([`patches/0001-program-cache/`](patches/0001-program-cache/))
  — process-wide LRU of `cel.Program` keyed by expression text.
  Headline k8s-side change for templated workloads.
- **0008** ([`patches/0008-shrink-cache-cap/`](patches/0008-shrink-cache-cap/))
  — caps the 0001 LRU at 1500 entries based on empirical per-entry cost
  (~150–250 KB).
- **0002 / 0004 / 0005** — collapse the per-policy `F` env-build term.
- **0006** — zero-variable fast path (admission-path GC churn relief).
- **0003** — expression-text canonicalization utility (wires into
  0001 / 0002 cache keys later).

## Empirical anchors

- pprof heap snapshot of a real apiserver at 1.7 GB inuse, captured in
  [`analysis/master-analysis.md`](analysis/master-analysis.md) and
  [`analysis/extended-analysis.md`](analysis/extended-analysis.md).
- Sweep benchmark in
  [`benchmarks/sweep/RESULTS.md`](benchmarks/sweep/RESULTS.md): per-program
  ~37 KB and per-env ~14 KB in plain cel-go; both several × higher on
  apiserver-shaped envs because of the function libraries
  (`urls`, `ip`, `cidr`, `regex`, `jsonpatch`, `quantity`, `semver`,
  `lists`, …) carried in `e.functions`.

## Conventions

- Every prose claim cites a `file:line` in the kubernetes or cel-go
  source tree. Cross-reference links use the
  [text](path#Lline) form so they are clickable in IDEs.
- Patches are produced by `git format-patch` on the `compiler-cache-fix`
  branch; apply with `git am` or `git apply`.
- "k8s side" = `kubernetes/kubernetes` repo. "cel-go side" =
  `google/cel-go` upstream, consumed via vendor bump.
