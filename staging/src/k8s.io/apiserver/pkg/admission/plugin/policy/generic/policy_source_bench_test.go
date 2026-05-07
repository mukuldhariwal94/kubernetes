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

package generic

import (
	goruntime "runtime"
	"strconv"
	"testing"

	v1 "k8s.io/api/admissionregistration/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
)

// This file isolates the steady-state per-refresh cost in
// policy_source.go that has nothing to do with CEL: the cache-hit path
// in compilePolicyLocked, which is called once per policy on every
// refreshPolicies tick (1 Hz when policiesDirty is set).
//
// The hot-path operation is meta.Accessor(policySpec) which uses
// reflection to extract Namespace / Name / ResourceVersion. For an
// already-compiled, unchanged policy this is the entire cost of the
// "cached" branch — the rest is a map lookup and a string compare.
//
// At N=10000 policies this runs N times per refresh tick. We measure
// the per-policy bytes and ns to quantify the steady-state CPU and
// allocation pressure on the apiserver, independent of CEL.

// minimalPolicy is a real *v1.ValidatingAdmissionPolicy with just enough
// metadata for meta.Accessor to extract Namespace / Name / RV.
type benchPolicy = v1.ValidatingAdmissionPolicy

// benchAccessor wraps a *benchPolicy with a minimal PolicyAccessor impl.
type benchAccessor struct{ p *benchPolicy }

func (a *benchAccessor) GetName() string                         { return a.p.Name }
func (a *benchAccessor) GetNamespace() string                    { return a.p.Namespace }
func (a *benchAccessor) GetParamKind() *v1.ParamKind             { return nil }
func (a *benchAccessor) GetMatchConstraints() *v1.MatchResources { return nil }
func (a *benchAccessor) GetFailurePolicy() *v1.FailurePolicyType { return nil }

// fakeEvaluator is a no-op Evaluator so the compiler callback can be a noop.
type fakeEvaluator struct{}

// fakeEvaluator is the bare Evaluator interface (an empty marker
// interface in the generic package) — no methods needed.
var _ Evaluator = fakeEvaluator{}

// makeBenchSource builds a policySource with N policies pre-cached, so
// that compilePolicyLocked always takes the cache-hit branch.
func makeBenchSource(n int) (*policySource[*benchPolicy, *v1.ValidatingAdmissionPolicyBinding, fakeEvaluator], []*benchPolicy) {
	s := &policySource[*benchPolicy, *v1.ValidatingAdmissionPolicyBinding, fakeEvaluator]{
		compiler:           func(p *benchPolicy) fakeEvaluator { return fakeEvaluator{} },
		newPolicyAccessor:  func(p *benchPolicy) PolicyAccessor { return &benchAccessor{p: p} },
		compiledPolicies:   make(map[types.NamespacedName]compiledPolicyEntry[fakeEvaluator], n),
	}
	policies := make([]*benchPolicy, n)
	for i := 0; i < n; i++ {
		p := &benchPolicy{
			ObjectMeta: metav1.ObjectMeta{
				Name:            "policy-" + strconv.Itoa(i),
				ResourceVersion: "1",
			},
		}
		policies[i] = p
		key := types.NamespacedName{Name: p.Name}
		s.compiledPolicies[key] = compiledPolicyEntry[fakeEvaluator]{
			policyVersion: "1",
			evaluator:     fakeEvaluator{},
		}
	}
	return s, policies
}

// BenchmarkCompilePolicyLocked_CacheHit measures the steady-state cost
// of compilePolicyLocked on cache hits. This is what runs N times on
// every refreshPolicies tick when no policy has changed.
func BenchmarkCompilePolicyLocked_CacheHit(b *testing.B) {
	const N = 1
	s, policies := makeBenchSource(N)
	p := policies[0]
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = s.compilePolicyLocked(p)
	}
	goruntime.KeepAlive(s)
}

// BenchmarkCompilePolicyLocked_FullSweep_10k measures cumulative cost
// of one full "refresh" pass over 10000 cached policies — the practical
// per-tick cost at extreme scale, with no policy changes.
func BenchmarkCompilePolicyLocked_FullSweep_10k(b *testing.B) {
	const N = 10000
	s, policies := makeBenchSource(N)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		for j := 0; j < N; j++ {
			_ = s.compilePolicyLocked(policies[j])
		}
	}
	goruntime.KeepAlive(s)
}

