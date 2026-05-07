/*
Copyright 2026 The Kubernetes Authors.
Licensed under the Apache License, Version 2.0.
*/

// Walkthrough test: a single, debuggable end-to-end trace of how a
// ValidatingAdmissionPolicy gets:
//
//   Step 1.  compiled (validating.compilePolicy)
//   Step 2.  loaded into the generic.policySource and stored into
//            policies.Store atomically
//   Step 3.  evaluated by the validator on a synthetic admission request
//
// Each step is a separate sub-test so you can run them individually under
// dlv / your IDE debugger:
//
//   go test -v -run TestVAPWalkthrough/Step1 ./staging/src/k8s.io/apiserver/pkg/admission/plugin/policy/validating/
//   go test -v -run TestVAPWalkthrough/Step2 ./staging/src/k8s.io/apiserver/pkg/admission/plugin/policy/validating/
//   go test -v -run TestVAPWalkthrough/Step3 ./staging/src/k8s.io/apiserver/pkg/admission/plugin/policy/validating/
//
// Recommended breakpoints for each step are marked inline as `BREAKPOINT:`.
// The test uses no mocks for the CEL pipeline — it goes through the real
// cel.NewCompositedCompiler / mustBuildEnvs / env.Compile / env.Program path.

package validating

import (
	"context"
	"fmt"
	"testing"
	"time"

	v1 "k8s.io/api/admissionregistration/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/runtime/serializer"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/apiserver/pkg/admission"
	"k8s.io/apiserver/pkg/admission/plugin/policy/generic"
	celconfig "k8s.io/apiserver/pkg/apis/cel"
	"k8s.io/client-go/dynamic"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes/fake"
	clienttesting "k8s.io/client-go/testing"
	"k8s.io/client-go/tools/cache"
)

// ---------------------------------------------------------------------------
// Synthetic policy used by every step.
// ---------------------------------------------------------------------------

// makePolicy returns a hand-built VAP whose validation should:
//   - admit requests where object.metadata.name starts with "good-"
//   - deny everything else with the message "name must start with good-"
//
// It uses a `variables` block so we exercise CompositedCompiler.
func makePolicy() *Policy {
	failPolicy := v1.Fail
	return &v1.ValidatingAdmissionPolicy{
		ObjectMeta: metav1.ObjectMeta{
			Name:            "walkthrough-policy",
			ResourceVersion: "1",
			UID:             types.UID("walkthrough-policy-uid"),
		},
		Spec: v1.ValidatingAdmissionPolicySpec{
			FailurePolicy: &failPolicy,
			MatchConstraints: &v1.MatchResources{
				ResourceRules: []v1.NamedRuleWithOperations{{
					RuleWithOperations: v1.RuleWithOperations{
						Operations: []v1.OperationType{v1.Create, v1.Update},
						Rule: v1.Rule{
							APIGroups:   []string{""},
							APIVersions: []string{"v1"},
							Resources:   []string{"configmaps"},
						},
					},
				}},
			},
			Variables: []v1.Variable{
				{Name: "objName", Expression: "object.metadata.name"},
				{Name: "isGood", Expression: "variables.objName.startsWith('good-')"},
			},
			Validations: []v1.Validation{
				{
					Expression: "variables.isGood",
					Message:    "name must start with good-",
				},
			},
			MatchConditions: []v1.MatchCondition{
				{
					Name:       "skip-empty-name",
					Expression: "object.metadata.name != ''",
				},
			},
		},
	}
}

// makeBinding returns a hand-built VAPB pointing at the policy with Deny action.
func makeBinding() *PolicyBinding {
	return &v1.ValidatingAdmissionPolicyBinding{
		ObjectMeta: metav1.ObjectMeta{
			Name:            "walkthrough-binding",
			ResourceVersion: "1",
			UID:             types.UID("walkthrough-binding-uid"),
		},
		Spec: v1.ValidatingAdmissionPolicyBindingSpec{
			PolicyName:        "walkthrough-policy",
			ValidationActions: []v1.ValidationAction{v1.Deny},
		},
	}
}

// ---------------------------------------------------------------------------
// TestVAPWalkthrough — three sub-tests, one per layer.
// ---------------------------------------------------------------------------

func TestVAPWalkthrough(t *testing.T) {
	t.Run("Step1_CompilePolicy", testStep1CompilePolicy)
	t.Run("Step2_PolicySource_BuildAndStore", testStep2PolicySourceBuildAndStore)
	t.Run("Step3_Validator_Evaluate", testStep3ValidatorEvaluate)
}

