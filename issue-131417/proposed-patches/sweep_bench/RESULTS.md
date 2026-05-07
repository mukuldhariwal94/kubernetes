# Sweep Benchmark — Results

Run from repo root via `go run ./issue-131417/proposed-patches/sweep_bench`.
Plain cel-go env (no Kubernetes-specific libraries); slopes are
*lower bounds* on what kube-apiserver would see in production because k8s
registers many additional function libraries (urls, ip, cidr, regex, jsonpatch,
quantity, semver, lists, …), each adding to the per-`*FunctionDecl` retained
count.

## Raw output

```
=== cel-go program retention sweep ===
(values measured as HeapInuse delta after runtime.GC × 3)

--- Scenario A: 1 env, N unique-text programs ---
    (probes per-program cost when env is shared)
  N=100      HeapInuse delta=    4.18 MB    HeapObjects delta=24528
  N=500      HeapInuse delta=   18.20 MB    HeapObjects delta=105799
  N=1000     HeapInuse delta=   36.60 MB    HeapObjects delta=211731
  N=5000     HeapInuse delta=  183.84 MB    HeapObjects delta=1059720
  N=10000    HeapInuse delta=  368.02 MB    HeapObjects delta=2119712

--- Scenario B: N envs (each separately constructed), 1 program each ---
    (probes per-env cost; this is what mustBuildEnvs / NewCompositedCompiler scale on)
  N=10       HeapInuse delta=  368.00 KB    HeapObjects delta=2602
  N=100      HeapInuse delta=    4.84 MB    HeapObjects delta=26067
  N=500      HeapInuse delta=   25.41 MB    HeapObjects delta=130712
  N=1000     HeapInuse delta=   51.20 MB    HeapObjects delta=261711
  N=2000     HeapInuse delta=  102.80 MB    HeapObjects delta=523711

--- Scenario C: N envs × M programs each (VAP-at-scale model) ---
    (combines per-env and per-program costs)
  envs=100  progs/env=5    total=500       HeapInuse delta=   21.12 MB
  envs=100  progs/env=50   total=5000      HeapInuse delta=  189.91 MB
  envs=1000 progs/env=5    total=5000      HeapInuse delta=  214.42 MB
  envs=1000 progs/env=50   total=50000     HeapInuse delta=    1.86 GB
```

## Per-unit costs (derived)

### Scenario A: per-program cost, env shared

| N | bytes/program |
|---|---|
| 100 | ~42 KB |
| 500 | ~36 KB |
| 1,000 | ~37 KB |
| 5,000 | ~37 KB |
| 10,000 | ~37 KB |

**Slope is flat at ~37 KB/program.** This is the *steady-state* per-program retained
heap when the env is shared. There is no asymptotic floor — the cost is genuinely
per-program, not amortized away.

### Scenario B vs A: per-env cost

Scenario A at N=1000 (1 env, 1000 progs): ~37 KB/prog → 36.6 MB total.
Scenario B at N=1000 (1000 envs, 1 prog each): 51.2 MB total → 51 KB / (env+prog).

Difference: **~14 KB per additional env.** The env brings ~14 KB of unique state
(separate function table copy from `cel.NewEnv`'s internal stdlib clone, plus
checker state).

### Scenario C: prediction vs actual

Model: `total ≈ 14 KB × envs + 37 KB × programs`

| Case | Predicted | Actual | Error |
|---|---|---|---|
| 100 envs × 5 progs (500 total) | 1.4 + 18.5 = 19.9 MB | 21.12 MB | +6% |
| 100 envs × 50 progs (5,000) | 1.4 + 185 = 186 MB | 189.91 MB | +2% |
| 1,000 envs × 5 progs (5,000) | 14 + 185 = 199 MB | 214.42 MB | +8% |
| 1,000 envs × 50 progs (50,000) | 14 + 1,850 = 1,864 MB | 1,907 MB | +2% |

The two-term linear model fits within ~8%. There is no missing nonlinear term.

## Implications for the patches

### What this *confirms*

1. **`cel.Program` cost is genuinely per-program, ~37 KB** in plain cel-go. In
   Kubernetes (more libraries → more FunctionDecls) it will be higher;
   pprof of a real apiserver suggested ~100–180 KB/program. Either way:
   **non-trivial and linear in unique program count.**

2. **Patch 0001 (program dedup by text) is the highest-leverage k8s-side
   change** by a wide margin. Every unique-text program costs ~37+ KB; every
   cache hit eliminates that cost.

3. **Patch B (shared `mustBuildEnvs`) helps a measurable but smaller term.**
   Per-env cost is ~14 KB. At 10,000 unique-shape policies that's ~140 MB —
   real but not the headline.

### What this *qualifies*

4. **Patch D (shared `Dispatcher` per env) — revised claim.** The pprof
   leaf-node ranking suggested ~62% of heap was per-program dispatcher state.
   The sweep shows the per-program cost is ~37 KB, of which the dispatcher is
   one component (alongside the `Interpretable` tree, attribute factory, etc.).
   Patch D would still help — likely saving 60–80% of the 37 KB per program,
   so ~22–30 KB per program — but the headline "62% of all heap" was an
   over-read of the pprof. **Realistic Patch D savings at 10,000 unique
   programs: ~250–350 MB.** Material, not transformative.

5. **The user's pushback was correct.** Saying "memory doesn't spike with
   unique expressions" is consistent with this data: 1,000 unique programs
   add ~37 MB. That's noticeable but easy to miss in a process whose total
   RSS is in the hundreds of MB. The bloat reporters see at scale comes
   from the *product* of policies × bindings × expressions, not from any one
   axis exploding.

### What the model predicts at issue-reporter scale

Reporter case: 1000 policies × 100 bindings × 50 expressions.

If expressions are *unique per policy* (worst case): 1000 × 50 = 50,000
unique programs → predicted ~1.85 GB just for programs. Plus per-policy env
overhead ~14 MB. **Total ~1.87 GB**, against the reported 7.1 GiB.

The remaining ~5 GB in the reporter's measurements is likely from per-binding
state (the dispatcher iterating bindings × matched-params), validator
allocations, and runtime Activation/lazy-map churn — not the compilation
subsystem this benchmark probes. Worth a separate investigation.

## How to interpret moving forward

- **Steady-state retained heap from CEL compilation: ~14 KB/env + ~37 KB/program**
  (in plain cel-go; ~3–5× higher in apiserver due to extra libraries).
- **0001 → Patch B → Patch D** is the right priority order, with 0001 the
  clear leader, B incremental, D limited by the actual per-program cost it
  can attack.
- **A and C are minor wins** that mostly help GC pressure and hot-path
  allocation rate, not steady-state RSS.
