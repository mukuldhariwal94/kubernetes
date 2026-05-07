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

package validating

import (
	"runtime"
	"testing"

	v1 "k8s.io/api/admissionregistration/v1"
)

// This file isolates the *non-CEL* per-policy overhead in compilePolicy:
// every byte allocated for a policy that has zero matchConditions, zero
// validations, zero auditAnnotations, zero messageExpressions, and zero
// variables. Anything left is pure framework overhead — composited
// compiler construction, varEnvs matrix build, convertv1* slices, the
// four CompileCondition wrappers, and the validator struct itself.
//
// Run:
//   go test -run='^$' -bench='BenchmarkPerPolicyOverhead' \
//     -benchmem -benchtime=200x -count=5 \
//     ./staging/src/k8s.io/apiserver/pkg/admission/plugin/policy/validating/...
//
// Steady-state retained heap per policy:
//   go test -run='TestRetainedHeapPerEmptyPolicy' -v \
//     ./staging/src/k8s.io/apiserver/pkg/admission/plugin/policy/validating/...

func emptyPolicy() *Policy {
	return &v1.ValidatingAdmissionPolicy{
		Spec: v1.ValidatingAdmissionPolicySpec{
			// All slices are nil. ParamKind nil. FailurePolicy nil.
			// MatchConstraints required by API validation but irrelevant to compile.
			MatchConstraints: &v1.MatchResources{},
		},
	}
}

func minimalPolicy() *Policy {
	failType := v1.Fail
	return &v1.ValidatingAdmissionPolicy{
		Spec: v1.ValidatingAdmissionPolicySpec{
			FailurePolicy:    &failType,
			MatchConstraints: &v1.MatchResources{},
			Validations: []v1.Validation{
				{Expression: "true"},
			},
		},
	}
}

func BenchmarkPerPolicyOverhead_Empty(b *testing.B) {
	p := emptyPolicy()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		v := compilePolicy(p)
		runtime.KeepAlive(v)
	}
}

func BenchmarkPerPolicyOverhead_OneValidation(b *testing.B) {
	p := minimalPolicy()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		v := compilePolicy(p)
		runtime.KeepAlive(v)
	}
}

// TestRetainedHeapPerEmptyPolicy compiles N empty policies, retains all
// of them in a slice, then triple-GCs and reports the per-policy
// retained delta in HeapInuse. Run with -v.
func TestRetainedHeapPerEmptyPolicy(t *testing.T) {
	const N = 5000
	p := emptyPolicy()

	runtime.GC()
	runtime.GC()
	var before runtime.MemStats
	runtime.ReadMemStats(&before)

	retained := make([]Validator, 0, N)
	for i := 0; i < N; i++ {
		retained = append(retained, compilePolicy(p))
	}

	runtime.GC()
	runtime.GC()
	runtime.GC()
	var after runtime.MemStats
	runtime.ReadMemStats(&after)

	delta := after.HeapInuse - before.HeapInuse
	perPolicy := float64(delta) / float64(N)

	t.Logf("policies retained:        %d", N)
	t.Logf("HeapInuse delta:          %.2f MiB", float64(delta)/(1024*1024))
	t.Logf("per-policy retained:      %.0f bytes  (~%.1f KiB)", perPolicy, perPolicy/1024)
	t.Logf("HeapObjects delta:        %d", after.HeapObjects-before.HeapObjects)
	t.Logf("HeapObjects/policy:       %.1f", float64(after.HeapObjects-before.HeapObjects)/float64(N))
	t.Logf("Mallocs delta:            %d", after.Mallocs-before.Mallocs)
	t.Logf("Mallocs/policy:           %.1f", float64(after.Mallocs-before.Mallocs)/float64(N))

	runtime.KeepAlive(retained)
}

// TestRetainedHeapPerOneValidation compiles N minimal policies (one
// trivial validation) and reports the same metric. The delta vs the
// empty case isolates the cost of a single "true" expression compile.
func TestRetainedHeapPerOneValidation(t *testing.T) {
	const N = 5000
	p := minimalPolicy()

	runtime.GC()
	runtime.GC()
	var before runtime.MemStats
	runtime.ReadMemStats(&before)

	retained := make([]Validator, 0, N)
	for i := 0; i < N; i++ {
		retained = append(retained, compilePolicy(p))
	}

	runtime.GC()
	runtime.GC()
	runtime.GC()
	var after runtime.MemStats
	runtime.ReadMemStats(&after)

	delta := after.HeapInuse - before.HeapInuse
	perPolicy := float64(delta) / float64(N)

	t.Logf("policies retained:        %d (each with 1 validation 'true')", N)
	t.Logf("HeapInuse delta:          %.2f MiB", float64(delta)/(1024*1024))
	t.Logf("per-policy retained:      %.0f bytes  (~%.1f KiB)", perPolicy, perPolicy/1024)
	t.Logf("HeapObjects delta:        %d", after.HeapObjects-before.HeapObjects)
	t.Logf("HeapObjects/policy:       %.1f", float64(after.HeapObjects-before.HeapObjects)/float64(N))

	runtime.KeepAlive(retained)
}