// BenchmarkMetaAccessorOnly isolates just the reflection cost of
// meta.Accessor on a typed *ValidatingAdmissionPolicy. This is the
// dominant per-policy cost inside compilePolicyLocked's cache-hit
// path.
func BenchmarkMetaAccessorOnly(b *testing.B) {
	p := &benchPolicy{
		ObjectMeta: metav1.ObjectMeta{
			Name:            "policy-1",
			ResourceVersion: "1",
		},
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		m, err := metaAccessor(p)
		if err != nil {
			b.Fatal(err)
		}
		_ = m.GetName()
		_ = m.GetNamespace()
		_ = m.GetResourceVersion()
	}
}

// metaAccessor is just a re-export of meta.Accessor for the bench.
func metaAccessor(o runtime.Object) (metaInterface, error) {
	m, err := meta.Accessor(o)
	if err != nil {
		return nil, err
	}
	return m, nil
}

type metaInterface interface {
	GetName() string
	GetNamespace() string
	GetResourceVersion() string
}

// BenchmarkBindingAccessorAlloc measures the per-binding allocation cost
// of the current "wrap each binding in a fresh accessor" pattern in
// calculatePolicyData. This runs N times per refresh tick where N =
// number of bindings.
func BenchmarkBindingAccessorAlloc(b *testing.B) {
	// Mirror the production wrapper: a single-field struct that delegates.
	type bindingAccessor struct {
		obj *v1.ValidatingAdmissionPolicyBinding
	}
	binding := &v1.ValidatingAdmissionPolicyBinding{
		ObjectMeta: metav1.ObjectMeta{Name: "binding-1"},
		Spec:       v1.ValidatingAdmissionPolicyBindingSpec{PolicyName: "policy-1"},
	}
	newBindingAccessor := func(b *v1.ValidatingAdmissionPolicyBinding) any {
		return &bindingAccessor{obj: b}
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = newBindingAccessor(binding)
	}
}

// BenchmarkRefreshSimulated_100x100 measures a multi-binding-per-policy
// shape: 100 policies × 100 bindings each = 10000 bindings, where the
// over-sized result slice (sized to len(bindingList) instead of
// len(policiesToBindings)) wastes memory.
func BenchmarkRefreshSimulated_100x100(b *testing.B) {
	const policyCount, bindingsPerPolicy = 100, 100
	bindings := make([]*v1.ValidatingAdmissionPolicyBinding, 0, policyCount*bindingsPerPolicy)
	policies := make([]*v1.ValidatingAdmissionPolicy, policyCount)
	for i := 0; i < policyCount; i++ {
		name := "policy-" + strconv.Itoa(i)
		policies[i] = &v1.ValidatingAdmissionPolicy{
			ObjectMeta: metav1.ObjectMeta{Name: name, ResourceVersion: "1"},
		}
		for j := 0; j < bindingsPerPolicy; j++ {
			bindings = append(bindings, &v1.ValidatingAdmissionPolicyBinding{
				ObjectMeta: metav1.ObjectMeta{Name: "binding-" + strconv.Itoa(i) + "-" + strconv.Itoa(j)},
				Spec:       v1.ValidatingAdmissionPolicyBindingSpec{PolicyName: name},
			})
		}
	}
	b.ReportAllocs()
	b.ResetTimer()
	for n := 0; n < b.N; n++ {
		policiesToBindings := map[types.NamespacedName][]*v1.ValidatingAdmissionPolicyBinding{}
		for _, bs := range bindings {
			key := types.NamespacedName{Name: bs.Spec.PolicyName}
			policiesToBindings[key] = append(policiesToBindings[key], bs)
		}
		// CURRENT production sizing: capacity = len(bindingList).
		result := make([]PolicyHook[*v1.ValidatingAdmissionPolicy, *v1.ValidatingAdmissionPolicyBinding, fakeEvaluator], 0, len(bindings))
		for _, p := range policies {
			result = append(result, PolicyHook[*v1.ValidatingAdmissionPolicy, *v1.ValidatingAdmissionPolicyBinding, fakeEvaluator]{
				Policy:   p,
				Bindings: policiesToBindings[types.NamespacedName{Name: p.Name}],
			})
		}
		goruntime.KeepAlive(result)
	}
}

