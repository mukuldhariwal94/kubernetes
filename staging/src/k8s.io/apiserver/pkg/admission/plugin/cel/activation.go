/*
Copyright 2024 The Kubernetes Authors.

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
	"context"
	"fmt"
	"github.com/google/cel-go/interpreter"
	"math"
	"time"

	admissionv1 "k8s.io/api/admission/v1"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apiserver/pkg/admission"
	"k8s.io/apiserver/pkg/cel"
	"k8s.io/apiserver/pkg/cel/library"
	"k8s.io/klog/v2"
)

// newActivation creates an activation for CEL admission plugins from the given request, admission chain and
// variable binding information.
func newActivation(compositionCtx CompositionContext, versionedAttr *admission.VersionedAttributes, request *admissionv1.AdmissionRequest, inputs OptionalVariableBindings, namespace *v1.Namespace) (*evaluationActivation, error) {
	klog.Infof("CEL_POLICY_TRACE: [6] newActivation creating CEL variable bindings")
	klog.V(3).InfoS("Preparing variables for CEL activation", "hasParams", inputs.VersionedParams != nil, "hasAuthorizer", inputs.Authorizer != nil, "hasNamespace", namespace != nil)

	oldObjectVal, err := objectToResolveVal(versionedAttr.VersionedOldObject)
	if err != nil {
		klog.V(2).InfoS("Failed to prepare oldObject variable", "error", err)
		return nil, fmt.Errorf("failed to prepare oldObject variable for evaluation: %w", err)
	}
	klog.V(3).InfoS("Prepared oldObject variable")

	objectVal, err := objectToResolveVal(versionedAttr.VersionedObject)
	if err != nil {
		klog.V(2).InfoS("Failed to prepare object variable", "error", err)
		return nil, fmt.Errorf("failed to prepare object variable for evaluation: %w", err)
	}
	klog.V(3).InfoS("Prepared object variable")

	var paramsVal, authorizerVal, requestResourceAuthorizerVal any
	if inputs.VersionedParams != nil {
		paramsVal, err = objectToResolveVal(inputs.VersionedParams)
		if err != nil {
			klog.V(2).InfoS("Failed to prepare params variable", "error", err)
			return nil, fmt.Errorf("failed to prepare params variable for evaluation: %w", err)
		}
		klog.V(3).InfoS("Prepared params variable")
	}

	if inputs.Authorizer != nil {
		authorizerVal = library.NewAuthorizerVal(versionedAttr.GetUserInfo(), inputs.Authorizer)
		requestResourceAuthorizerVal = library.NewResourceAuthorizerVal(versionedAttr.GetUserInfo(), inputs.Authorizer, versionedAttr)
		klog.V(3).InfoS("Prepared authorizer variables")
	}

	requestVal, err := convertObjectToUnstructured(request)
	if err != nil {
		klog.V(2).InfoS("Failed to prepare request variable", "error", err)
		return nil, fmt.Errorf("failed to prepare request variable for evaluation: %w", err)
	}
	klog.V(3).InfoS("Prepared request variable")

	namespaceVal, err := objectToResolveVal(namespace)
	if err != nil {
		klog.V(2).InfoS("Failed to prepare namespace variable", "error", err)
		return nil, fmt.Errorf("failed to prepare namespace variable for evaluation: %w", err)
	}
	klog.V(3).InfoS("Prepared namespace variable")

	va := &evaluationActivation{
		object:                    objectVal,
		oldObject:                 oldObjectVal,
		params:                    paramsVal,
		request:                   requestVal.Object,
		namespace:                 namespaceVal,
		authorizer:                authorizerVal,
		requestResourceAuthorizer: requestResourceAuthorizerVal,
	}

	// composition is an optional feature that only applies for ValidatingAdmissionPolicy and MutatingAdmissionPolicy.
	if compositionCtx != nil {
		klog.V(3).InfoS("Applying composition context to activation")
		va.variables = compositionCtx.Variables(va)
	}
	klog.V(2).InfoS("Activation created successfully with all variables bound")
	return va, nil
}

type evaluationActivation struct {
	object, oldObject, params, request, namespace, authorizer, requestResourceAuthorizer, variables interface{}
}

// ResolveName returns a value from the activation by qualified name, or false if the name
// could not be found.
func (a *evaluationActivation) ResolveName(name string) (interface{}, bool) {
	switch name {
	case ObjectVarName:
		return a.object, true
	case OldObjectVarName:
		return a.oldObject, true
	case ParamsVarName:
		return a.params, true // params may be null
	case RequestVarName:
		return a.request, true
	case NamespaceVarName:
		return a.namespace, true
	case AuthorizerVarName:
		return a.authorizer, a.authorizer != nil
	case RequestResourceAuthorizerVarName:
		return a.requestResourceAuthorizer, a.requestResourceAuthorizer != nil
	case VariableVarName: // variables always present
		return a.variables, true
	default:
		return nil, false
	}
}

// Parent returns the parent of the current activation, may be nil.
// If non-nil, the parent will be searched during resolve calls.
func (a *evaluationActivation) Parent() interpreter.Activation {
	return nil
}

// Evaluate runs a compiled CEL admission plugin expression using the provided activation and CEL
// runtime cost budget.
func (a *evaluationActivation) Evaluate(ctx context.Context, compositionCtx CompositionContext, compilationResult CompilationResult, remainingBudget int64) (EvaluationResult, int64, error) {
	klog.Infof("CEL_POLICY_TRACE: [7] evaluationActivation.Evaluate executing CEL program")
	klog.V(2).InfoS("Starting CEL program evaluation", "remainingBudget", remainingBudget)

	var evaluation = EvaluationResult{}
	if compilationResult.ExpressionAccessor == nil { // in case of placeholder
		klog.V(3).InfoS("Skipping evaluation for placeholder expression")
		return evaluation, remainingBudget, nil
	}

	evaluation.ExpressionAccessor = compilationResult.ExpressionAccessor
	if compilationResult.Error != nil {
		klog.V(2).InfoS("Compilation error detected during evaluation", "expression", compilationResult.ExpressionAccessor.GetExpression(), "error", compilationResult.Error)
		evaluation.Error = &cel.Error{
			Type:   cel.ErrorTypeInvalid,
			Detail: fmt.Sprintf("compilation error: %v", compilationResult.Error),
			Cause:  compilationResult.Error,
		}
		return evaluation, remainingBudget, nil
	}
	if compilationResult.Program == nil {
		klog.V(2).InfoS("No compiled program found for expression")
		evaluation.Error = &cel.Error{
			Type:   cel.ErrorTypeInternal,
			Detail: "unexpected internal error compiling expression",
		}
		return evaluation, remainingBudget, nil
	}

	klog.V(3).InfoS("Executing compiled CEL program", "expression", compilationResult.ExpressionAccessor.GetExpression())
	t1 := time.Now()
	evalResult, evalDetails, err := compilationResult.Program.ContextEval(ctx, a)
	
	var cost int64 = -1
	if evalDetails != nil && evalDetails.ActualCost() != nil {
		cost = int64(*evalDetails.ActualCost())
	}
	klog.Infof("CEL_POLICY_TRACE: [8] CEL program evaluated. Cost: %v, Error: %v", cost, err)
	klog.V(2).InfoS("CEL program execution completed", "cost", cost, "hasError", err != nil)
	
	// budget may be spent due to lazy evaluation of composited variables
	if compositionCtx != nil {
		klog.V(3).InfoS("Checking composition context cost")
		compositionCost := compositionCtx.GetAndResetCost()
		if compositionCost > remainingBudget {
			klog.V(2).InfoS("Out of budget due to composition cost", "compositionCost", compositionCost, "remainingBudget", remainingBudget)
			return evaluation, -1, &cel.Error{
				Type:   cel.ErrorTypeInvalid,
				Detail: "validation failed due to running out of cost budget, no further validation rules will be run",
				Cause:  cel.ErrOutOfBudget,
			}
		}
		remainingBudget -= compositionCost
		klog.V(3).InfoS("Updated budget after composition", "newRemainingBudget", remainingBudget)
	}

	elapsed := time.Since(t1)
	evaluation.Elapsed = elapsed
	klog.V(3).InfoS("Expression evaluation elapsed time", "duration", elapsed)

	if evalDetails == nil {
		klog.V(2).InfoS("No evaluation details available")
		return evaluation, -1, &cel.Error{
			Type:   cel.ErrorTypeInternal,
			Detail: fmt.Sprintf("runtime cost could not be calculated for expression: %v, no further expression will be run", compilationResult.ExpressionAccessor.GetExpression()),
		}
	} else {
		rtCost := evalDetails.ActualCost()
		if rtCost == nil {
			klog.V(2).InfoS("Runtime cost could not be calculated")
			return evaluation, -1, &cel.Error{
				Type:   cel.ErrorTypeInvalid,
				Detail: fmt.Sprintf("runtime cost could not be calculated for expression: %v, no further expression will be run", compilationResult.ExpressionAccessor.GetExpression()),
				Cause:  cel.ErrOutOfBudget,
			}
		} else {
			if *rtCost > math.MaxInt64 || int64(*rtCost) > remainingBudget {
				klog.V(2).InfoS("Out of budget", "runtimeCost", *rtCost, "remainingBudget", remainingBudget, "maxInt64", math.MaxInt64)
				return evaluation, -1, &cel.Error{
					Type:   cel.ErrorTypeInvalid,
					Detail: "validation failed due to running out of cost budget, no further validation rules will be run",
					Cause:  cel.ErrOutOfBudget,
				}
			}
			remainingBudget -= int64(*rtCost)
			klog.V(3).InfoS("Updated budget after expression evaluation", "newRemainingBudget", remainingBudget)
		}
	}

	if err != nil {
		klog.V(2).InfoS("CEL program execution error", "expression", compilationResult.ExpressionAccessor.GetExpression(), "error", err)
		evaluation.Error = &cel.Error{
			Type:   cel.ErrorTypeInvalid,
			Detail: fmt.Sprintf("expression '%v' resulted in error: %v", compilationResult.ExpressionAccessor.GetExpression(), err),
		}
	} else {
		evaluation.EvalResult = evalResult
		klog.V(3).InfoS("CEL program execution successful", "result", evalResult)
	}
	
	klog.V(2).InfoS("CEL program evaluation finished", "elapsed", elapsed, "finalRemainingBudget", remainingBudget)
	return evaluation, remainingBudget, nil
}