// ---------------------------------------------------------------------------
// STEP 1 — Direct call into validating.compilePolicy.
//
// What this exercises (set breakpoints in these files to step through):
//
//   1. validating/plugin.go              compilePolicy(policy)
//   2. cel/composition.go                NewCompositedCompiler  → Extend({variables})
//   3. cel/compile.go                    mustBuildEnvs           → 8× createEnvForOpts
//                                                                → 16× cel.Env.Extend
//   4. cel/composition.go                CompileAndStoreVariables (per variable)
//        cel/compile.go                    CompileCELExpression  → compileFresh
//          cel-go cel/env.go                 Env.Compile         (parse + check)
//          cel-go cel/program.go             Env.Program         (newProgram → dispatcher copy)
//   5. cel/composition.go                CompileCondition × 4
//                                          (validations / matchConditions / audit / message)
//
// The Validator returned holds four ConditionEvaluators + matcher; that is
// what gets cached in the policy source.
// ---------------------------------------------------------------------------
func testStep1CompilePolicy(t *testing.T) {
	t.Helper()
	policy := makePolicy()

	t.Logf("--- Step 1: compilePolicy ---")
	t.Logf("policy.Name=%s, ResourceVersion=%s", policy.Name, policy.ResourceVersion)
	t.Logf("Variables=%d  Validations=%d  MatchConditions=%d",
		len(policy.Spec.Variables), len(policy.Spec.Validations), len(policy.Spec.MatchConditions))

	// BREAKPOINT 1.A: step into compilePolicy. The first thing it does is
	// fetch the singleton env template (`getCompositionEnvTemplateWithStrictCost`)
	// and call `cel.NewCompositedCompiler(template)`.
	v := compilePolicy(policy)

	val, ok := v.(*validator)
	if !ok {
		t.Fatalf("compilePolicy returned %T, expected *validator", v)
	}

	// BREAKPOINT 1.B: inspect the *validator. Each non-nil filter is a
	// *cel.CompositedConditionEvaluator wrapping len(spec.X) compiled programs.
	t.Logf("validator.compileError=%v", val.compileError)
	t.Logf("validator.celMatcher set=%v  (should be true: we declared MatchConditions)", val.celMatcher != nil)
	t.Logf("validator.validationFilter set=%v", val.validationFilter != nil)
	t.Logf("validator.messageFilter set=%v", val.messageFilter != nil)
	t.Logf("validator.auditAnnotationFilter set=%v", val.auditAnnotationFilter != nil)

	if val.compileError != nil {
		t.Fatalf("unexpected compile error: %v", val.compileError)
	}
	if val.validationFilter == nil || val.celMatcher == nil {
		t.Fatalf("expected non-nil validation filter and celMatcher")
	}
}

