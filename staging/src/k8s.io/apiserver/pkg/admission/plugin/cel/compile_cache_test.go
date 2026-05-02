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
	"testing"

	celgo "github.com/google/cel-go/cel"

	"k8s.io/apiserver/pkg/cel/environment"
)

// fakeAccessor is a minimal ExpressionAccessor for tests that doesn't drag in
// the validating package.
type fakeAccessor struct {
	expr     string
	rt       []*celgo.Type
	identity string
}

func (f *fakeAccessor) GetExpression() string    { return f.expr }
func (f *fakeAccessor) ReturnTypes() []*celgo.Type { return f.rt }

// newCompilerForTest returns a *compiler attached to the process-wide cache
// (i.e. with a non-nil templateID), so tests can verify cache behavior.
func newCompilerForTest(t *testing.T) Compiler {
	t.Helper()
	env := environment.MustBaseEnvSet(environment.DefaultCompatibilityVersion())
	return newCachedCompiler(env, env)
}

func TestCompileCache_DedupesIdenticalExpressions(t *testing.T) {
	restore := SetCompileCacheSizeForTests(64)
	defer restore()

	c := newCompilerForTest(t)
	a1 := &fakeAccessor{expr: "1 == 1", rt: []*celgo.Type{celgo.BoolType}, identity: "first"}
	a2 := &fakeAccessor{expr: "1 == 1", rt: []*celgo.Type{celgo.BoolType}, identity: "second"}

	r1 := c.CompileCELExpression(a1, OptionalVariableDeclarations{}, environment.StoredExpressions)
	if r1.Error != nil {
		t.Fatalf("first compile errored: %v", r1.Error)
	}
	r2 := c.CompileCELExpression(a2, OptionalVariableDeclarations{}, environment.StoredExpressions)
	if r2.Error != nil {
		t.Fatalf("second compile errored: %v", r2.Error)
	}

	// Same Program pointer means we hit the cache.
	if r1.Program == nil || r2.Program == nil {
		t.Fatalf("expected non-nil programs, got %v and %v", r1.Program, r2.Program)
	}
	// Cache holds the same Program; the wrapper struct is rebuilt per call.
	if got, want := GetCompileCacheStats().Hits, int64(1); got != want {
		t.Errorf("expected exactly %d cache hit, got %d (misses=%d)", want, got, GetCompileCacheStats().Misses)
	}

	// ExpressionAccessor is rebound to the caller's instance on a hit.
	if r2.ExpressionAccessor != a2 {
		t.Errorf("ExpressionAccessor not rebound on cache hit")
	}
}

func TestCompileCache_RespectsOptionalDecls(t *testing.T) {
	restore := SetCompileCacheSizeForTests(64)
	defer restore()

	c := newCompilerForTest(t)
	a := &fakeAccessor{expr: "object == oldObject", rt: []*celgo.Type{celgo.BoolType}}

	_ = c.CompileCELExpression(a, OptionalVariableDeclarations{HasParams: false}, environment.StoredExpressions)
	_ = c.CompileCELExpression(a, OptionalVariableDeclarations{HasParams: true}, environment.StoredExpressions)

	if got := GetCompileCacheStats().Hits; got != 0 {
		t.Errorf("expected 0 hits across different OptionalVariableDeclarations, got %d", got)
	}
	if got := GetCompileCacheStats().Misses; got != 2 {
		t.Errorf("expected 2 misses, got %d", got)
	}
}

func TestCompileCache_RespectsEnvType(t *testing.T) {
	restore := SetCompileCacheSizeForTests(64)
	defer restore()

	c := newCompilerForTest(t)
	a := &fakeAccessor{expr: "1 == 1", rt: []*celgo.Type{celgo.BoolType}}

	_ = c.CompileCELExpression(a, OptionalVariableDeclarations{}, environment.StoredExpressions)
	_ = c.CompileCELExpression(a, OptionalVariableDeclarations{}, environment.NewExpressions)

	if got := GetCompileCacheStats().Hits; got != 0 {
		t.Errorf("expected 0 hits across different envTypes, got %d", got)
	}
}

func TestCompileCache_DoesNotCacheCompilationErrors(t *testing.T) {
	restore := SetCompileCacheSizeForTests(64)
	defer restore()

	c := newCompilerForTest(t)
	a := &fakeAccessor{expr: "this is not valid CEL", rt: []*celgo.Type{celgo.BoolType}}

	r1 := c.CompileCELExpression(a, OptionalVariableDeclarations{}, environment.StoredExpressions)
	if r1.Error == nil {
		t.Fatalf("expected compile error, got success")
	}
	r2 := c.CompileCELExpression(a, OptionalVariableDeclarations{}, environment.StoredExpressions)
	if r2.Error == nil {
		t.Fatalf("expected compile error on second call too")
	}
	if got := GetCompileCacheStats().Hits; got != 0 {
		t.Errorf("expected 0 hits (errors must not be cached), got %d", got)
	}
	if got := GetCompileCacheStats().Misses; got != 2 {
		t.Errorf("expected 2 misses, got %d", got)
	}
}

func TestCompileCache_NormalizesLeadingTrailingWhitespace(t *testing.T) {
	restore := SetCompileCacheSizeForTests(64)
	defer restore()

	c := newCompilerForTest(t)
	r1 := c.CompileCELExpression(&fakeAccessor{expr: "1 == 1", rt: []*celgo.Type{celgo.BoolType}}, OptionalVariableDeclarations{}, environment.StoredExpressions)
	r2 := c.CompileCELExpression(&fakeAccessor{expr: "  1 == 1\n", rt: []*celgo.Type{celgo.BoolType}}, OptionalVariableDeclarations{}, environment.StoredExpressions)
	if r1.Error != nil || r2.Error != nil {
		t.Fatalf("unexpected errors: %v %v", r1.Error, r2.Error)
	}
	if got := GetCompileCacheStats().Hits; got != 1 {
		t.Errorf("expected 1 cache hit (whitespace-only difference), got %d", got)
	}
}

func TestCompileCache_BoundedSize(t *testing.T) {
	restore := SetCompileCacheSizeForTests(2)
	defer restore()

	c := newCompilerForTest(t)
	exprs := []string{"1 == 1", "2 == 2", "3 == 3"}
	for _, e := range exprs {
		r := c.CompileCELExpression(&fakeAccessor{expr: e, rt: []*celgo.Type{celgo.BoolType}}, OptionalVariableDeclarations{}, environment.StoredExpressions)
		if r.Error != nil {
			t.Fatalf("compile error for %q: %v", e, r.Error)
		}
	}
	if got, want := compileCache().Len(), 2; got != want {
		t.Errorf("cache size: got %d, want %d", got, want)
	}
}
