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

package validating

import (
	"container/list"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"sync"

	v1 "k8s.io/api/admissionregistration/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apiserver/pkg/admission"
	"k8s.io/apiserver/pkg/admission/initializer"
	"k8s.io/apiserver/pkg/admission/plugin/cel"
	"k8s.io/apiserver/pkg/admission/plugin/policy/generic"
	"k8s.io/apiserver/pkg/admission/plugin/policy/matching"
	"k8s.io/apiserver/pkg/admission/plugin/webhook/matchconditions"
	"k8s.io/apiserver/pkg/authorization/authorizer"
	"k8s.io/apiserver/pkg/cel/environment"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/klog/v2"
)

const (
	// PluginName indicates the name of admission plug-in
	PluginName = "ValidatingAdmissionPolicy"

	// maxPolicyCompilationCacheEntries bounds the cache to prevent unbounded memory growth
	maxPolicyCompilationCacheEntries = 10000
)

var (
	lazyCompositionEnvTemplateWithStrictCostInit sync.Once
	lazyCompositionEnvTemplateWithStrictCost     *cel.CompositionEnv
)

// policyCompilationCacheKey represents the CEL-relevant fields for caching compiled policies
type policyCompilationCacheKey struct {
	HasParams bool `json:"hasParams"`
	Variables []struct {
		Name       string `json:"name"`
		Expression string `json:"expression"`
	} `json:"variables,omitempty"`
	MatchConditions []struct {
		Name       string `json:"name"`
		Expression string `json:"expression"`
	} `json:"matchConditions,omitempty"`
	Validations []struct {
		Expression        string `json:"expression"`
		Message           string `json:"message,omitempty"`
		MessageExpression string `json:"messageExpression,omitempty"`
		Reason            string `json:"reason,omitempty"`
	} `json:"validations,omitempty"`
	AuditAnnotations []struct {
		Key             string `json:"key"`
		ValueExpression string `json:"valueExpression"`
	} `json:"auditAnnotations,omitempty"`
}

// compiledPolicy contains the compiled CEL filters that can be reused across equivalent policies
type compiledPolicy struct {
	validationFilter      cel.ConditionEvaluator
	auditAnnotationFilter cel.ConditionEvaluator
	messageFilter         cel.ConditionEvaluator
}

// newValidator creates a fresh validator wrapper for each policy, preserving policy-specific behavior
func (cp *compiledPolicy) newValidator(policy *Policy) Validator {
	hasParam := policy.Spec.ParamKind != nil
	optionalVars := cel.OptionalVariableDeclarations{HasParams: hasParam, HasAuthorizer: true}

	var matcher matchconditions.Matcher = nil
	if len(policy.Spec.MatchConditions) > 0 {
		matchExpressionAccessors := make([]cel.ExpressionAccessor, len(policy.Spec.MatchConditions))
		for i := range policy.Spec.MatchConditions {
			matchExpressionAccessors[i] = (*matchconditions.MatchCondition)(&policy.Spec.MatchConditions[i])
		}
		compositionEnvTemplate := getCompositionEnvTemplateWithStrictCost()
		filterCompiler := cel.NewCompositedCompilerFromTemplate(compositionEnvTemplate)
		filterCompiler.CompileAndStoreVariables(convertv1beta1Variables(policy.Spec.Variables), optionalVars, environment.StoredExpressions)
		matchConditions := filterCompiler.CompileCondition(matchExpressionAccessors, optionalVars, environment.StoredExpressions)
		matcher = matchconditions.NewMatcher(matchConditions, policy.Spec.FailurePolicy, "policy", "validate", policy.Name)
	}

	return NewValidator(
		cp.validationFilter,
		matcher,
		cp.auditAnnotationFilter,
		cp.messageFilter,
		policy.Spec.FailurePolicy,
	)
}

// policyCompilationCacheEntry represents an entry in the LRU cache
type policyCompilationCacheEntry struct {
	key      string
	compiled *compiledPolicy
	element  *list.Element
}

// policyCompiler manages the bounded LRU cache for compiled policies
type policyCompiler struct {
	mu       sync.RWMutex
	cache    map[string]*policyCompilationCacheEntry
	lruList  *list.List
	maxItems int
}

// newPolicyCompiler creates a new bounded LRU cache for compiled policies
func newPolicyCompiler() *policyCompiler {
	return &policyCompiler{
		cache:    make(map[string]*policyCompilationCacheEntry),
		lruList:  list.New(),
		maxItems: maxPolicyCompilationCacheEntries,
	}
}