// ---------------------------------------------------------------------------
// STEP 2 — generic.policySource builds the PolicyHook list and stores it.
//
// This is the control plane: informers fire → `notify()` flips dirty bit →
// `refreshPolicies()` runs `calculatePolicyData()` under the source's mutex →
// `policies.Store(&newHooks)` atomically swaps the new slice in.
//
// Files exercised (set breakpoints):
//
//   policy/generic/policy_source.go      Run                  (waits for sync, then refresh loop)
//   policy/generic/policy_source.go      refreshPolicies      (drains dirty bit, calls calculate)
//   policy/generic/policy_source.go      calculatePolicyData  (the work — under s.lock)
//   policy/generic/policy_source.go      compilePolicyLocked  (caches by ResourceVersion)
//
// We bypass `Run()` and call `calculatePolicyData()` directly via a
// reflective helper-free path: instead of running the goroutine loop, we
// drive `Source.HasSynced` then call `refreshPolicies()` indirectly by
// exposing the public path the same way the apiserver does — through a
// freshly-built source plus a tracker-driven informer pair.
// ---------------------------------------------------------------------------
func testStep2PolicySourceBuildAndStore(t *testing.T) {
	t.Helper()

	policy := makePolicy()
	binding := makeBinding()

	// ---- fake infra (mirrors generic/policy_test_context.go pattern) ----

	scheme := runtime.NewScheme()
	if err := fake.AddToScheme(scheme); err != nil {
		t.Fatalf("AddToScheme: %v", err)
	}
	policyGVR := schema.GroupVersionResource{Group: "admissionregistration.k8s.io", Version: "v1", Resource: "validatingadmissionpolicies"}
	bindingGVR := schema.GroupVersionResource{Group: "admissionregistration.k8s.io", Version: "v1", Resource: "validatingadmissionpolicybindings"}
	policyGVK := policyGVR.GroupVersion().WithKind("ValidatingAdmissionPolicy")
	bindingGVK := bindingGVR.GroupVersion().WithKind("ValidatingAdmissionPolicyBinding")
	scheme.AddKnownTypes(policyGVR.GroupVersion(),
		&v1.ValidatingAdmissionPolicy{}, &v1.ValidatingAdmissionPolicyList{},
		&v1.ValidatingAdmissionPolicyBinding{}, &v1.ValidatingAdmissionPolicyBindingList{})

	tracker := clienttesting.NewObjectTracker(scheme, serializer.NewCodecFactory(scheme).UniversalDecoder())

	// Seed the tracker BEFORE the informer starts — informers list on first
	// connection. Adding here is equivalent to the API server returning these
	// objects on initial list.
	if err := tracker.Add(policy); err != nil {
		t.Fatalf("tracker.Add(policy): %v", err)
	}
	if err := tracker.Add(binding); err != nil {
		t.Fatalf("tracker.Add(binding): %v", err)
	}

	policyInformer := cache.NewSharedIndexInformer(
		cache.ToListWatcherWithWatchListSemantics(&cache.ListWatch{
			ListFunc: func(opts metav1.ListOptions) (runtime.Object, error) {
				return tracker.List(policyGVR, policyGVK, "")
			},
			WatchFunc: func(opts metav1.ListOptions) (watch.Interface, error) {
				return tracker.Watch(policyGVR, "", opts)
			},
		}, tracker),
		&v1.ValidatingAdmissionPolicy{},
		0,
		cache.Indexers{cache.NamespaceIndex: cache.MetaNamespaceIndexFunc},
	)
	bindingInformer := cache.NewSharedIndexInformer(
		cache.ToListWatcherWithWatchListSemantics(&cache.ListWatch{
			ListFunc: func(opts metav1.ListOptions) (runtime.Object, error) {
				return tracker.List(bindingGVR, bindingGVK, "")
			},
			WatchFunc: func(opts metav1.ListOptions) (watch.Interface, error) {
				return tracker.Watch(bindingGVR, "", opts)
			},
		}, tracker),
		&v1.ValidatingAdmissionPolicyBinding{},
		0,
		cache.Indexers{cache.NamespaceIndex: cache.MetaNamespaceIndexFunc},
	)

	// ---- build the source ----
	// BREAKPOINT 2.A: step into NewPolicySource — it just stores the
	// closures and returns. No work happens here.
	nativeClient := fake.NewSimpleClientset()
	dynamicClient := dynamicfake.NewSimpleDynamicClient(scheme)
	informerFactory := informers.NewSharedInformerFactory(nativeClient, 30*time.Second)
	restMapper := meta.NewDefaultRESTMapper(nil)

	source := generic.NewPolicySource[*Policy, *PolicyBinding, Validator](
		policyInformer,
		bindingInformer,
		NewValidatingAdmissionPolicyAccessor,
		NewValidatingAdmissionPolicyBindingAccessor,
		compilePolicy, // <- our compiler closure from Step 1
		informerFactory,
		dynamic.Interface(dynamicClient),
		restMapper,
	)

	// ---- start informers, wait for cache sync, then drive Run() in a goroutine ----
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	go policyInformer.Run(ctx.Done())
	go bindingInformer.Run(ctx.Done())

	if !cache.WaitForCacheSync(ctx.Done(), policyInformer.HasSynced, bindingInformer.HasSynced) {
		t.Fatalf("informers failed to sync")
	}

	// We don't run source.Run() in a goroutine — that would start the 1Hz
	// refresh loop and event handlers. For deterministic stepping we drive
	// the path through the public Source interface instead.
	//
	// HasSynced returns false until the first refresh stores the policy
	// slice. Run() does that on first tick. We replicate that here by
	// running source.Run in a goroutine and polling HasSynced.
	runErr := make(chan error, 1)
	go func() { runErr <- source.Run(ctx) }()

	// BREAKPOINT 2.B: wait for the first Store(&hooks). When it returns,
	// the source has compiled all policies and atomically swapped the slice.
	if err := waitFor(ctx, 5*time.Second, source.HasSynced); err != nil {
		t.Fatalf("source did not sync: %v", err)
	}

	// BREAKPOINT 2.C: read the active policy hooks. This is exactly what
	// generic.Plugin.Dispatch does on every admission request.
	hooks := source.Hooks()
	t.Logf("--- Step 2: source.Hooks() ---")
	t.Logf("len(hooks)=%d  (should be 1)", len(hooks))
	if len(hooks) != 1 {
		t.Fatalf("expected 1 hook, got %d", len(hooks))
	}
	hook := hooks[0]
	t.Logf("hook.Policy.Name=%s", hook.Policy.Name)
	t.Logf("hook.Bindings count=%d", len(hook.Bindings))
	t.Logf("hook.ConfigurationError=%v", hook.ConfigurationError)

	val, ok := hook.Evaluator.(*validator)
	if !ok {
		t.Fatalf("hook.Evaluator is %T, expected *validator", hook.Evaluator)
	}
	t.Logf("hook.Evaluator is a *validator with compileError=%v", val.compileError)

	// Cancel and let Run() return cleanly.
	cancel()
	select {
	case <-runErr:
	case <-time.After(2 * time.Second):
		t.Logf("warning: source.Run did not exit within 2s of cancel")
	}
}

