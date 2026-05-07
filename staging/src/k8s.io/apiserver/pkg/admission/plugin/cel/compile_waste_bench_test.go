/*
Copyright 2026 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package cel

import (
	"runtime"
	"testing"

	"github.com/google/cel-go/cel"

	"k8s.io/apiserver/pkg/cel/environment"
)

// Isolates two specific waste sites in CompileCondition /
// CompileCELExpression that have nothing to do with the cel.Program
// retention discussed elsewhere in issue-131417:
//
//   1. The AstToCheckedExpr discard at compile.go:348-352. Walks the
//      whole AST, builds a full *exprpb.CheckedExpr proto tree
//      (refMap + typeMap + recursive ExprToProto), throws away
//      everything but the error.
//   2. ReturnTypes() implementations on every ExpressionAccessor
//      (ValidationCondition, MessageExpressionCondition,
//      AuditAnnotationCondition, MatchCondition, Variable, celExpression)
//      that return a fresh slice literal on every call.

// astToCheckedExprDiscardBench compiles an AST and then performs the
// same defensive AstToCheckedExpr call that compileFresh makes, just
// to measure that single line's cost.
func astToCheckedExprDiscardBench(b *testing.B, expr string) {
	// Use the same production env shape (with object / oldObject /
	// namespaceObject / request declared) that compileFresh would.
	baseEnvSet := environment.MustBaseEnvSet(environment.DefaultCompatibilityVersion())
	envSet, err := createEnvForOpts(baseEnvSet, BuildNamespaceType(), BuildRequestType(),
		OptionalVariableDeclarations{HasParams: false, HasAuthorizer: true})
	if err != nil {
		b.Fatal(err)
	}
	env, err := envSet.Env(environment.NewExpressions)
	if err != nil {
		b.Fatal(err)
	}
	ast, issues := env.Compile(expr)
	if issues != nil && issues.Err() != nil {
		b.Fatalf("compile: %v", issues.Err())
	}
	runtime.GC()
	var beforeMS runtime.MemStats
	runtime.ReadMemStats(&beforeMS)

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		// Same call as compile.go:348 — result discarded.
		_, err := cel.AstToCheckedExpr(ast)
		if err != nil {
			b.Fatal(err)
		}
	}
	b.StopTimer()

	runtime.GC()
	var afterMS runtime.MemStats
	runtime.ReadMemStats(&afterMS)
	b.ReportMetric(float64(afterMS.HeapInuse-beforeMS.HeapInuse), "heap-delta-B")
}

func BenchmarkAstToCheckedExpr_Discard_Trivial(b *testing.B) {
	astToCheckedExprDiscardBench(b, `true`)
}

func BenchmarkAstToCheckedExpr_Discard_Simple(b *testing.B) {
	astToCheckedExprDiscardBench(b, `"foo" == "bar"`)
}

func BenchmarkAstToCheckedExpr_Discard_Realistic(b *testing.B) {
	astToCheckedExprDiscardBench(b,
		`object.spec.replicas <= 5 && object.metadata.name.startsWith("prod-") && size(object.spec.containers) > 0`)
}

func BenchmarkAstToCheckedExpr_Discard_Complex(b *testing.B) {
	astToCheckedExprDiscardBench(b, `
		object.spec.containers.all(c, c.resources.limits.memory != "" &&
		    c.resources.limits.cpu != "" &&
		    c.image.startsWith("registry.example.com/") &&
		    !(c.name in ["root", "admin"]) &&
		    size(c.env) <= 50 &&
		    c.imagePullPolicy in ["IfNotPresent", "Always"]
		) && object.metadata.labels.size() < 64`)
}

// returnTypesAllocBench measures the per-call allocation of the
// existing ReturnTypes() pattern: `return []*cel.Type{cel.BoolType}`
// vs a package-level singleton.

var (
	freshReturnBool   = func() []*cel.Type { return []*cel.Type{cel.BoolType} }
	cachedReturnBool  = []*cel.Type{cel.BoolType}
	cachedReturnFunc  = func() []*cel.Type { return cachedReturnBool }
)

func BenchmarkReturnTypes_FreshSliceLiteral(b *testing.B) {
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_ = freshReturnBool()
	}
}

func BenchmarkReturnTypes_PackageSingleton(b *testing.B) {
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_ = cachedReturnFunc()
	}
}

// boolAccessor is a minimal ExpressionAccessor whose ReturnTypes
// returns a fresh slice literal every call, mirroring the production
// implementations on *ValidationCondition / *MatchCondition / etc.
type boolAccessor struct{ expr string }

func (a *boolAccessor) GetExpression() string  { return a.expr }
func (a *boolAccessor) ReturnTypes() []*cel.Type {
	return []*cel.Type{cel.BoolType}
}

// BenchmarkCompileFresh_End2End measures the full per-expression
// compile path (env load + env.Compile + AstToCheckedExpr discard +
// env.Program). Use to compare against the post-patch numbers.
func BenchmarkCompileFresh_End2End(b *testing.B) {
	for _, tc := range []struct {
		name string
		expr string
	}{
		{"Trivial", `true`},
		{"Simple", `"foo" == "bar"`},
		{"Realistic", `object.spec.replicas <= 5 && object.metadata.name.startsWith("prod-") && size(object.spec.containers) > 0`},
		{"Complex", `object.spec.containers.all(c, c.resources.limits.memory != "" && c.resources.limits.cpu != "" && c.image.startsWith("registry.example.com/") && !(c.name in ["root", "admin"]) && size(c.env) <= 50 && c.imagePullPolicy in ["IfNotPresent", "Always"]) && object.metadata.labels.size() < 64`},
	} {
		b.Run(tc.name, func(b *testing.B) {
			baseEnvSet := environment.MustBaseEnvSet(environment.DefaultCompatibilityVersion())
			c := &compiler{varEnvs: newVariableDeclEnvs(baseEnvSet)}
			acc := &boolAccessor{expr: tc.expr}
			opts := OptionalVariableDeclarations{HasParams: false, HasAuthorizer: true}
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				_ = c.compileFresh(acc, opts, environment.NewExpressions)
			}
		})
	}
}
