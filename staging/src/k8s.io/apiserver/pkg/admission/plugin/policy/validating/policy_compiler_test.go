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
	"reflect"
	"testing"

	"github.com/stretchr/testify/require"

	v1 "k8s.io/api/admissionregistration/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestPolicyCompilerCachesEquivalentPolicies(t *testing.T) {
	compiler := newPolicyCompiler()

	first := compilePolicyForTest(t, compiler, validatingPolicyForCacheTest("first", "must be ok"))
	second := compilePolicyForTest(t, compiler, validatingPolicyForCacheTest("second", "must be ok"))

	require.Len(t, compiler.entries, 1)
	require.Equal(t, interfacePointer(first.validationFilter), interfacePointer(second.validationFilter))
	require.Equal(t, interfacePointer(first.auditAnnotationFilter), interfacePointer(second.auditAnnotationFilter))
	require.Equal(t, interfacePointer(first.messageFilter), interfacePointer(second.messageFilter))

	// Match-condition metrics include the policy name, so each validator still gets
	// a per-policy matcher wrapped around the shared compiled condition filter.
	require.NotEqual(t, interfacePointer(first.celMatcher), interfacePointer(second.celMatcher))
}

func TestPolicyCompilerDoesNotSharePolicySpecificValidationMetadata(t *testing.T) {
	compiler := newPolicyCompiler()

	first := compilePolicyForTest(t, compiler, validatingPolicyForCacheTest("first", "first message"))
	second := compilePolicyForTest(t, compiler, validatingPolicyForCacheTest("second", "second message"))

	require.Len(t, compiler.entries, 2)
	require.NotEqual(t, interfacePointer(first.validationFilter), interfacePointer(second.validationFilter))
}

func TestPolicyCompilerReusesCompilationAcrossFailurePolicies(t *testing.T) {
	compiler := newPolicyCompiler()
	fail := v1.Fail
	ignore := v1.Ignore
	firstPolicy := validatingPolicyForCacheTest("first", "must be ok")
	firstPolicy.Spec.FailurePolicy = &fail
	secondPolicy := validatingPolicyForCacheTest("second", "must be ok")
	secondPolicy.Spec.FailurePolicy = &ignore

	first := compilePolicyForTest(t, compiler, firstPolicy)
	second := compilePolicyForTest(t, compiler, secondPolicy)

	require.Len(t, compiler.entries, 1)
	require.Equal(t, interfacePointer(first.validationFilter), interfacePointer(second.validationFilter))
	require.Equal(t, fail, *first.failPolicy)
	require.Equal(t, ignore, *second.failPolicy)
}

func compilePolicyForTest(t *testing.T, compiler *policyCompiler, policy *Policy) *validator {
	t.Helper()
	compiled, ok := compiler.compile(policy).(*validator)
	require.True(t, ok)
	return compiled
}

func interfacePointer(v interface{}) uintptr {
	return reflect.ValueOf(v).Pointer()
}

func validatingPolicyForCacheTest(name, message string) *Policy {
	reason := metav1.StatusReasonForbidden
	return &Policy{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: v1.ValidatingAdmissionPolicySpec{
			ParamKind: &v1.ParamKind{
				APIVersion: "policy.example.com/v1",
				Kind:       "ExampleParam",
			},
			Variables: []v1.Variable{{
				Name:       "objectName",
				Expression: "object.metadata.name",
			}},
			MatchConditions: []v1.MatchCondition{{
				Name:       "has-name",
				Expression: "object.metadata.name != ''",
			}},
			Validations: []v1.Validation{{
				Expression:        "variables.objectName == 'ok'",
				Message:           message,
				MessageExpression: "'denied ' + object.metadata.name",
				Reason:            &reason,
			}},
			AuditAnnotations: []v1.AuditAnnotation{{
				Key:             "decision",
				ValueExpression: "'checked'",
			}},
		},
	}
}