// ---------------------------------------------------------------------------
// STEP 3 — Drive validator.Validate on a synthetic admission request.
//
// This is the data plane. Files exercised:
//
//   validating/validator.go              validator.Validate
//   webhook/matchconditions/matcher.go   Match (matchConditions short-circuit)
//   cel/composition.go                   CompositedConditionEvaluator.ForInput
//   cel/condition.go                     condition.ForInput      (per-expression Eval)
//   cel/activation.go                    evaluationActivation.Evaluate
//   cel-go cel/program.go                prog.ContextEval
//
// We test both the admit path (good- prefix) and the deny path.
// ---------------------------------------------------------------------------
func testStep3ValidatorEvaluate(t *testing.T) {
	t.Helper()

	// Compile the policy once (fresh — this test does not depend on Step 2).
	v := compilePolicy(makePolicy())
	val := v.(*validator)
	if val.compileError != nil {
		t.Fatalf("unexpected compile error: %v", val.compileError)
	}

	cases := []struct {
		name           string
		objName        string
		expectAdmit    bool
		expectMessage  string
	}{
		{"admit_good_prefix", "good-configmap", true, ""},
		{"deny_bad_prefix", "bad-configmap", false, "name must start with good-"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			obj := &unstructured.Unstructured{Object: map[string]interface{}{
				"apiVersion": "v1",
				"kind":       "ConfigMap",
				"metadata":   map[string]interface{}{"name": tc.objName, "namespace": "default"},
				"data":       map[string]interface{}{"foo": "bar"},
			}}

			gvr := schema.GroupVersionResource{Group: "", Version: "v1", Resource: "configmaps"}
			gvk := schema.GroupVersionKind{Group: "", Version: "v1", Kind: "ConfigMap"}
			attr := admission.NewAttributesRecord(
				obj, nil,
				gvk,
				"default", tc.objName,
				gvr,
				"", admission.Create,
				&metav1.CreateOptions{}, false,
				nil,
			)
			versionedAttr, err := admission.NewVersionedAttributes(attr, gvk, nil)
			if err != nil {
				t.Fatalf("NewVersionedAttributes: %v", err)
			}

			// BREAKPOINT 3.A: step into validator.Validate. First it consults
			// celMatcher; if matchConditions return false we'd get an empty
			// ValidateResult here. With "good-configmap" / "bad-configmap"
			// the precondition (name != "") matches, so we proceed.
			res := val.Validate(
				context.Background(),
				gvr,
				versionedAttr,
				nil, // no params
				&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "default"}},
				celconfig.RuntimeCELCostBudget,
				nil, // no authorizer
			)

			t.Logf("--- Step 3: case=%s ---", tc.name)
			t.Logf("decisions=%d  audits=%d", len(res.Decisions), len(res.AuditAnnotations))
			for i, d := range res.Decisions {
				t.Logf("  [%d] action=%v eval=%v message=%q", i, d.Action, d.Evaluation, d.Message)
			}

			if tc.expectAdmit {
				for _, d := range res.Decisions {
					if d.Action != ActionAdmit {
						t.Errorf("expected admit, got action=%v message=%q", d.Action, d.Message)
					}
				}
			} else {
				if len(res.Decisions) == 0 {
					t.Fatalf("expected at least one decision")
				}
				d := res.Decisions[0]
				if d.Action != ActionDeny {
					t.Errorf("expected deny, got action=%v", d.Action)
				}
				if d.Message != tc.expectMessage {
					t.Errorf("expected message %q, got %q", tc.expectMessage, d.Message)
				}
			}
		})
	}
}

// ---------------------------------------------------------------------------
// helper
// ---------------------------------------------------------------------------

// waitFor polls cond until it returns true or ctx is cancelled.
func waitFor(ctx context.Context, timeout time.Duration, cond func() bool) error {
	deadline := time.Now().Add(timeout)
	tick := time.NewTicker(20 * time.Millisecond)
	defer tick.Stop()
	for {
		if cond() {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("waitFor: timed out after %v", timeout)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-tick.C:
		}
	}
}