// compile returns a compiled policy, using the cache when possible
func (pc *policyCompiler) compile(policy *Policy) Validator {
	cacheKey := pc.computeCacheKey(policy)

	pc.mu.RLock()
	if entry, exists := pc.cache[cacheKey]; exists {
		// Cache hit - move to front and reuse compiled policy
		pc.lruList.MoveToFront(entry.element)
		compiled := entry.compiled
		pc.mu.RUnlock()
		return compiled.newValidator(policy)
	}
	pc.mu.RUnlock()

	// Cache miss - compile the policy
	compiled := pc.compilePolicyExpressions(policy)

	pc.mu.Lock()
	defer pc.mu.Unlock()

	// Check again in case another goroutine added it
	if entry, exists := pc.cache[cacheKey]; exists {
		pc.lruList.MoveToFront(entry.element)
		return entry.compiled.newValidator(policy)
	}

	// Add to cache with LRU eviction
	element := pc.lruList.PushFront(cacheKey)
	entry := &policyCompilationCacheEntry{
		key:      cacheKey,
		compiled: compiled,
		element:  element,
	}
	pc.cache[cacheKey] = entry

	// Evict oldest entries if cache is full
	for pc.lruList.Len() > pc.maxItems {
		oldest := pc.lruList.Back()
		if oldest != nil {
			oldKey := oldest.Value.(string)
			delete(pc.cache, oldKey)
			pc.lruList.Remove(oldest)
		}
	}

	return compiled.newValidator(policy)
}

// computeCacheKey generates a cache key based on CEL-relevant policy fields
func (pc *policyCompiler) computeCacheKey(policy *Policy) string {
	key := policyCompilationCacheKey{
		HasParams: policy.Spec.ParamKind != nil,
	}

	// Add variables
	if len(policy.Spec.Variables) > 0 {
		key.Variables = make([]struct {
			Name       string `json:"name"`
			Expression string `json:"expression"`
		}, len(policy.Spec.Variables))
		for i, v := range policy.Spec.Variables {
			key.Variables[i].Name = v.Name
			key.Variables[i].Expression = v.Expression
		}
	}

	// Add match conditions
	if len(policy.Spec.MatchConditions) > 0 {
		key.MatchConditions = make([]struct {
			Name       string `json:"name"`
			Expression string `json:"expression"`
		}, len(policy.Spec.MatchConditions))
		for i, mc := range policy.Spec.MatchConditions {
			key.MatchConditions[i].Name = mc.Name
			key.MatchConditions[i].Expression = mc.Expression
		}
	}

	// Add validations
	if len(policy.Spec.Validations) > 0 {
		key.Validations = make([]struct {
			Expression        string `json:"expression"`
			Message           string `json:"message,omitempty"`
			MessageExpression string `json:"messageExpression,omitempty"`
			Reason            string `json:"reason,omitempty"`
		}, len(policy.Spec.Validations))
		for i, val := range policy.Spec.Validations {
			key.Validations[i].Expression = val.Expression
			key.Validations[i].Message = val.Message
			key.Validations[i].MessageExpression = val.MessageExpression
			if val.Reason != nil {
				key.Validations[i].Reason = string(*val.Reason)
			}
		}
	}

	// Add audit annotations
	if len(policy.Spec.AuditAnnotations) > 0 {
		key.AuditAnnotations = make([]struct {
			Key             string `json:"key"`
			ValueExpression string `json:"valueExpression"`
		}, len(policy.Spec.AuditAnnotations))
		for i, aa := range policy.Spec.AuditAnnotations {
			key.AuditAnnotations[i].Key = aa.Key
			key.AuditAnnotations[i].ValueExpression = aa.ValueExpression
		}
	}

	// Serialize to JSON and hash for consistent cache key
	keyBytes, err := json.Marshal(key)
	if err != nil {
		// Fallback to a unique key if serialization fails
		return fmt.Sprintf("fallback-%p", policy)
	}

	hash := sha256.Sum256(keyBytes)
	return fmt.Sprintf("%x", hash)
}

// compilePolicyExpressions performs the actual CEL compilation
func (pc *policyCompiler) compilePolicyExpressions(policy *Policy) *compiledPolicy {
	klog.Infof("MD:PATCHED: compilePolicyExpressions")
	hasParam := policy.Spec.ParamKind != nil
	optionalVars := cel.OptionalVariableDeclarations{HasParams: hasParam, HasAuthorizer: true}
	expressionOptionalVars := cel.OptionalVariableDeclarations{HasParams: hasParam, HasAuthorizer: false}

	compositionEnvTemplate := getCompositionEnvTemplateWithStrictCost()
	filterCompiler := cel.NewCompositedCompilerFromTemplate(compositionEnvTemplate)
	filterCompiler.CompileAndStoreVariables(convertv1beta1Variables(policy.Spec.Variables), optionalVars, environment.StoredExpressions)

	return &compiledPolicy{
		validationFilter:      filterCompiler.CompileCondition(convertv1Validations(policy.Spec.Validations), optionalVars, environment.StoredExpressions),
		auditAnnotationFilter: filterCompiler.CompileCondition(convertv1AuditAnnotations(policy.Spec.AuditAnnotations), optionalVars, environment.StoredExpressions),
		messageFilter:         filterCompiler.CompileCondition(convertv1MessageExpressions(policy.Spec.Validations), expressionOptionalVars, environment.StoredExpressions),
	}
}

