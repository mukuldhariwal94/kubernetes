// Memory-sweep benchmark for cel.Program retention.
//
// Question this answers: when you build N programs from M envs, what is the
// per-program incremental retained heap? Specifically:
//
//   - Scenario A: 1 env, N unique-text programs.
//       → measures the per-program cost when the env is shared.
//       → if FunctionDecl.Bindings / Dispatcher are pointer-shared from the
//         env (my revised hypothesis), this slope is small (~1–5 KB/prog).
//       → if they are deep-copied per-program (the original Patch D claim),
//         this slope is large (~100+ KB/prog).
//
//   - Scenario B: N envs, 1 program each (all unique text).
//       → measures the per-env cost.
//       → this should be the fixed-cost-heavy scenario.
//
//   - Scenario C: N envs × M programs each.
//       → models the VAP-at-scale case (one env per "shape", many programs).
//
// Run from the kubernetes repo root so the vendored cel-go is picked up:
//
//   go run ./issue-131417/proposed-patches/sweep_bench
//
// Example expected output (illustrative — your numbers may differ):
//
//   --- Scenario A: 1 env, N unique programs ---
//      N    HeapInuse delta    bytes/prog
//    100             1.8 MB       18,000
//    500             8.7 MB       17,500
//   1000            17.2 MB       17,200
//   5000            85.1 MB       17,400
//
// If bytes/prog stays roughly flat across N, the cost is genuinely per-program.
// If it shrinks toward a floor as N grows, there's amortizable shared state.

package main

import (
	"fmt"
	"runtime"
	"runtime/debug"
	"strings"

	"github.com/google/cel-go/cel"
)

func main() {
	// Disable GC during measurement windows for reproducibility, then re-enable
	// and force collection at sample points.
	debug.SetGCPercent(-1)

	fmt.Println("=== cel-go program retention sweep ===")
	fmt.Println("(values measured as HeapInuse delta after runtime.GC × 3)")
	fmt.Println()

	scenarioA()
	scenarioB()
	scenarioC()
}

// buildEnv builds a base env roughly comparable in feature surface to what
// kube-apiserver constructs for VAP (no Kubernetes-specific libraries — keeping
// it portable; the *shape* of the question is the same).
func buildEnv() (*cel.Env, error) {
	return cel.NewEnv(
		cel.Variable("x", cel.IntType),
		cel.Variable("y", cel.IntType),
		cel.Variable("s", cel.StringType),
	)
}

// genExpr returns a structurally simple but textually unique expression.
// Distinct text means env.Compile produces a distinct AST; the question is
// whether env.Program produces a distinct heap footprint of any meaningful
// size on top of that.
func genExpr(i int) string {
	return fmt.Sprintf("(x + %d) * (y + %d) + size(s)", i, i*7+13)
}

func sample(label string, work func() any) {
	runtime.GC()
	runtime.GC()
	runtime.GC()
	var before runtime.MemStats
	runtime.ReadMemStats(&before)

	keep := work()

	runtime.GC()
	runtime.GC()
	runtime.GC()
	var after runtime.MemStats
	runtime.ReadMemStats(&after)

	delta := int64(after.HeapInuse) - int64(before.HeapInuse)
	fmt.Printf("  %-40s  HeapInuse delta=%10s  HeapObjects delta=%d\n",
		label, humanBytes(delta), int64(after.HeapObjects)-int64(before.HeapObjects))

	runtime.KeepAlive(keep)
}

func humanBytes(n int64) string {
	const k = 1024
	abs := n
	if abs < 0 {
		abs = -abs
	}
	switch {
	case abs >= k*k*k:
		return fmt.Sprintf("%.2f GB", float64(n)/float64(k*k*k))
	case abs >= k*k:
		return fmt.Sprintf("%.2f MB", float64(n)/float64(k*k))
	case abs >= k:
		return fmt.Sprintf("%.2f KB", float64(n)/float64(k))
	default:
		return fmt.Sprintf("%d B", n)
	}
}

// ---------------------------------------------------------------------------
// Scenario A — 1 env, N unique-text programs.
// ---------------------------------------------------------------------------
func scenarioA() {
	fmt.Println("--- Scenario A: 1 env, N unique-text programs ---")
	fmt.Println("    (probes per-program cost when env is shared)")
	fmt.Println()

	env, err := buildEnv()
	if err != nil {
		panic(err)
	}

	for _, n := range []int{100, 500, 1000, 5000, 10000} {
		sample(fmt.Sprintf("N=%-6d", n), func() any {
			progs := make([]cel.Program, 0, n)
			for i := 0; i < n; i++ {
				ast, iss := env.Compile(genExpr(i))
				if iss != nil && iss.Err() != nil {
					panic(iss.Err())
				}
				prog, err := env.Program(ast)
				if err != nil {
					panic(err)
				}
				progs = append(progs, prog)
			}
			return progs
		})
	}
	fmt.Println()
}

// ---------------------------------------------------------------------------
// Scenario B — N envs, 1 program each.
// ---------------------------------------------------------------------------
func scenarioB() {
	fmt.Println("--- Scenario B: N envs (each separately constructed), 1 program each ---")
	fmt.Println("    (probes per-env cost; this is what mustBuildEnvs / NewCompositedCompiler scale on)")
	fmt.Println()

	for _, n := range []int{10, 100, 500, 1000, 2000} {
		sample(fmt.Sprintf("N=%-6d", n), func() any {
			type pair struct {
				env  *cel.Env
				prog cel.Program
			}
			out := make([]pair, 0, n)
			for i := 0; i < n; i++ {
				env, err := buildEnv()
				if err != nil {
					panic(err)
				}
				ast, iss := env.Compile(genExpr(i))
				if iss != nil && iss.Err() != nil {
					panic(iss.Err())
				}
				prog, err := env.Program(ast)
				if err != nil {
					panic(err)
				}
				out = append(out, pair{env, prog})
			}
			return out
		})
	}
	fmt.Println()
}

// ---------------------------------------------------------------------------
// Scenario C — N envs × M programs each (the VAP-at-scale model).
// ---------------------------------------------------------------------------
func scenarioC() {
	fmt.Println("--- Scenario C: N envs × M programs each (VAP-at-scale model) ---")
	fmt.Println("    (combines per-env and per-program costs)")
	fmt.Println()

	cases := []struct{ envs, progsPerEnv int }{
		{100, 5},
		{100, 50},
		{1000, 5},
		{1000, 50},
	}
	for _, c := range cases {
		label := fmt.Sprintf("envs=%-4d progs/env=%-3d  total=%-6d",
			c.envs, c.progsPerEnv, c.envs*c.progsPerEnv)
		sample(label, func() any {
			type bundle struct {
				env   *cel.Env
				progs []cel.Program
			}
			out := make([]bundle, 0, c.envs)
			for i := 0; i < c.envs; i++ {
				env, err := buildEnv()
				if err != nil {
					panic(err)
				}
				progs := make([]cel.Program, 0, c.progsPerEnv)
				for j := 0; j < c.progsPerEnv; j++ {
					ast, iss := env.Compile(genExpr(i*c.progsPerEnv + j))
					if iss != nil && iss.Err() != nil {
						panic(iss.Err())
					}
					prog, err := env.Program(ast)
					if err != nil {
						panic(err)
					}
					progs = append(progs, prog)
				}
				out = append(out, bundle{env, progs})
			}
			return out
		})
	}
	fmt.Println()

	_ = strings.TrimSpace // silence unused import if I trim things later
}
