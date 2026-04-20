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
	"context"
	"sync"

	admissionv1 "k8s.io/api/admission/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apiserver/pkg/admission"
	"k8s.io/apiserver/pkg/admission/plugin/cel"
)

// lazyMessageEvaluator defers compilation of a policy's messageExpression
// programs until the first matching request evaluates validations. Every
// other CompileCondition call site stays eager — messageExpression output
// is only consumed on the validation-failure branch, so deferring it trades
// a one-time first-match compile for memory that would otherwise be retained
// by every policy whose requests never fail.
//
// sync.Once ensures exactly one build under concurrent first-match races.
type lazyMessageEvaluator struct {
	once  sync.Once
	inner cel.ConditionEvaluator
	build func() cel.ConditionEvaluator
}

var _ cel.ConditionEvaluator = &lazyMessageEvaluator{}

func newLazyMessageEvaluator(build func() cel.ConditionEvaluator) cel.ConditionEvaluator {
	return &lazyMessageEvaluator{build: build}
}

func (l *lazyMessageEvaluator) get() cel.ConditionEvaluator {
	l.once.Do(func() { l.inner = l.build() })
	return l.inner
}

func (l *lazyMessageEvaluator) ForInput(ctx context.Context, versionedAttr *admission.VersionedAttributes, request *admissionv1.AdmissionRequest, optionalVars cel.OptionalVariableBindings, namespace *corev1.Namespace, runtimeCELCostBudget int64) ([]cel.EvaluationResult, int64, error) {
	return l.get().ForInput(ctx, versionedAttr, request, optionalVars, namespace, runtimeCELCostBudget)
}

func (l *lazyMessageEvaluator) CompilationErrors() []error {
	return l.get().CompilationErrors()
}