func getCompositionEnvTemplateWithStrictCost() *cel.CompositionEnv {
	lazyCompositionEnvTemplateWithStrictCostInit.Do(func() {
		env, err := cel.NewCompositionEnv(cel.VariablesTypeName, environment.MustBaseEnvSet(environment.DefaultCompatibilityVersion()))
		if err != nil {
			panic(err)
		}
		lazyCompositionEnvTemplateWithStrictCost = env
	})
	return lazyCompositionEnvTemplateWithStrictCost
}

// Register registers a plugin
func Register(plugins *admission.Plugins) {
	plugins.Register(PluginName, func(configFile io.Reader) (admission.Interface, error) {
		return NewPlugin(configFile), nil
	})
}

// Plugin is an implementation of admission.Interface.
type Policy = v1.ValidatingAdmissionPolicy
type PolicyBinding = v1.ValidatingAdmissionPolicyBinding
type PolicyEvaluator = Validator
type PolicyHook = generic.PolicyHook[*Policy, *PolicyBinding, PolicyEvaluator]

type Plugin struct {
	*generic.Plugin[PolicyHook]
	policyCompiler *policyCompiler
}

var _ admission.Interface = &Plugin{}
var _ admission.ValidationInterface = &Plugin{}
var _ initializer.WantsExcludedAdmissionResources = &Plugin{}

func NewPlugin(_ io.Reader) *Plugin {
	handler := admission.NewHandler(admission.Connect, admission.Create, admission.Delete, admission.Update)

	compiler := newPolicyCompiler()

	p := &Plugin{

		policyCompiler: compiler,
		Plugin: generic.NewPlugin(
			handler,
			func(f informers.SharedInformerFactory, client kubernetes.Interface, dynamicClient dynamic.Interface, restMapper meta.RESTMapper) generic.Source[PolicyHook] {
				return generic.NewPolicySource(
					f.Admissionregistration().V1().ValidatingAdmissionPolicies().Informer(),
					f.Admissionregistration().V1().ValidatingAdmissionPolicyBindings().Informer(),
					NewValidatingAdmissionPolicyAccessor,
					NewValidatingAdmissionPolicyBindingAccessor,
					compiler.compile,
					f,
					dynamicClient,
					restMapper,
				)
			},
			func(a authorizer.Authorizer, m *matching.Matcher, client kubernetes.Interface) generic.Dispatcher[PolicyHook] {
				return NewDispatcher(a, generic.NewPolicyMatcher(m))
			},
		),
	}
	p.SetEnabled(true)
	return p
}

// Validate makes an admission decision based on the request attributes.
func (a *Plugin) Validate(ctx context.Context, attr admission.Attributes, o admission.ObjectInterfaces) error {
	return a.Plugin.Dispatch(ctx, attr, o)
}

func convertv1Validations(inputValidations []v1.Validation) []cel.ExpressionAccessor {
	celExpressionAccessor := make([]cel.ExpressionAccessor, len(inputValidations))
	for i, validation := range inputValidations {
		validation := ValidationCondition{
			Expression: validation.Expression,
			Message:    validation.Message,
			Reason:     validation.Reason,
		}
		celExpressionAccessor[i] = &validation
	}
	return celExpressionAccessor
}

func convertv1MessageExpressions(inputValidations []v1.Validation) []cel.ExpressionAccessor {
	celExpressionAccessor := make([]cel.ExpressionAccessor, len(inputValidations))
	for i, validation := range inputValidations {
		if validation.MessageExpression != "" {
			condition := MessageExpressionCondition{
				MessageExpression: validation.MessageExpression,
			}
			celExpressionAccessor[i] = &condition
		}
	}
	return celExpressionAccessor
}

func convertv1AuditAnnotations(inputValidations []v1.AuditAnnotation) []cel.ExpressionAccessor {
	celExpressionAccessor := make([]cel.ExpressionAccessor, len(inputValidations))
	for i, validation := range inputValidations {
		validation := AuditAnnotationCondition{
			Key:             validation.Key,
			ValueExpression: validation.ValueExpression,
		}
		celExpressionAccessor[i] = &validation
	}
	return celExpressionAccessor
}

func convertv1beta1Variables(variables []v1.Variable) []cel.NamedExpressionAccessor {
	namedExpressions := make([]cel.NamedExpressionAccessor, len(variables))
	for i, variable := range variables {
		namedExpressions[i] = &Variable{Name: variable.Name, Expression: variable.Expression}
	}
	return namedExpressions
}