// BenchmarkRefreshSimulated_100x100_PreSized variant with the proposed
// fix — capacity = len(policiesToBindings) (i.e. unique-policy count).
func BenchmarkRefreshSimulated_100x100_PreSized(b *testing.B) {
	const policyCount, bindingsPerPolicy = 100, 100
	bindings := make([]*v1.ValidatingAdmissionPolicyBinding, 0, policyCount*bindingsPerPolicy)
	policies := make([]*v1.ValidatingAdmissionPolicy, policyCount)
	for i := 0; i < policyCount; i++ {
		name := "policy-" + strconv.Itoa(i)
		policies[i] = &v1.ValidatingAdmissionPolicy{
			ObjectMeta: metav1.ObjectMeta{Name: name, ResourceVersion: "1"},
		}
		for j := 0; j < bindingsPerPolicy; j++ {
			bindings = append(bindings, &v1.ValidatingAdmissionPolicyBinding{
				ObjectMeta: metav1.ObjectMeta{Name: "binding-" + strconv.Itoa(i) + "-" + strconv.Itoa(j)},
				Spec:       v1.ValidatingAdmissionPolicyBindingSpec{PolicyName: name},
			})
		}
	}
	b.ReportAllocs()
	b.ResetTimer()
	for n := 0; n < b.N; n++ {
		policiesToBindings := map[types.NamespacedName][]*v1.ValidatingAdmissionPolicyBinding{}
		for _, bs := range bindings {
			key := types.NamespacedName{Name: bs.Spec.PolicyName}
			policiesToBindings[key] = append(policiesToBindings[key], bs)
		}
		// PROPOSED sizing: capacity = unique-policy count.
		result := make([]PolicyHook[*v1.ValidatingAdmissionPolicy, *v1.ValidatingAdmissionPolicyBinding, fakeEvaluator], 0, len(policiesToBindings))
		for _, p := range policies {
			result = append(result, PolicyHook[*v1.ValidatingAdmissionPolicy, *v1.ValidatingAdmissionPolicyBinding, fakeEvaluator]{
				Policy:   p,
				Bindings: policiesToBindings[types.NamespacedName{Name: p.Name}],
			})
		}
		goruntime.KeepAlive(result)
	}
}

// BenchmarkRefreshSimulated_10k_NoChange measures the per-refresh
// allocation cost at 10000 bindings + 10000 policies (1 binding per
// policy) when nothing has changed. Simulates the loop body in
// calculatePolicyData (line 296-302 + 307-364) without the informer
// List() cost.
func BenchmarkRefreshSimulated_10k_NoChange(b *testing.B) {
	const N = 10000
	bindings := make([]*v1.ValidatingAdmissionPolicyBinding, N)
	policies := make([]*v1.ValidatingAdmissionPolicy, N)
	for i := 0; i < N; i++ {
		name := "policy-" + strconv.Itoa(i)
		policies[i] = &v1.ValidatingAdmissionPolicy{
			ObjectMeta: metav1.ObjectMeta{Name: name, ResourceVersion: "1"},
		}
		bindings[i] = &v1.ValidatingAdmissionPolicyBinding{
			ObjectMeta: metav1.ObjectMeta{Name: "binding-" + strconv.Itoa(i)},
			Spec:       v1.ValidatingAdmissionPolicyBindingSpec{PolicyName: name},
		}
	}

	// Production wrapper types.
	type bindingAccessor struct {
		obj *v1.ValidatingAdmissionPolicyBinding
	}
	type policyAccessor struct {
		obj *v1.ValidatingAdmissionPolicy
	}
	getPolicyName := func(b *v1.ValidatingAdmissionPolicyBinding) types.NamespacedName {
		acc := &bindingAccessor{obj: b}
		return types.NamespacedName{Name: acc.obj.Spec.PolicyName}
	}
	getPolicyAccessor := func(p *v1.ValidatingAdmissionPolicy) any {
		return &policyAccessor{obj: p}
	}

	b.ReportAllocs()
	b.ResetTimer()
	for n := 0; n < b.N; n++ {
		// Simulates calculatePolicyData per-refresh body (no informer List).
		policiesToBindings := map[types.NamespacedName][]*v1.ValidatingAdmissionPolicyBinding{}
		for _, bs := range bindings {
			key := getPolicyName(bs)
			policiesToBindings[key] = append(policiesToBindings[key], bs)
		}
		result := make([]PolicyHook[*v1.ValidatingAdmissionPolicy, *v1.ValidatingAdmissionPolicyBinding, fakeEvaluator], 0, len(bindings))
		for _, p := range policies {
			_ = getPolicyAccessor(p)
			result = append(result, PolicyHook[*v1.ValidatingAdmissionPolicy, *v1.ValidatingAdmissionPolicyBinding, fakeEvaluator]{
				Policy:   p,
				Bindings: policiesToBindings[types.NamespacedName{Name: p.Name}],
			})
		}
		goruntime.KeepAlive(result)
	}
}
